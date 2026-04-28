package builds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent/schema"
)

// ErrBuildTerminal is returned by AppendStage when the build row exists
// but its status is succeeded or failed. Callers handling late events
// (the webhook, primarily) should treat this as a no-op.
var ErrBuildTerminal = errors.New("build is terminal")

// Stage names. These match what the entrypoint script and the
// orchestrator both emit; documenting them here keeps the contract
// visible from one file.
const (
	StageReconcile = "reconcile"
	StagePrepare   = "prepare"
	StageDispatch  = "dispatch"
	StageClone     = "clone"
	StageBuild     = "build"
	StagePush      = "push"
)

// Stage statuses.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// AppendStage atomically appends one stage event to builds.stages.
//
// Postgres' jsonb || operator runs as a single atomic statement, so
// concurrent appends from the worker and webhook never lose an event.
// We deliberately don't read-modify-write through ent: that's the
// classic last-writer-wins race the planning doc warned about.
//
// db is the raw *sql.DB lib/db.Open returned alongside the ent client
// — ent doesn't expose a stable raw-SQL exec path on the generated
// client, and threading the *sql.DB is cheap.
func AppendStage(ctx context.Context, db *sql.DB, buildID uuid.UUID, ev schema.StageEvent) error {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal stage event: %w", err)
	}
	// jsonb_build_array wraps the single event so `||` appends rather
	// than concatenating object keys. The status guard prevents a late
	// webhook from mutating an already-terminal build's timeline.
	res, err := db.ExecContext(ctx,
		`UPDATE builds
		   SET stages = stages || jsonb_build_array($1::jsonb)
		 WHERE id = $2 AND status NOT IN ('succeeded', 'failed')`,
		string(payload), buildID,
	)
	if err != nil {
		return fmt.Errorf("append stage: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("append stage rows affected: %w", err)
	}
	if rows == 0 {
		// Disambiguate: row missing entirely vs row terminal. Callers
		// distinguish the two via errors.Is(err, ErrBuildTerminal).
		var status string
		row := db.QueryRowContext(ctx, `SELECT status FROM builds WHERE id = $1`, buildID)
		switch err := row.Scan(&status); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("append stage: no row matched build id %s", buildID)
		case err != nil:
			return fmt.Errorf("append stage status check: %w", err)
		}
		return ErrBuildTerminal
	}
	return nil
}

// StageRunning is a small constructor so calling code reads as prose:
// AppendStage(ctx, db, id, StageRunning("reconcile")).
func StageRunning(name string) schema.StageEvent {
	return schema.StageEvent{Name: name, Status: StatusRunning, At: time.Now().UTC()}
}

// StageSucceeded marks a stage done, with optional structured data
// (image_uri etc. for push). Pass nil for stages with no payload.
func StageSucceeded(name string, data map[string]any) schema.StageEvent {
	ev := schema.StageEvent{Name: name, Status: StatusSucceeded, At: time.Now().UTC()}
	if len(data) > 0 {
		ev.Data = data
	}
	return ev
}

// StageFailed marks a stage as failed with a human-readable error
// message. Convention: data["error"] always carries the message string;
// callers can still attach extra fields if useful.
func StageFailed(name, errMsg string) schema.StageEvent {
	return schema.StageEvent{
		Name:   name,
		Status: StatusFailed,
		At:     time.Now().UTC(),
		Data:   map[string]any{"error": errMsg},
	}
}

// LatestRunningStage returns the name of the most recent stage whose
// status is "running" — i.e., the stage the build is currently in. The
// UI uses this to highlight the active step in the timeline.
//
// Returns "" when no stage is running (queued, succeeded, failed).
func LatestRunningStage(events []schema.StageEvent) string {
	// Walk in reverse — append-only, so the latest entry wins.
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Status == StatusRunning {
			return ev.Name
		}
		// If we hit a terminal entry (succeeded/failed) for a stage
		// that we don't see "running" for past us, the build is past
		// that stage. Either way we're done if this entry is final.
		if ev.Status == StatusSucceeded || ev.Status == StatusFailed {
			return ""
		}
	}
	return ""
}
