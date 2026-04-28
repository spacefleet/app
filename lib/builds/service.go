// Package builds owns the builds table and the orchestration that turns
// a "build this app" request into a running Fargate task and, eventually,
// an image in the customer's ECR.
//
// service.go is the synchronous half: validate the request, resolve the
// ref to a SHA, mint a per-build webhook secret, insert the row, and
// hand the build to River for the worker. worker.go is the asynchronous
// half (reconcile → prepare → dispatch + DescribeTasks polling backstop).
// webhook.go is the public, HMAC-authenticated endpoint that builders
// call to report stage transitions.
package builds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	entapp "github.com/spacefleet/app/ent/app"
	"github.com/spacefleet/app/ent/build"
	"github.com/spacefleet/app/lib/github"
)

// WebhookSecretBytes is the size of the random per-build HMAC key. 32
// bytes is a comfortable margin over the SHA-256 block size and matches
// what BUILD_PIPELINE.md calls out.
const WebhookSecretBytes = 32

// Sentinel errors. Callers (the API layer) map these to specific 4xx
// responses; anything else surfaces as 5xx.
var (
	ErrAppNotFound         = errors.New("app not found")
	ErrAppDeleting         = errors.New("app is being torn down; can't start new builds")
	ErrRefNotResolvable    = errors.New("ref does not resolve to a commit in the repo")
	ErrBuildNotFound       = errors.New("build not found")
	ErrGitHubNotConfigured = errors.New("github is not configured on this server")
)

// CommitResolver looks up the full SHA for a (org, installation, repo,
// ref) tuple. *github.Service implements this. Defining the interface
// here (rather than importing github directly) lets tests fake it
// without spinning up the real GitHub plumbing.
type CommitResolver interface {
	ResolveCommit(ctx context.Context, orgSlug string, installationID int64, repoFullName string, ref string) (string, error)
}

// JobEnqueuer is what the service hands a freshly-inserted build to
// River. *queue.Client implements this; tests fake it. Defining the
// interface here decouples the service from queue's import graph.
type JobEnqueuer interface {
	EnqueueBuild(ctx context.Context, buildID uuid.UUID) error
}

// Service is the sync side of the build pipeline. Owns the ent client,
// holds references to the resolver + enqueuer, exposes a small public
// surface (Create / Get / List).
type Service struct {
	ent      *ent.Client
	resolver CommitResolver
	queue    JobEnqueuer
}

// NewService constructs the service. resolver and queue may be nil:
//   - nil resolver -> Create returns ErrGitHubNotConfigured.
//   - nil queue -> Create returns a clear "queue not configured" error.
//
// We keep the API service usable in route-level tests that don't wire
// either dep.
func NewService(entClient *ent.Client, resolver CommitResolver, queue JobEnqueuer) *Service {
	return &Service{ent: entClient, resolver: resolver, queue: queue}
}

// CreateParams is the input shape. AppID is *our* row UUID; the resolver
// looks up the GitHub installation by its row, not by URL.
type CreateParams struct {
	OrgSlug   string
	AppSlug   string
	Ref       string // empty -> use app.default_branch
	CreatedBy string
}

