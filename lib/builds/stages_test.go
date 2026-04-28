package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent/schema"
)

func TestAppendStage_Concurrent(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)

	ctx := context.Background()
	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("user").
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rawDB := rawDBFromClient(t, client)

	// Hit AppendStage concurrently from many goroutines and verify we
	// don't lose events. Postgres' jsonb || is atomic, so all events
	// should land.
	const N = 20
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			errCh <- AppendStage(ctx, rawDB, row.ID, schema.StageEvent{
				Name:   "build",
				Status: StatusRunning,
				At:     time.Now().UTC(),
				Data:   map[string]any{"i": i},
			})
		}(i)
	}
	for i := 0; i < N; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent append: %v", err)
		}
	}

	got, err := client.Build.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Stages) != N {
		t.Errorf("stages = %d, want %d (concurrent appends should not lose events)", len(got.Stages), N)
	}
}

func TestAppendStage_NonexistentBuild(t *testing.T) {
	client := newTestClient(t)
	rawDB := rawDBFromClient(t, client)
	err := AppendStage(context.Background(), rawDB, uuid.New(), StageRunning("reconcile"))
	if err == nil {
		t.Fatal("expected error for nonexistent build")
	}
	if errors.Is(err, ErrBuildTerminal) {
		t.Fatal("expected not-found error, got ErrBuildTerminal")
	}
}

// TestAppendStage_TerminalBuildRejected protects against a late
// webhook (or other anomaly) appending stages onto a build that has
// already reached succeeded/failed.
func TestAppendStage_TerminalBuildRejected(t *testing.T) {
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	ctx := context.Background()
	rawDB := rawDBFromClient(t, client)

	for _, status := range []string{BuildStatusSucceeded, BuildStatusFailed} {
		t.Run(status, func(t *testing.T) {
			row, err := client.Build.Create().
				SetAppID(fix.app.ID).
				SetSourceRef("main").
				SetSourceSha(strings.Repeat("a", 40)).
				SetWebhookSecret("s").
				SetCreatedBy("u").
				SetStatus(status).
				Save(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = AppendStage(ctx, rawDB, row.ID, StageRunning(StageBuild))
			if !errors.Is(err, ErrBuildTerminal) {
				t.Fatalf("got %v, want ErrBuildTerminal", err)
			}
			got, _ := client.Build.Get(ctx, row.ID)
			if len(got.Stages) != 0 {
				t.Errorf("stages mutated despite terminal status: %+v", got.Stages)
			}
		})
	}
}

func TestLatestRunningStage(t *testing.T) {
	cases := []struct {
		name string
		evs  []schema.StageEvent
		want string
	}{
		{"empty", nil, ""},
		{"one running", []schema.StageEvent{{Name: "reconcile", Status: StatusRunning}}, "reconcile"},
		{"running then succeeded", []schema.StageEvent{
			{Name: "reconcile", Status: StatusRunning},
			{Name: "reconcile", Status: StatusSucceeded},
		}, ""},
		{"two running stages", []schema.StageEvent{
			{Name: "reconcile", Status: StatusSucceeded},
			{Name: "build", Status: StatusRunning},
		}, "build"},
		{"stages then push succeeded", []schema.StageEvent{
			{Name: "reconcile", Status: StatusSucceeded},
			{Name: "build", Status: StatusSucceeded},
			{Name: "push", Status: StatusSucceeded},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LatestRunningStage(tc.evs); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
