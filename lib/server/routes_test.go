package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spacefleet/app/lib/config"
)

// handler builds the HTTP tree without a real Postgres/Redis. The tests
// below only exercise routes that don't touch either — health is public,
// /api/ping hits the auth middleware and should reject before any handler
// runs, and the SPA fallback is entirely static.
func handler() http.Handler {
	return buildHandler(&config.Config{Addr: ":0", Env: "test"}, nil, nil, nil)
}

func TestHealthEndpointIsPublic(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("expected status=ok, got %q", body.Status)
	}
}

func TestProtectedEndpointRejectsUnauthenticated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/ping?name=crew", nil)
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session token, got %d", rec.Code)
	}
}

func TestSPAFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/some/spa/route", nil)
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" || ct[:9] != "text/html" {
		t.Fatalf("expected html content-type, got %q", ct)
	}
}
