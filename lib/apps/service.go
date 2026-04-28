// Package apps owns the apps table: CRUD, slug minting, the
// destroy-with-or-without-resources delete contract, and validation
// against connected GitHub installations and AWS accounts.
//
// The service is the synchronous coordinator: it validates that
// references resolve, captures the default branch from GitHub at
// create time, and either drops the row directly or enqueues a
// destroy_app River job that tears down the per-app Pulumi stack
// before dropping the row.
package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/app"
	"github.com/spacefleet/app/ent/build"
	"github.com/spacefleet/app/ent/cloudaccount"
	"github.com/spacefleet/app/ent/githubinstallation"
)

// MaxSlugSearchSuffix bounds how many "-N" suffixes we'll try when
// resolving a slug collision. Picking 1000 means an org can register
// 1000 differently-named apps that all base-slug to the same value
// before we surrender. Past that, the user's naming has bigger
// problems than this loop can fix.
const MaxSlugSearchSuffix = 1000

// Sentinel errors. Callers map these to specific 4xx responses.
var (
	ErrAppNotFound          = errors.New("app not found")
	ErrInvalidName          = errors.New("name produces an empty slug; pick a name that contains letters or digits")
	ErrSlugReserved         = errors.New("that name maps to a reserved slug; pick another")
	ErrCloudAccountMissing  = errors.New("cloud account not found in this org")
	ErrCloudAccountNotReady = errors.New("cloud account is not connected — finish onboarding first")
	ErrInstallationMissing  = errors.New("github installation not found in this org")
	ErrSlugSpaceExhausted   = errors.New("could not find a free slug after trying many variants")
	ErrBuildsRunning        = errors.New("cannot delete an app while builds are running for it")
	ErrAlreadyDeleting      = errors.New("app is already being torn down")
	ErrDestroyNotConfigured = errors.New("destroy with resources requires the build pipeline to be configured on this server")
)

// RepoLookup resolves a GitHub installation + repo to its default
// branch. Implemented by *github.Service in production; tests inject a
// fake. Returning err covers both "installation not in org" and "repo
// not visible" cases.
type RepoLookup interface {
	DefaultBranch(ctx context.Context, orgSlug string, installationID int64, repoFullName string) (string, error)
}

// DestroyEnqueuer is the seam for kicking off a destroy_app River job.
// queue.Client implements it; tests fake it.
type DestroyEnqueuer interface {
	EnqueueDestroyApp(ctx context.Context, appID uuid.UUID) error
}

// Service is the persistence-aware coordinator. It owns the ent
// client and depends on RepoLookup for default-branch capture and
// DestroyEnqueuer for resource teardown. Either dependency may be nil
// — if RepoLookup is nil the create endpoint will surface a clear
// "not configured" error; if DestroyEnqueuer is nil the
// delete-with-destroy path returns a similar error.
type Service struct {
	ent     *ent.Client
	repos   RepoLookup
	destroy DestroyEnqueuer
}

func NewService(entClient *ent.Client, repos RepoLookup, destroy DestroyEnqueuer) *Service {
	return &Service{ent: entClient, repos: repos, destroy: destroy}
}

// CreateParams is what the API hands the service. CloudAccountID and
// GithubInstallationID are *our* ent UUIDs (not GitHub's int64
// installation_id and not AWS's 12-digit account id).
type CreateParams struct {
	OrgSlug              string
	Name                 string
	CloudAccountID       uuid.UUID
	GithubInstallationID uuid.UUID
	GithubRepoFullName   string
	CreatedBy            string
}

