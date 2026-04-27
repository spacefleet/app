package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These tests cover the API-shape concerns of the build handlers — the
// not-configured / unauthorized branches and the parameter wiring. The
// LogsController's behaviour is exercised in lib/builds/logs_test.go
// against a real Postgres; reproducing that surface here would just
// duplicate it.

func TestGetBuildLogs_NotConfigured(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, nil)
	resp, err := srv.GetBuildLogs(withClerkSession(context.Background()), GetBuildLogsRequestObject{
		Slug:    "acme",
		AppSlug: "app",
		BuildId: BuildID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("GetBuildLogs: %v", err)
	}
	got, ok := resp.(GetBuildLogsdefaultJSONResponse)
	if !ok {
		t.Fatalf("expected error response, got %T", resp)
	}
	if got.StatusCode != 503 {
		t.Errorf("status = %d, want 503", got.StatusCode)
	}
}
