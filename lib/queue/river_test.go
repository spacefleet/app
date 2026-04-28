package queue

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// TestOpenValidates exercises the input checks in Open. We don't
// connect to a real Postgres for these — the DSN parser kicks them out
// before any network round-trip.
func TestOpenValidates(t *testing.T) {
	cases := []struct {
		name        string
		dsn         string
		concurrency int
		wantErr     string
	}{
		{"empty dsn", "", 4, "DATABASE_URL is empty"},
		{"zero concurrency", "postgres://x", 0, "concurrency must be > 0"},
		{"negative concurrency", "postgres://x", -1, "concurrency must be > 0"},
		{"too high concurrency", "postgres://x", MaxConcurrency + 1, "exceeds max"},
		{"unparseable dsn", "not a dsn", 4, "parse dsn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := Open(ctx, tc.dsn, tc.concurrency)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestNewClientValidates covers the Config-level checks. River has its
// own checks deeper in (e.g., requires Workers when Queues are set);
// this test pins our wrapper's contract.
func TestNewClientValidates(t *testing.T) {
	pool := &pgxpool.Pool{} // never used — validation runs first

	if _, err := NewClient(nil, Config{}); err == nil {
		t.Error("expected error for nil pool")
	}
	if _, err := NewClient(pool, Config{WorkerMode: true, Concurrency: 0}); err == nil {
		t.Error("expected error for WorkerMode without concurrency")
	}
	if _, err := NewClient(pool, Config{WorkerMode: true, Concurrency: MaxConcurrency + 1}); err == nil {
		t.Error("expected error for concurrency above max")
	}
	if _, err := NewClient(pool, Config{WorkerMode: true, Concurrency: 1, Workers: nil}); err == nil {
		t.Error("expected error for WorkerMode without Workers")
	}
}

// TestNewWorkers smoke-tests the wrapper. The registry is empty in v1;
// this is here so a future regression that breaks Inner() is caught.
func TestNewWorkers(t *testing.T) {
	w := NewWorkers()
	if w == nil || w.Inner() == nil {
		t.Fatal("NewWorkers returned a wrapper without an inner registry")
	}
}

// ---- Integration tests (need real Postgres) ---------------------------------

// integrationDSN reads TEST_DATABASE_URL and skips the test when unset.
// We don't try to spin up a container in-process; that adds opacity for
// limited gain. Local dev runs `make services-up` and exports the URL.
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return dsn
}

// TestMigrateIsIdempotent runs Migrate twice. The second run should
// apply zero migrations.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	cleanRiverSchema(t, pool)

	first, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("expected at least one migration on a clean DB")
	}

	second, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("expected no pending migrations on second run, got %d", len(second))
	}
}

// TestWorkerProcessesAJob runs end-to-end: migrate, register a probe
// worker, start the client, insert a job, wait for it to land in done.
// This is the real proof that the wiring works.
func TestWorkerProcessesAJob(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	cleanRiverSchema(t, pool)
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	workers := NewWorkers()
	done := make(chan struct{}, 1)
	probe := &probeWorker{done: done}
	river.AddWorker(workers.Inner(), probe)

	client, err := NewClient(pool, Config{WorkerMode: true, Concurrency: 2, Workers: workers})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Stop must run before the pool closes — the worker drain calls
	// resign-leadership and complete-job queries on the way out, both
	// of which need a live pool. t.Cleanup is LIFO, and pool.Close was
	// registered first, so this Stop runs first. Tests that wire it
	// the other way around get noisy "pool is closed" errors.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	if _, err := client.Insert(ctx, probeArgs{Echo: "hello"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	select {
	case <-done:
		// success
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not process the job within 10s")
	}
}

// TestInsertOnlyClientCannotStart asserts the contract that the API
// process can't accidentally turn itself into a worker by calling Start
// on a client built without WorkerMode.
func TestInsertOnlyClientCannotStart(t *testing.T) {
	dsn := integrationDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	client, err := NewClient(pool, Config{WorkerMode: false})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err == nil {
		t.Fatal("expected Start to fail on insert-only client")
	}
}

// cleanRiverSchema drops every River-owned object so a fresh Migrate
// run starts from zero. We do this so the tests are reproducible against
// the same dev Postgres without `make services-reset`.
func cleanRiverSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DROP TABLE IF EXISTS river_job CASCADE`,
		`DROP TABLE IF EXISTS river_leader CASCADE`,
		`DROP TABLE IF EXISTS river_queue CASCADE`,
		`DROP TABLE IF EXISTS river_client CASCADE`,
		`DROP TABLE IF EXISTS river_client_queue CASCADE`,
		`DROP TABLE IF EXISTS river_migration CASCADE`,
		`DROP TYPE  IF EXISTS river_job_state CASCADE`,
	}
	ctx := context.Background()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// CASCADE handles dependencies; failures here are usually
			// "doesn't exist" on a clean DB, which is fine.
			t.Logf("clean: %s -> %v", s, err)
		}
	}
}

// ---- Probe worker for the integration test ----------------------------------

type probeArgs struct {
	Echo string `json:"echo"`
}

func (probeArgs) Kind() string { return "spacefleet_test_probe" }

type probeWorker struct {
	river.WorkerDefaults[probeArgs]
	done chan<- struct{}
}

func (w *probeWorker) Work(_ context.Context, _ *river.Job[probeArgs]) error {
	select {
	case w.done <- struct{}{}:
	default:
	}
	return nil
}
