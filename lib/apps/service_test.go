package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/spacefleet/app/ent"
)

// openRawDB opens a database/sql connection via the pgx driver. The
// scope arg is just for diagnostics — it doesn't change the
// connection itself; search_path is wired through the DSN.
func openRawDB(dsn, _ string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// integrationDSN reads TEST_DATABASE_URL and skips the test when
// unset. Mirrors lib/queue's pattern — tests that need real Postgres
// don't try to spin up containers in-process.
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return dsn
}

// schemaCounter ensures each test gets its own Postgres schema, so
// parallel runs and crash residue from previous runs don't bleed
// across tests. Tables get created via ent.Schema.Create on first
// use; the per-schema namespace means no cross-test interference.
var schemaCounter uint64

// newTestClient opens an ent.Client in a freshly-minted Postgres
// schema. The schema is dropped on test cleanup so a sequence of
// runs doesn't accumulate junk. ent.Schema.Create is used instead of
// the SQL migrations in db/migrations/ because those carry FK
// constraints that depend on table-creation order; ent's
// auto-migrate handles the dependency graph itself, and the FKs
// produced are equivalent for testing purposes.
func newTestClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := integrationDSN(t)

	idx := atomic.AddUint64(&schemaCounter, 1)
	schemaName := fmt.Sprintf("apps_test_%d_%d", time.Now().UnixNano(), idx)

	rootDB, err := openRawDB(dsn, "")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := rootDB.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = rootDB.Close()
		t.Fatalf("create schema: %v", err)
	}
	_ = rootDB.Close()

	scopedDSN := withSearchPath(dsn, schemaName)
	db, err := openRawDB(scopedDSN, schemaName)
	if err != nil {
		t.Fatalf("open scoped: %v", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))

	if err := client.Schema.Create(context.Background()); err != nil {
		_ = client.Close()
		_ = db.Close()
		t.Fatalf("schema create: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = db.Close()
		// Re-open a connection without the search_path scope so
		// DROP SCHEMA can run.
		cleanup, err := openRawDB(dsn, "")
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS "` + schemaName + `" CASCADE`)
	})
	return client
}

// fakeRepoLookup is the test stub for RepoLookup. Callers can pin
// the default branch returned for a given repo or wire up an error.
type fakeRepoLookup struct {
	branch string
	err    error

	gotOrgSlug      string
	gotInstallation int64
	gotRepoFullName string
	calls           int
}

func (f *fakeRepoLookup) DefaultBranch(_ context.Context, orgSlug string, installationID int64, repoFullName string) (string, error) {
	f.calls++
	f.gotOrgSlug = orgSlug
	f.gotInstallation = installationID
	f.gotRepoFullName = repoFullName
	if f.err != nil {
		return "", f.err
	}
	return f.branch, nil
}

// fakeDestroy records EnqueueDestroyApp calls. The phase-5 worker
// will replace this with a River-backed implementation.
type fakeDestroy struct {
	calls   []uuid.UUID
	failure error
}

func (f *fakeDestroy) EnqueueDestroyApp(_ context.Context, appID uuid.UUID) error {
	if f.failure != nil {
		return f.failure
	}
	f.calls = append(f.calls, appID)
	return nil
}

// fixture is a minimal, valid (org, cloud account, installation)
// triple used as the starting point for most service tests. Returns
// the saved rows so tests can reach for their IDs.
type fixture struct {
	org          string
	cloudID      uuid.UUID
	installID    uuid.UUID
	githubAppID  int64
	repoFullName string
}

func setupFixture(t *testing.T, client *ent.Client, opts ...func(*fixture)) *fixture {
	t.Helper()
	f := &fixture{
		org:          "acme",
		githubAppID:  12345,
		repoFullName: "acme/app",
	}
	for _, o := range opts {
		o(f)
	}

	ca, err := client.CloudAccount.Create().
		SetOrgSlug(f.org).
		SetProvider("aws").
		SetLabel("acme-prod").
		SetExternalID("FAKE-EXTERNAL-ID").
		SetStatus("connected").
		Save(context.Background())
	if err != nil {
		t.Fatalf("create cloud account: %v", err)
	}
	f.cloudID = ca.ID

	gi, err := client.GithubInstallation.Create().
		SetOrgSlug(f.org).
		SetInstallationID(f.githubAppID).
		SetAccountLogin("acme").
		SetAccountType("Organization").
		SetAccountID(987654).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create installation: %v", err)
	}
	f.installID = gi.ID

	return f
}

func TestCreate_HappyPath(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	repos := &fakeRepoLookup{branch: "trunk"}
	svc := NewService(client, repos, &fakeDestroy{})

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "My Cool App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user_123",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Slug != "my-cool-app" {
		t.Errorf("slug = %q, want my-cool-app", row.Slug)
	}
	if row.DefaultBranch != "trunk" {
		t.Errorf("default_branch = %q, want trunk", row.DefaultBranch)
	}
	if row.Name != "My Cool App" {
		t.Errorf("name = %q", row.Name)
	}
	if row.OrgSlug != fix.org {
		t.Errorf("org_slug = %q", row.OrgSlug)
	}
	if row.CreatedBy != "user_123" {
		t.Errorf("created_by = %q", row.CreatedBy)
	}
	if repos.gotInstallation != fix.githubAppID {
		t.Errorf("github int64 id passed to lookup = %d, want %d", repos.gotInstallation, fix.githubAppID)
	}
}

func TestCreate_ResolvesSlugCollisions(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	repos := &fakeRepoLookup{branch: "main"}
	svc := NewService(client, repos, nil)

	mk := func(name string) string {
		t.Helper()
		row, err := svc.Create(context.Background(), CreateParams{
			OrgSlug:              fix.org,
			Name:                 name,
			CloudAccountID:       fix.cloudID,
			GithubInstallationID: fix.installID,
			GithubRepoFullName:   fix.repoFullName,
			CreatedBy:            "user",
		})
		if err != nil {
			t.Fatalf("Create %q: %v", name, err)
		}
		return row.Slug
	}

	if got := mk("Stuff"); got != "stuff" {
		t.Errorf("first = %q, want stuff", got)
	}
	if got := mk("Stuff"); got != "stuff-2" {
		t.Errorf("second = %q, want stuff-2", got)
	}
	if got := mk("STUFF!"); got != "stuff-3" {
		t.Errorf("third = %q, want stuff-3", got)
	}
}

func TestCreate_RejectsReservedSlugs(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	repos := &fakeRepoLookup{branch: "main"}
	svc := NewService(client, repos, nil)

	for _, name := range []string{"new", "Settings", "API"} {
		_, err := svc.Create(context.Background(), CreateParams{
			OrgSlug:              fix.org,
			Name:                 name,
			CloudAccountID:       fix.cloudID,
			GithubInstallationID: fix.installID,
			GithubRepoFullName:   fix.repoFullName,
			CreatedBy:            "user",
		})
		if !errors.Is(err, ErrSlugReserved) {
			t.Errorf("Create(%q) error = %v, want ErrSlugReserved", name, err)
		}
	}
}

func TestCreate_RejectsEmptyName(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)

	for _, name := range []string{"", "   ", "...", "🚀"} {
		_, err := svc.Create(context.Background(), CreateParams{
			OrgSlug:              fix.org,
			Name:                 name,
			CloudAccountID:       fix.cloudID,
			GithubInstallationID: fix.installID,
			GithubRepoFullName:   fix.repoFullName,
			CreatedBy:            "user",
		})
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestCreate_CloudAccountMustBeConnected(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)

	// Flip the cloud account back to pending.
	if _, err := client.CloudAccount.UpdateOneID(fix.cloudID).SetStatus("pending").Save(context.Background()); err != nil {
		t.Fatalf("flip cloud account: %v", err)
	}

	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)
	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if !errors.Is(err, ErrCloudAccountNotReady) {
		t.Errorf("error = %v, want ErrCloudAccountNotReady", err)
	}
}

func TestCreate_CloudAccountMustBeInOrg(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)

	// Make a cloud account belonging to a different org.
	otherCA, err := client.CloudAccount.Create().
		SetOrgSlug("other").
		SetProvider("aws").
		SetLabel("other-prod").
		SetExternalID("OTHER-EXT").
		SetStatus("connected").
		Save(context.Background())
	if err != nil {
		t.Fatalf("create other CA: %v", err)
	}

	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)
	_, err = svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       otherCA.ID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if !errors.Is(err, ErrCloudAccountMissing) {
		t.Errorf("error = %v, want ErrCloudAccountMissing", err)
	}
}

func TestCreate_InstallationMustBeInOrg(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)

	other, err := client.GithubInstallation.Create().
		SetOrgSlug("other").
		SetInstallationID(99999).
		SetAccountLogin("other").
		SetAccountType("User").
		SetAccountID(1).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create other install: %v", err)
	}

	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)
	_, err = svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: other.ID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if !errors.Is(err, ErrInstallationMissing) {
		t.Errorf("error = %v, want ErrInstallationMissing", err)
	}
}

func TestCreate_PropagatesRepoLookupError(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	wantErr := errors.New("github says no")
	svc := NewService(client, &fakeRepoLookup{err: wantErr}, nil)

	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wraps %v", err, wantErr)
	}
}

func TestCreate_RejectsBadRepoFormat(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)

	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   "not-a-slash-form",
		CreatedBy:            "user",
	})
	if err == nil {
		t.Fatal("expected error for bad repo format")
	}
}

func TestList_NewestFirstAndScopedToOrg(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)

	for _, n := range []string{"alpha", "beta", "gamma"} {
		if _, err := svc.Create(context.Background(), CreateParams{
			OrgSlug:              fix.org,
			Name:                 n,
			CloudAccountID:       fix.cloudID,
			GithubInstallationID: fix.installID,
			GithubRepoFullName:   fix.repoFullName,
			CreatedBy:            "user",
		}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
		// Force ordering by giving each row a distinct created_at.
		time.Sleep(2 * time.Millisecond)
	}

	rows, err := svc.List(context.Background(), fix.org)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
	if rows[0].Name != "gamma" || rows[2].Name != "alpha" {
		t.Errorf("ordering wrong: %s ... %s", rows[0].Name, rows[2].Name)
	}
}

func TestGet_ScopedToOrg(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, nil)

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.Get(context.Background(), fix.org, row.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != row.ID {
		t.Errorf("got id = %v, want %v", got.ID, row.ID)
	}

	if _, err := svc.Get(context.Background(), "other-org", row.Slug); !errors.Is(err, ErrAppNotFound) {
		t.Errorf("cross-org get: err = %v, want ErrAppNotFound", err)
	}
}

func TestDelete_WithoutResources(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	dest := &fakeDestroy{}
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, dest)

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug:          fix.org,
		Slug:             row.Slug,
		DestroyResources: false,
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Enqueued {
		t.Error("expected Enqueued=false for non-destroy delete")
	}
	if len(dest.calls) != 0 {
		t.Errorf("destroy enqueue called %d times, want 0", len(dest.calls))
	}
	if _, err := svc.Get(context.Background(), fix.org, row.Slug); !errors.Is(err, ErrAppNotFound) {
		t.Errorf("after delete: %v, want ErrAppNotFound", err)
	}
}

func TestDelete_WithResourcesEnqueuesAndMarks(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	dest := &fakeDestroy{}
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, dest)

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug:          fix.org,
		Slug:             row.Slug,
		DestroyResources: true,
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !res.Enqueued {
		t.Error("expected Enqueued=true")
	}
	if len(dest.calls) != 1 || dest.calls[0] != row.ID {
		t.Errorf("destroy calls = %v, want [%v]", dest.calls, row.ID)
	}

	// Row still exists with deleting_at set; the worker drops it.
	got, err := svc.Get(context.Background(), fix.org, row.Slug)
	if err != nil {
		t.Fatalf("Get after destroy enqueue: %v", err)
	}
	if got.DeletingAt == nil {
		t.Error("expected deleting_at to be set")
	}

	// Second call: 'already deleting'.
	if _, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug:          fix.org,
		Slug:             row.Slug,
		DestroyResources: true,
	}); !errors.Is(err, ErrAlreadyDeleting) {
		t.Errorf("second delete: err = %v, want ErrAlreadyDeleting", err)
	}
}

func TestDelete_RollsBackDeletingAtOnEnqueueFailure(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	dest := &fakeDestroy{failure: errors.New("queue exploded")}
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, dest)

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug:          fix.org,
		Slug:             row.Slug,
		DestroyResources: true,
	}); err == nil {
		t.Fatal("expected error when enqueue fails")
	}

	got, err := svc.Get(context.Background(), fix.org, row.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeletingAt != nil {
		t.Error("expected deleting_at to be cleared after enqueue failure")
	}
}

func TestDelete_BlocksOnRunningBuild(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, &fakeDestroy{})

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:              fix.org,
		Name:                 "App",
		CloudAccountID:       fix.cloudID,
		GithubInstallationID: fix.installID,
		GithubRepoFullName:   fix.repoFullName,
		CreatedBy:            "user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Create a running build directly via ent.
	if _, err := client.Build.Create().
		SetAppID(row.ID).
		SetSourceRef("main").
		SetStatus("running").
		SetWebhookSecret("supersecret").
		SetCreatedBy("user").
		Save(context.Background()); err != nil {
		t.Fatalf("create build: %v", err)
	}

	if _, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug:          fix.org,
		Slug:             row.Slug,
		DestroyResources: false,
	}); !errors.Is(err, ErrBuildsRunning) {
		t.Errorf("delete with running build: err = %v, want ErrBuildsRunning", err)
	}
}

func TestDelete_NotFoundIsNotFound(t *testing.T) {
	client := newTestClient(t)
	fix := setupFixture(t, client)
	svc := NewService(client, &fakeRepoLookup{branch: "main"}, &fakeDestroy{})
	_, err := svc.Delete(context.Background(), DeleteParams{
		OrgSlug: fix.org,
		Slug:    "does-not-exist",
	})
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

// withSearchPath rewrites the dsn to set a search_path, so tables go
// into the per-test schema. Works for both URL-style DSNs (with a
// `?` prefix) and key=value DSNs.
func withSearchPath(dsn, schema string) string {
	if schema == "" {
		return dsn
	}
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}