// Create validates every reference, captures the default branch from
// GitHub, resolves any slug collision, and inserts the row. Returns
// the saved entity on success.
//
// Validation ordering is intentional: cheap local checks first, then
// DB lookups for the cloud account and installation, then the GitHub
// round trip last. That keeps the failure path cheap when the user
// just typed an unusable name.
func (s *Service) Create(ctx context.Context, p CreateParams) (*ent.App, error) {
	if p.OrgSlug == "" {
		return nil, errors.New("org slug required")
	}
	if p.CreatedBy == "" {
		return nil, errors.New("created_by required")
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, ErrInvalidName
	}
	base := BaseSlug(name)
	if base == "" {
		return nil, ErrInvalidName
	}
	if IsReserved(base) {
		return nil, ErrSlugReserved
	}
	if p.GithubRepoFullName == "" || !strings.Contains(p.GithubRepoFullName, "/") {
		return nil, errors.New("github_repo_full_name must look like owner/repo")
	}

	// Cloud account: must be in this org and currently connected.
	// We deliberately reject pending/error accounts here rather than
	// allowing them and surfacing the failure on the first build —
	// the create page can show "this account isn't connected" right
	// next to the picker.
	ca, err := s.ent.CloudAccount.Query().
		Where(
			cloudaccount.IDEQ(p.CloudAccountID),
			cloudaccount.OrgSlugEQ(p.OrgSlug),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrCloudAccountMissing
		}
		return nil, err
	}
	if ca.Status != "connected" {
		return nil, ErrCloudAccountNotReady
	}

	// GitHub installation: must be in this org. Suspended installs
	// are still acceptable here — the user might be picking a repo
	// to register before unsuspending. Builds will fail loudly later
	// if it stays suspended.
	gi, err := s.ent.GithubInstallation.Query().
		Where(
			githubinstallation.IDEQ(p.GithubInstallationID),
			githubinstallation.OrgSlugEQ(p.OrgSlug),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrInstallationMissing
		}
		return nil, err
	}

	// Repo + default branch. We use the GitHub installation token to
	// validate the repo is actually visible to the installation in
	// the same call — a 404 here means either the repo doesn't exist
	// or this installation doesn't grant access.
	if s.repos == nil {
		return nil, errors.New("github not configured")
	}
	defaultBranch, err := s.repos.DefaultBranch(ctx, p.OrgSlug, gi.InstallationID, p.GithubRepoFullName)
	if err != nil {
		return nil, fmt.Errorf("resolve repo default branch: %w", err)
	}
	if defaultBranch == "" {
		// Empty defaults shouldn't happen in practice (every repo
		// has a default branch), but if we ever get one back we
		// pick "main" rather than persisting an empty string and
		// failing on the next build.
		defaultBranch = "main"
	}

	// Slug collision resolution. We try base, base-2, ... up to
	// MaxSlugSearchSuffix. Each candidate runs through the unique
	// (org_slug, slug) DB index — between the read-then-create
	// races we retry-on-conflict so we don't surface a flake when
	// two creates land at once.
	saved, err := s.insertWithSlug(ctx, base, &createInputs{
		params:        p,
		name:          name,
		defaultBranch: defaultBranch,
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

type createInputs struct {
	params        CreateParams
	name          string
	defaultBranch string
}

func (s *Service) insertWithSlug(ctx context.Context, base string, in *createInputs) (*ent.App, error) {
	for n := 1; n <= MaxSlugSearchSuffix; n++ {
		slug := SlugWithSuffix(base, n)
		if IsReserved(slug) {
			// A truncated suffix could land on a reserved word in
			// theory; skip past it.
			continue
		}
		row, err := s.ent.App.Create().
			SetOrgSlug(in.params.OrgSlug).
			SetName(in.name).
			SetSlug(slug).
			SetCloudAccountID(in.params.CloudAccountID).
			SetGithubInstallationID(in.params.GithubInstallationID).
			SetGithubRepoFullName(in.params.GithubRepoFullName).
			SetDefaultBranch(in.defaultBranch).
			SetCreatedBy(in.params.CreatedBy).
			Save(ctx)
		if err == nil {
			return row, nil
		}
		if !ent.IsConstraintError(err) {
			return nil, err
		}
		// Slug collision; try the next candidate.
	}
	return nil, ErrSlugSpaceExhausted
}

// List returns every app in the org, newest first. Paginated later.
func (s *Service) List(ctx context.Context, orgSlug string) ([]*ent.App, error) {
	return s.ent.App.Query().
		Where(app.OrgSlugEQ(orgSlug)).
		Order(ent.Desc(app.FieldCreatedAt)).
		All(ctx)
}

// Get looks up one app by (org, slug). Scoped so a guess at another
// org's UUID can't escape the tenant.
func (s *Service) Get(ctx context.Context, orgSlug, slug string) (*ent.App, error) {
	row, err := s.ent.App.Query().
		Where(app.OrgSlugEQ(orgSlug), app.SlugEQ(slug)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	return row, nil
}

// DeleteParams names the per-delete options. DestroyResources=true is
// the "scorched earth" path that runs `pulumi destroy` against the
// per-app stack; false leaves the customer's AWS resources alone and
// just drops our row.
type DeleteParams struct {
	OrgSlug          string
	Slug             string
	DestroyResources bool
}

// DeleteResult tells the caller what kind of operation actually ran
// — used by the API layer to choose 200 vs 202.
type DeleteResult struct {
	// Enqueued is true when DestroyResources was requested and a
	// destroy_app job was enqueued; the caller responds 202. The
	// row is *not* deleted yet — the worker drops it after
	// pulumi destroy succeeds.
	Enqueued bool
}

// Delete drops an app and (optionally) tears down its AWS resources.
// Always rejects when builds are still running for the app; that's a
// visible state the user can resolve, and tearing down the ECR repo
// while a build is pushing to it is a particularly nasty footgun.
func (s *Service) Delete(ctx context.Context, p DeleteParams) (*DeleteResult, error) {
	row, err := s.Get(ctx, p.OrgSlug, p.Slug)
	if err != nil {
		return nil, err
	}
	if row.DeletingAt != nil {
		return nil, ErrAlreadyDeleting
	}

	running, err := s.hasRunningBuilds(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	if running {
		return nil, ErrBuildsRunning
	}

	if !p.DestroyResources {
		// Cascading FKs on builds → apps mean we don't have to
		// clean up the builds table by hand.
		if err := s.ent.App.DeleteOneID(row.ID).Exec(ctx); err != nil {
			return nil, err
		}
		return &DeleteResult{Enqueued: false}, nil
	}

	if s.destroy == nil {
		return nil, ErrDestroyNotConfigured
	}

	now := timeNow()
	if _, err := s.ent.App.UpdateOneID(row.ID).SetDeletingAt(now).Save(ctx); err != nil {
		return nil, err
	}
	if err := s.destroy.EnqueueDestroyApp(ctx, row.ID); err != nil {
		// Best-effort rollback of the deleting_at marker so the
		// user can retry without an "already deleting" error.
		_, _ = s.ent.App.UpdateOneID(row.ID).ClearDeletingAt().Save(ctx)
		return nil, fmt.Errorf("enqueue destroy: %w", err)
	}
	return &DeleteResult{Enqueued: true}, nil
}

// hasRunningBuilds reports whether any builds for this app are in
// status=running. Used as a guard for delete; the per-app build
// concurrency rule in lib/builds runs an equivalent query at
// promotion time.
func (s *Service) hasRunningBuilds(ctx context.Context, appID uuid.UUID) (bool, error) {
	count, err := s.ent.Build.Query().
		Where(
			build.AppIDEQ(appID),
			build.StatusEQ("running"),
		).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
