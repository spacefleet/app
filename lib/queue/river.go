// Package queue is the Spacefleet wrapper around riverqueue's Postgres
// job queue. The interesting wrinkles vs. a stock River setup:
//
//   - We need to share the database with ent (which uses pgx via
//     database/sql). River's Go driver only ships a pgxv5 implementation
//     today, so this package keeps a separate *pgxpool.Pool — both pools
//     point at the same Postgres but River's pool is sized just for River
//     so a busy worker can't starve the HTTP API and vice versa.
//
//   - River brings its own SQL migrations. We expose [Migrate] so the
//     `worker` subcommand can run them on startup and the operator never
//     has to remember a separate command. Migrations are applied in the
//     same Postgres database as our atlas-style migrations; River uses
//     its own bookkeeping table (`river_migration`).
//
//   - All Spacefleet jobs are registered through [Register] so callers
//     never reach for the raw `river.AddWorker` generic — keeps the panic
//     surface contained to one file in the codebase.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// QueueDefault is the queue name used for build/destroy jobs in v1. We
// keep one queue today; a future deploy queue or per-tenant queue can
// land here without touching the public surface.
const QueueDefault = river.QueueDefault

// MaxConcurrency clamps the value passed to River's queue config. River
// will accept any positive int, but a runaway value (e.g. a typo in
// SPACEFLEET_WORKER_CONCURRENCY) would let the worker open more
// connections than the pool has. 64 is plenty for v1 — way more than
// the 4-default we ship.
const MaxConcurrency = 64

// Workers wraps river.Workers so callers don't have to learn the River
// type system on day one. Workers are added through the package's
// Register* helpers (RegisterBuildWorker, RegisterDestroyAppWorker)
// rather than river.AddWorker so the panic surface stays in one file.
type Workers struct {
	r *river.Workers
}

// NewWorkers builds a fresh registry. Each `worker` process makes one.
func NewWorkers() *Workers {
	return &Workers{r: river.NewWorkers()}
}

// Inner returns the underlying *river.Workers. Reach for this only when
// you need to call River's generic helpers (AddWorker[T]) — keep all
// such calls in one file per package so the panic surface stays small.
func (w *Workers) Inner() *river.Workers {
	return w.r
}

// Open opens a pgx pool sized for the worker process. Callers own the
// pool and must Close it on shutdown.
//
// Sizing logic: River reserves up to (concurrency + 1) connections at
// any given moment for fetches, notifies, and inserts on top of the
// per-job connection. Worst case, a job that itself opens connections
// can deadlock against the pool — keep some headroom.
func Open(ctx context.Context, dsn string, concurrency int) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("queue: DATABASE_URL is empty")
	}
	if concurrency <= 0 {
		return nil, fmt.Errorf("queue: concurrency must be > 0, got %d", concurrency)
	}
	if concurrency > MaxConcurrency {
		return nil, fmt.Errorf("queue: concurrency %d exceeds max %d", concurrency, MaxConcurrency)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("queue: parse dsn: %w", err)
	}
	// concurrency for jobs + headroom (1 listener, 1 inserter, plus a
	// little slack for the maintenance services River runs).
	cfg.MaxConns = int32(concurrency + 4)
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("queue: pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("queue: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies any pending River migrations against the given pool.
// Idempotent — re-running with no pending versions is a no-op that
// returns an empty MigrateResult.
//
// We always migrate at startup of the worker process; the cost is one
// transaction that resolves in milliseconds, and it removes the failure
// mode where a fresh DB starts the worker before someone runs a
// migration command.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]MigrationApplied, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, fmt.Errorf("queue: rivermigrate: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return nil, fmt.Errorf("queue: migrate: %w", err)
	}
	out := make([]MigrationApplied, 0, len(res.Versions))
	for _, v := range res.Versions {
		out = append(out, MigrationApplied{Version: v.Version, Name: v.Name, Duration: v.Duration})
	}
	return out, nil
}

// MigrationApplied is what Migrate returns to callers. We trim River's
// internal type to just the fields we display — no SQL bodies, no
// direction (up is the only direction we ever run from production
// code).
type MigrationApplied struct {
	Version  int
	Name     string
	Duration time.Duration
}

// Config is what NewClient takes. It mirrors the few river.Config knobs
// we expose; everything else stays at River's defaults until we have a
// reason to change it.
type Config struct {
	// Concurrency is the maximum number of jobs to work in parallel on
	// the default queue. Required when WorkerMode is true.
	Concurrency int

	// WorkerMode controls whether the resulting client fetches and
	// works jobs. When false, the client is insert-only — used by the
	// HTTP API to enqueue work without competing for it.
	WorkerMode bool

	// Workers is the registry the client should run against. Only
	// consulted when WorkerMode is true. Insert-only clients can leave
	// this nil; the HTTP API doesn't need to know the worker types.
	Workers *Workers

	// Logger is the structured logger River uses for its own output
	// (job lifecycle events, leadership changes, etc.). Optional —
	// River writes to stdout at warn level when nil.
	Logger *slog.Logger
}

// NewClient builds a *river.Client. The result is generic over the
// transaction type; we always use pgx.Tx via riverpgxv5, so callers
// receive `*river.Client[pgx.Tx]` (the Tx parameter is `pgx.Tx` from the
// driver).
func NewClient(pool *pgxpool.Pool, cfg Config) (*Client, error) {
	if pool == nil {
		return nil, errors.New("queue: pool is nil")
	}
	if cfg.WorkerMode && cfg.Concurrency <= 0 {
		return nil, fmt.Errorf("queue: WorkerMode requires Concurrency > 0, got %d", cfg.Concurrency)
	}
	if cfg.WorkerMode && cfg.Concurrency > MaxConcurrency {
		return nil, fmt.Errorf("queue: concurrency %d exceeds max %d", cfg.Concurrency, MaxConcurrency)
	}

	rcfg := &river.Config{Logger: cfg.Logger}
	if cfg.WorkerMode {
		if cfg.Workers == nil {
			return nil, errors.New("queue: WorkerMode requires Workers")
		}
		rcfg.Queues = map[string]river.QueueConfig{
			QueueDefault: {MaxWorkers: cfg.Concurrency},
		}
		rcfg.Workers = cfg.Workers.Inner()
	}

	client, err := river.NewClient(riverpgxv5.New(pool), rcfg)
	if err != nil {
		return nil, fmt.Errorf("queue: river client: %w", err)
	}
	return &Client{inner: client, mode: cfg.WorkerMode}, nil
}