// Create runs the synchronous part of "build this app":
//  1. Look up the app, refuse if it's being torn down.
//  2. Default ref to app.default_branch when blank.
//  3. Resolve ref -> 40-char commit SHA via the GitHub installation.
//  4. Mint a 32-byte webhook secret.
//  5. Insert the builds row (status=queued).
//  6. Enqueue the River BuildJob.
//
// Returns the saved row. The plaintext webhook secret stays on the row
// — ent's Sensitive() field omits it from logs but the worker reads it
// when minting environment for the Fargate task.
//
// We deliberately don't wrap insert + enqueue in a transaction. River
// uses pgx; ent uses database/sql. Sharing a transaction across drivers
// is more pain than it's worth here, and the failure mode is benign:
// if enqueue fails after insert, the build sits in status=queued and
// the next worker startup either picks it up via reattach (if status
// got promoted to running) or stays orphaned (we add a periodic
// "queued > N min" sweeper later if it ever happens in practice).
func (s *Service) Create(ctx context.Context, p CreateParams) (*ent.Build, error) {
	if p.OrgSlug == "" {
		return nil, errors.New("org slug required")
	}
	if p.AppSlug == "" {
		return nil, errors.New("app slug required")
	}
	if p.CreatedBy == "" {
		return nil, errors.New("created_by required")
	}
	if s.resolver == nil {
		return nil, ErrGitHubNotConfigured
	}
	if s.queue == nil {
		return nil, errors.New("build queue not configured")
	}

	app, err := s.lookupApp(ctx, p.OrgSlug, p.AppSlug)
	if err != nil {
		return nil, err
	}
	if app.DeletingAt != nil {
		return nil, ErrAppDeleting
	}

	ref := p.Ref
	if ref == "" {
		ref = app.DefaultBranch
	}

	// The resolver wants GitHub's int64 installation_id, not our row
	// UUID. The app schema stores the FK as a column, not an edge, so
	// we look the row up directly.
	installation, err := s.ent.GithubInstallation.Get(ctx, app.GithubInstallationID)
	if err != nil {
		return nil, fmt.Errorf("look up installation: %w", err)
	}
	sha, err := s.resolver.ResolveCommit(ctx, p.OrgSlug, installation.InstallationID, app.GithubRepoFullName, ref)
	if err != nil {
		if errors.Is(err, github.ErrRefNotFound) || errors.Is(err, github.ErrRepoNotFound) {
			return nil, ErrRefNotResolvable
		}
		return nil, fmt.Errorf("resolve ref: %w", err)
	}

	secret, err := newWebhookSecret()
	if err != nil {
		return nil, err
	}

	row, err := s.ent.Build.Create().
		SetAppID(app.ID).
		SetSourceRef(ref).
		SetSourceSha(sha).
		SetStatus("queued").
		SetWebhookSecret(secret).
		SetCreatedBy(p.CreatedBy).
		Save(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.queue.EnqueueBuild(ctx, row.ID); err != nil {
		// Best-effort cleanup: the row is useless without a worker, and
		// leaving status=queued forever pollutes the UI. We don't try
		// to be clever — a delete failure here just means a tombstone
		// the user can ignore.
		_ = s.ent.Build.DeleteOneID(row.ID).Exec(ctx)
		return nil, fmt.Errorf("enqueue build: %w", err)
	}
	return row, nil
}

// Get returns one build by id, scoped to (org, app) so a guess at
// another app's UUID can't escape the tenant boundary.
func (s *Service) Get(ctx context.Context, orgSlug, appSlug string, buildID uuid.UUID) (*ent.Build, error) {
	app, err := s.lookupApp(ctx, orgSlug, appSlug)
	if err != nil {
		return nil, err
	}
	row, err := s.ent.Build.Query().
		Where(
			build.IDEQ(buildID),
			build.AppIDEQ(app.ID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrBuildNotFound
		}
		return nil, err
	}
	return row, nil
}

// List returns every build for an app, newest first. Pagination is a
// future concern — the dominant access pattern shows the most recent
// handful and we can land cursor-based paging when that hurts.
func (s *Service) List(ctx context.Context, orgSlug, appSlug string) ([]*ent.Build, error) {
	app, err := s.lookupApp(ctx, orgSlug, appSlug)
	if err != nil {
		return nil, err
	}
	return s.ent.Build.Query().
		Where(build.AppIDEQ(app.ID)).
		Order(ent.Desc(build.FieldCreatedAt)).
		All(ctx)
}

// lookupApp finds the app by (org, slug) — the same scope check the
// apps service uses, duplicated here so we don't need to inject the
// apps service.
func (s *Service) lookupApp(ctx context.Context, orgSlug, appSlug string) (*ent.App, error) {
	row, err := s.ent.App.Query().
		Where(
			entapp.OrgSlugEQ(orgSlug),
			entapp.SlugEQ(appSlug),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAppNotFound
		}
		return nil, err
	}
	return row, nil
}

// newWebhookSecret returns a fresh hex-encoded random secret. We hex
// instead of base64 so the value is shell-safe — the builder script
// passes it as an env var to openssl and a `+` or `/` mid-string isn't
// a problem in practice but hex avoids the question entirely.
func newWebhookSecret() (string, error) {
	buf := make([]byte, WebhookSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
