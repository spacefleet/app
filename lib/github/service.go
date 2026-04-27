package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/githubinstallation"
	"github.com/spacefleet/app/ent/githubinstallstate"
)

// stateTTL bounds how long an install handshake can take. Five minutes is
// enough for a normal click-through, short enough that a leaked URL is
// usually dead on arrival.
const stateTTL = 5 * time.Minute

// Sentinel errors. Callers map these to 4xx; everything else is 5xx.
var (
	ErrInvalidState      = errors.New("invalid install state")
	ErrStateExpired      = errors.New("install state expired")
	ErrStateConsumed     = errors.New("install state already used")
	ErrStateUserMismatch = errors.New("install state belongs to a different user")
	ErrInstallNotFound   = errors.New("installation not found")
)

// Service is the persistence-aware wrapper around App. Routes call Service;
// Service calls App for the GitHub side and ent for ours.
type Service struct {
	ent *ent.Client
	app *App
}

func NewService(entClient *ent.Client, app *App) *Service {
	return &Service{ent: entClient, app: app}
}

// App returns the underlying App. Lets routes build the install URL
// without re-implementing slug handling.
func (s *Service) App() *App { return s.app }

// CreateInstallState mints a random state, persists its sha256, and returns
// the plaintext for inclusion in the install URL. The state binds the
// pending install to (orgSlug, userID); both must match at completion.
func (s *Service) CreateInstallState(ctx context.Context, orgSlug, userID string) (string, error) {
	plaintext, hash, err := newState()
	if err != nil {
		return "", err
	}
	_, err = s.ent.GithubInstallState.Create().
		SetStateHash(hash).
		SetOrgSlug(orgSlug).
		SetUserID(userID).
		SetExpiresAt(time.Now().Add(stateTTL)).
		Save(ctx)
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// CompleteInstall finalizes the handshake: validate the state, fetch the
// installation from GitHub, persist (or refresh) the row, and return the
// stored entity. userID is the *current* session's user; we reject if it
// doesn't match the one that initiated the install.
func (s *Service) CompleteInstall(ctx context.Context, state, userID string, installationID int64) (*ent.GithubInstallation, error) {
	tx, err := s.ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func() { _ = tx.Rollback() }

	hash := sha256Sum(state)
	rec, err := tx.GithubInstallState.Query().Where(githubinstallstate.StateHashEQ(hash)).Only(ctx)
	if err != nil {
		rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInvalidState
		}
		return nil, err
	}
	now := time.Now()
	if rec.ConsumedAt != nil {
		rollback()
		return nil, ErrStateConsumed
	}
	if !rec.ExpiresAt.After(now) {
		rollback()
		return nil, ErrStateExpired
	}
	if rec.UserID != userID {
		rollback()
		return nil, ErrStateUserMismatch
	}

	if _, err := tx.GithubInstallState.UpdateOneID(rec.ID).SetConsumedAt(now).Save(ctx); err != nil {
		rollback()
		return nil, err
	}

	// Fetch installation metadata using App-level auth. We do this inside
	// the transaction so a GitHub failure rolls back the state-consume —
	// the user can retry.
	ghInstall, err := s.app.GetInstallation(ctx, installationID)
	if err != nil {
		rollback()
		return nil, err
	}

	// Upsert: an org reinstalling an app on the same GitHub account hits
	// the same installation_id, so re-bind the row to the (potentially
	// different) org slug rather than failing on the unique constraint.
	existing, err := tx.GithubInstallation.Query().
		Where(githubinstallation.InstallationIDEQ(ghInstall.ID)).
		Only(ctx)
	var saved *ent.GithubInstallation
	switch {
	case err == nil:
		upd := tx.GithubInstallation.UpdateOneID(existing.ID).
			SetOrgSlug(rec.OrgSlug).
			SetAccountLogin(ghInstall.Account.Login).
			SetAccountType(ghInstall.Account.Type).
			SetAccountID(ghInstall.Account.ID).
			SetUpdatedAt(now)
		if ghInstall.Suspended {
			upd = upd.SetSuspendedAt(now)
		} else {
			upd = upd.ClearSuspendedAt()
		}
		saved, err = upd.Save(ctx)
	case ent.IsNotFound(err):
		create := tx.GithubInstallation.Create().
			SetOrgSlug(rec.OrgSlug).
			SetInstallationID(ghInstall.ID).
			SetAccountLogin(ghInstall.Account.Login).
			SetAccountType(ghInstall.Account.Type).
			SetAccountID(ghInstall.Account.ID)
		if ghInstall.Suspended {
			create = create.SetSuspendedAt(now)
		}
		saved, err = create.Save(ctx)
	}
	if err != nil {
		rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return saved, nil
}

// ListInstallations returns every installation an org has connected, newest
// first. Hides installations from other orgs even if the caller asks by id.
func (s *Service) ListInstallations(ctx context.Context, orgSlug string) ([]*ent.GithubInstallation, error) {
	return s.ent.GithubInstallation.Query().
		Where(githubinstallation.OrgSlugEQ(orgSlug)).
		Order(ent.Desc(githubinstallation.FieldCreatedAt)).
		All(ctx)
}

// GetInstallation looks up an installation by GitHub's installation_id,
// scoped to orgSlug. Used by routes that receive the id from a URL.
func (s *Service) GetInstallation(ctx context.Context, orgSlug string, installationID int64) (*ent.GithubInstallation, error) {
	row, err := s.ent.GithubInstallation.Query().
		Where(
			githubinstallation.OrgSlugEQ(orgSlug),
			githubinstallation.InstallationIDEQ(installationID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrInstallNotFound
		}
		return nil, err
	}
	return row, nil
}

// DeleteInstallation removes the App from the customer's GitHub account
// *and* drops our local row. GitHub-first ordering matters: if we deleted
// locally first and the GitHub call failed, the App would be orphaned on
// their side with no UI to clean it up (Connect would just show the
// existing install on github.com and dump them back here, confused).
// 404 from GitHub is treated as success — same end state.
func (s *Service) DeleteInstallation(ctx context.Context, orgSlug string, installationID int64) error {
	row, err := s.GetInstallation(ctx, orgSlug, installationID)
	if err != nil {
		return err
	}
	if err := s.app.DeleteInstallation(ctx, installationID); err != nil {
		return err
	}
	return s.ent.GithubInstallation.DeleteOneID(row.ID).Exec(ctx)
}

// ListRepositories proves the auth chain end-to-end: org → installation →
// access token → repo list. Mints a fresh installation token per call;
// per-call latency is fine for v1 (~50ms), and the alternative — caching
// tokens — adds revocation complexity we don't need yet.
func (s *Service) ListRepositories(ctx context.Context, orgSlug string, installationID int64) ([]Repository, error) {
	if _, err := s.GetInstallation(ctx, orgSlug, installationID); err != nil {
		return nil, err
	}
	return s.app.ListInstallationRepositories(ctx, installationID)
}

// IssueInstallationToken returns a short-lived access token for the named
// installation, after verifying it belongs to orgSlug. This is the seam
// the build pipeline will plug into when it needs to clone source.
func (s *Service) IssueInstallationToken(ctx context.Context, orgSlug string, installationID int64) (*AccessToken, error) {
	if _, err := s.GetInstallation(ctx, orgSlug, installationID); err != nil {
		return nil, err
	}
	return s.app.InstallationToken(ctx, installationID)
}

// GetInstallationRepository proves the org → installation → repo chain
// in one call: confirms the installation belongs to the org, then asks
// GitHub for the repo using an installation-scoped token (so 404 also
// covers "repo isn't in this installation"). Returns ErrInstallNotFound
// or ErrRepoNotFound for the two distinct user errors so callers can
// pick clearer 4xx responses.
//
// Note that installationID here is GitHub's int64 — not our row UUID.
// Callers that have only the UUID should look up the installation
// row first and use its InstallationID field.
func (s *Service) GetInstallationRepository(ctx context.Context, orgSlug string, installationID int64, fullName string) (*Repository, error) {
	if _, err := s.GetInstallation(ctx, orgSlug, installationID); err != nil {
		return nil, err
	}
	return s.app.GetInstallationRepository(ctx, installationID, fullName)
}

// ResolveCommit looks up a ref (branch/tag/SHA) inside an installation's
// repo and returns the full commit SHA. Verifies the installation
// belongs to orgSlug first; ErrRefNotFound covers both "ref doesn't
// exist" and "installation can't see the repo" because GitHub returns
// the same 404 for both — same end state from the caller's POV.
func (s *Service) ResolveCommit(ctx context.Context, orgSlug string, installationID int64, fullName, ref string) (string, error) {
	if _, err := s.GetInstallation(ctx, orgSlug, installationID); err != nil {
		return "", err
	}
	return s.app.ResolveCommit(ctx, installationID, fullName, ref)
}

// DefaultBranch satisfies lib/apps.RepoLookup. The apps service hands
// us GitHub's int64 installation_id (not our UUID — it's already
// looked up the row by the time it gets here), and we surface only
// the default-branch string. Errors propagate verbatim so callers
// can tell ErrInstallNotFound from ErrRepoNotFound.
func (s *Service) DefaultBranch(ctx context.Context, orgSlug string, installationID int64, repoFullName string) (string, error) {
	repo, err := s.app.GetInstallationRepository(ctx, installationID, repoFullName)
	if err != nil {
		return "", err
	}
	return repo.DefaultBranch, nil
}

func newState() (plaintext string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", nil, err
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(plaintext))
	return plaintext, h[:], nil
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
