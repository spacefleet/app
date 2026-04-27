package builds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/github"
)

// integrationDSN mirrors lib/apps' helper. Tests that need a real
// Postgres skip when TEST_DATABASE_URL isn't set.
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return dsn
}

var schemaCounter uint64

// testClientPair holds the parallel ent.Client + *sql.DB the worker /
// poller / webhook need. We attach the sql handle to a per-client map
// so callers can fetch it back via rawDBFromClient.
var testClientDBs = map[*ent.Client]*sql.DB{}
var testClientDBsMu sync.Mutex

func newTestClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := integrationDSN(t)
	idx := atomic.AddUint64(&schemaCounter, 1)
	schemaName := fmt.Sprintf("builds_test_%d_%d", time.Now().UnixNano(), idx)

	rootDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := rootDB.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = rootDB.Close()
		t.Fatalf("create schema: %v", err)
	}
	_ = rootDB.Close()

	scoped := withSearchPath(dsn, schemaName)
	db, err := sql.Open("pgx", scoped)
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
	testClientDBsMu.Lock()
	testClientDBs[client] = db
	testClientDBsMu.Unlock()
	t.Cleanup(func() {
		testClientDBsMu.Lock()
		delete(testClientDBs, client)
		testClientDBsMu.Unlock()
		_ = client.Close()
		_ = db.Close()
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS "` + schemaName + `" CASCADE`)
	})
	return client
}

// rawDBFromClient retrieves the *sql.DB created alongside an ent.Client
// in newTestClient. Tests that exercise raw-SQL paths (AppendStage, the
// promotion advisory lock, the poller's reads) need it.
func rawDBFromClient(t *testing.T, client *ent.Client) *sql.DB {
	t.Helper()
	testClientDBsMu.Lock()
	defer testClientDBsMu.Unlock()
	db := testClientDBs[client]
	if db == nil {
		t.Fatal("no *sql.DB recorded for this ent.Client (was it created via newTestClient?)")
	}
	return db
}

func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}

// fakeResolver is a CommitResolver test double. Tests pin sha or wire
// up an error.
type fakeResolver struct {
	sha        string
	err        error
	gotOrg     string
	gotInstall int64
	gotRepo    string
	gotRef     string
	calls      int
}

func (f *fakeResolver) ResolveCommit(_ context.Context, orgSlug string, installationID int64, repoFullName, ref string) (string, error) {
	f.calls++
	f.gotOrg, f.gotInstall, f.gotRepo, f.gotRef = orgSlug, installationID, repoFullName, ref
	if f.err != nil {
		return "", f.err
	}
	return f.sha, nil
}

// fakeQueue is a JobEnqueuer test double. Captures every enqueue.
type fakeQueue struct {
	enqueued []uuid.UUID
	err      error
}

func (f *fakeQueue) EnqueueBuild(_ context.Context, buildID uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, buildID)
	return nil
}

type appFixture struct {
	app       *ent.App
	installID int64
	cloudID   uuid.UUID
}

func newAppFixture(t *testing.T, client *ent.Client) *appFixture {
	t.Helper()
	ctx := context.Background()
	ca, err := client.CloudAccount.Create().
		SetOrgSlug("acme").
		SetProvider("aws").
		SetLabel("acme-prod").
		SetExternalID("FAKE").
		SetStatus("connected").
		Save(ctx)
	if err != nil {
		t.Fatalf("cloud account: %v", err)
	}
	gi, err := client.GithubInstallation.Create().
		SetOrgSlug("acme").
		SetInstallationID(98765).
		SetAccountLogin("acme").
		SetAccountType("Organization").
		SetAccountID(11).
		Save(ctx)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	app, err := client.App.Create().
		SetOrgSlug("acme").
		SetName("App").
		SetSlug("app").
		SetCloudAccountID(ca.ID).
		SetGithubInstallationID(gi.ID).
		SetGithubRepoFullName("acme/repo").
		SetDefaultBranch("main").
		SetCreatedBy("user_1").
		Save(ctx)
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	return &appFixture{app: app, installID: gi.InstallationID, cloudID: ca.ID}
}

func TestCreate_HappyPath(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	res := &fakeResolver{sha: "abc1234567890abcdef1234567890abcdef12345"}
	q := &fakeQueue{}
	svc := NewService(client, res, q)

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		Ref:       "feature/x",
		CreatedBy: "user_1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.SourceSha != res.sha {
		t.Errorf("source_sha = %q", row.SourceSha)
	}
	if row.SourceRef != "feature/x" {
		t.Errorf("source_ref = %q", row.SourceRef)
	}
	if row.Status != "queued" {
		t.Errorf("status = %q", row.Status)
	}
	if row.WebhookSecret == "" || len(row.WebhookSecret) != WebhookSecretBytes*2 {
		t.Errorf("webhook secret length wrong: %d", len(row.WebhookSecret))
	}
	if res.gotInstall != fix.installID {
		t.Errorf("resolver got install = %d, want %d", res.gotInstall, fix.installID)
	}
	if len(q.enqueued) != 1 || q.enqueued[0] != row.ID {
		t.Errorf("enqueue calls = %v, want %v", q.enqueued, row.ID)
	}
}

func TestCreate_DefaultsRefToAppDefaultBranch(t *testing.T) {
	client := newTestClient(t)
	newAppFixture(t, client)
	res := &fakeResolver{sha: "abcabcabcabcabcabcabcabcabcabcabcabcabca"}
	svc := NewService(client, res, &fakeQueue{})

	row, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		CreatedBy: "user_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.SourceRef != "main" {
		t.Errorf("source_ref = %q, want main", row.SourceRef)
	}
	if res.gotRef != "main" {
		t.Errorf("resolver ref = %q", res.gotRef)
	}
}

func TestCreate_RefNotResolvable(t *testing.T) {
	client := newTestClient(t)
	newAppFixture(t, client)
	res := &fakeResolver{err: github.ErrRefNotFound}
	svc := NewService(client, res, &fakeQueue{})

	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		Ref:       "nope",
		CreatedBy: "user_1",
	})
	if !errors.Is(err, ErrRefNotResolvable) {
		t.Errorf("err = %v, want ErrRefNotResolvable", err)
	}
}

func TestCreate_RepoNotAccessible(t *testing.T) {
	client := newTestClient(t)
	newAppFixture(t, client)
	res := &fakeResolver{err: github.ErrRepoNotFound}
	svc := NewService(client, res, &fakeQueue{})

	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		Ref:       "main",
		CreatedBy: "user_1",
	})
	if !errors.Is(err, ErrRefNotResolvable) {
		t.Errorf("err = %v, want ErrRefNotResolvable", err)
	}
}

func TestCreate_AppDeletingRefuses(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	if _, err := client.App.UpdateOneID(fix.app.ID).SetDeletingAt(time.Now()).Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	svc := NewService(client, &fakeResolver{sha: "x"}, &fakeQueue{})
	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		CreatedBy: "user_1",
	})
	if !errors.Is(err, ErrAppDeleting) {
		t.Errorf("err = %v, want ErrAppDeleting", err)
	}
}

func TestCreate_AppNotFound(t *testing.T) {
	client := newTestClient(t)
	svc := NewService(client, &fakeResolver{sha: "x"}, &fakeQueue{})

	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "missing",
		CreatedBy: "user_1",
	})
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

func TestCreate_ResolverNotConfigured(t *testing.T) {
	client := newTestClient(t)
	newAppFixture(t, client)
	svc := NewService(client, nil, &fakeQueue{})
	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		CreatedBy: "user_1",
	})
	if !errors.Is(err, ErrGitHubNotConfigured) {
		t.Errorf("err = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestCreate_EnqueueFailureRollsBack(t *testing.T) {
	client := newTestClient(t)
	newAppFixture(t, client)
	res := &fakeResolver{sha: "abc1234567890abcdef1234567890abcdef12345"}
	q := &fakeQueue{err: errors.New("queue down")}

	svc := NewService(client, res, q)
	_, err := svc.Create(context.Background(), CreateParams{
		OrgSlug:   "acme",
		AppSlug:   "app",
		CreatedBy: "user_1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// The row should have been deleted as best-effort cleanup.
	count, err := client.Build.Query().Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 builds after enqueue failure, got %d", count)
	}
}

func TestList_NewestFirst(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)

	mk := func(ref string) {
		t.Helper()
		_, err := client.Build.Create().
			SetAppID(fix.app.ID).
			SetSourceRef(ref).
			SetSourceSha(strings.Repeat("a", 40)).
			SetWebhookSecret("s").
			SetCreatedBy("user_1").
			Save(context.Background())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Force a tick so created_at differs.
		time.Sleep(2 * time.Millisecond)
	}
	mk("first")
	mk("second")
	mk("third")

	svc := NewService(client, &fakeResolver{}, &fakeQueue{})
	rows, err := svc.List(context.Background(), "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d", len(rows))
	}
	if rows[0].SourceRef != "third" {
		t.Errorf("first = %q, want third", rows[0].SourceRef)
	}
}

func TestGet_OrgScoped(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("user_1").
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(client, &fakeResolver{}, &fakeQueue{})
	got, err := svc.Get(context.Background(), "acme", "app", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != row.ID {
		t.Errorf("id mismatch")
	}
	// Wrong org -> ErrAppNotFound (org is checked first).
	_, err = svc.Get(context.Background(), "other", "app", row.ID)
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("cross-org err = %v, want ErrAppNotFound", err)
	}
	// Wrong build id -> ErrBuildNotFound.
	_, err = svc.Get(context.Background(), "acme", "app", uuid.New())
	if !errors.Is(err, ErrBuildNotFound) {
		t.Errorf("err = %v, want ErrBuildNotFound", err)
	}
}
