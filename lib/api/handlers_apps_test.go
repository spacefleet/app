package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/spacefleet/app/lib/apps"
	"github.com/spacefleet/app/lib/auth"
)

// fakeAppsService is enough surface to exercise the handler logic
// without touching ent. The real apps.Service has a different
// concrete type, so we wrap it: handlers go through the *Server which
// accepts *apps.Service. To keep this file test-only and decoupled
// from a real DB, we stub via a type that implements only the methods
// we need — but our Server type holds *apps.Service, not an
// interface. So the handler tests are best done as integration tests
// alongside lib/apps/service_test.go where the DB is set up.
//
// What we *can* test cheaply here:
//   - The error-mapping helpers (apps service sentinel → HTTP status).
//   - The service-not-configured branches.

func TestCreateApp_NotConfigured(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, nil)
	resp, err := srv.CreateApp(withClerkSession(context.Background()), CreateAppRequestObject{
		Slug: "acme",
		Body: &CreateAppJSONRequestBody{
			Name:                 "App",
			CloudAccountId:       uuid.New(),
			GithubInstallationId: uuid.New(),
			GithubRepoFullName:   "acme/app",
		},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, ok := resp.(CreateAppdefaultJSONResponse)
	if !ok {
		t.Fatalf("expected error response, got %T", resp)
	}
	if got.StatusCode != 503 {
		t.Errorf("status = %d, want 503", got.StatusCode)
	}
}

func TestCreateApp_RequiresClerkSession(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, nil)
	resp, err := srv.CreateApp(withCLISession(context.Background()), CreateAppRequestObject{
		Slug: "acme",
		Body: &CreateAppJSONRequestBody{Name: "App"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, ok := resp.(CreateAppdefaultJSONResponse)
	if !ok {
		t.Fatalf("expected error response, got %T", resp)
	}
	if got.StatusCode != 403 {
		t.Errorf("status = %d, want 403", got.StatusCode)
	}
}

func TestCreateApp_RequiresAuth(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, nil)
	resp, err := srv.CreateApp(context.Background(), CreateAppRequestObject{
		Slug: "acme",
		Body: &CreateAppJSONRequestBody{Name: "App"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, ok := resp.(CreateAppdefaultJSONResponse)
	if !ok {
		t.Fatalf("expected error response, got %T", resp)
	}
	if got.StatusCode != 401 {
		t.Errorf("status = %d, want 401", got.StatusCode)
	}
}

func TestAppsErrToCreateRespMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{apps.ErrInvalidName, 400},
		{apps.ErrSlugReserved, 400},
		{apps.ErrCloudAccountMissing, 400},
		{apps.ErrCloudAccountNotReady, 400},
		{apps.ErrInstallationMissing, 400},
		{apps.ErrSlugSpaceExhausted, 409},
		{errors.New("oddly worded user error"), 400},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			resp := appsErrToCreateResp(tc.err)
			got, ok := resp.(CreateAppdefaultJSONResponse)
			if !ok {
				t.Fatalf("expected error response, got %T", resp)
			}
			if got.StatusCode != tc.want {
				t.Errorf("err %v → status %d, want %d", tc.err, got.StatusCode, tc.want)
			}
		})
	}
}

func TestAppsErrToDeleteRespMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{apps.ErrAppNotFound, 404},
		{apps.ErrBuildsRunning, 409},
		{apps.ErrAlreadyDeleting, 409},
		{errors.New("kaboom"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			resp := appsErrToDeleteResp(tc.err)
			got, ok := resp.(DeleteAppdefaultJSONResponse)
			if !ok {
				t.Fatalf("expected error response, got %T", resp)
			}
			if got.StatusCode != tc.want {
				t.Errorf("err %v → status %d, want %d", tc.err, got.StatusCode, tc.want)
			}
		})
	}
}

func withClerkSession(ctx context.Context) context.Context {
	return auth.WithCLISession(ctx, &auth.Session{
		Source:  auth.SourceClerk,
		UserID:  "user_clerk",
		OrgSlug: "acme",
	})
}

func withCLISession(ctx context.Context) context.Context {
	return auth.WithCLISession(ctx, &auth.Session{
		Source: auth.SourceCLI,
		UserID: "user_cli",
	})
}
