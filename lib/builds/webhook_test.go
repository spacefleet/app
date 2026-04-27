package builds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	awsint "github.com/spacefleet/app/lib/aws"
)

// fakeVerifier never reaches AWS; the webhook handler only invokes it
// during the best-effort secret cleanup goroutine. We make it return
// dummy creds so the goroutine completes; tests assert on visible side
// effects (status row, stages array) rather than the cleanup itself.
type fakeVerifier struct {
	err error
}

func (f *fakeVerifier) AssumeRoleEnv(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIA",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN":     "token",
		"AWS_REGION":            "us-east-1",
	}, nil
}

func setupBuild(t *testing.T) (*ent.Client, *ent.Build) {
	t.Helper()
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("test-secret").
		SetCreatedBy("user").
		SetStatus("running").
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return client, row
}

func newWebhookHandlerForTest(t *testing.T, client *ent.Client) *WebhookHandler {
	t.Helper()
	h, err := NewWebhookHandler(WebhookConfig{
		Ent:           client,
		DB:            rawDBFromClient(t, client),
		Verifier:      &fakeVerifier{err: errors.New("cleanup disabled")},
		SecretsClient: noopSecretsFactory,
		Now:           func() time.Time { return time.Unix(1700_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// noopSecretsFactory satisfies the SecretsFactory shape but returns an
// error so the cleanup goroutine bails immediately. The webhook
// response isn't blocked on it.
func noopSecretsFactory(_ context.Context, _ awsint.SessionCreds) (awsint.SecretsClient, error) {
	return nil, errors.New("secrets disabled in tests")
}

// signRequest builds a valid POST against the webhook handler. ts must
// be within the freshness window of the handler's `now` (we set it to
// 1700_000_000 in tests, so caller passes that or a small drift).
func signRequest(t *testing.T, secret string, ts int64, body []byte, buildID uuid.UUID) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		"/api/internal/builds/"+buildID.String()+"/events",
		bytes.NewReader(body))
	r.SetPathValue("buildID", buildID.String())
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	r.Header.Set(HeaderSignature, SignWebhook(secret, ts, body))
	return r
}

func TestWebhook_HappyPath_StageEvent(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"clone","status":"running"}`)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got, err := client.Build.Get(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Stages) != 1 {
		t.Fatalf("stages = %d", len(got.Stages))
	}
	if got.Stages[0].Name != "clone" || got.Stages[0].Status != "running" {
		t.Errorf("stage = %+v", got.Stages[0])
	}
	if got.Status != "running" {
		t.Errorf("top-level status = %q (should still be running)", got.Status)
	}
}

func TestWebhook_TerminalSuccess(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	bodyJSON := map[string]any{
		"stage":  "push",
		"status": "succeeded",
		"data": map[string]any{
			"image_uri":    "111.dkr.ecr.us-east-1.amazonaws.com/spacefleet-x:abc",
			"image_digest": "sha256:abcdef",
		},
	}
	body, _ := json.Marshal(bodyJSON)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got, err := client.Build.Get(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	if got.ImageURI == "" || got.ImageDigest == "" {
		t.Errorf("image fields missing: uri=%q digest=%q", got.ImageURI, got.ImageDigest)
	}
	if got.EndedAt == nil {
		t.Error("expected ended_at")
	}
}

func TestWebhook_TerminalFailure(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"build","status":"failed","data":{"error":"Dockerfile not found"}}`)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got, err := client.Build.Get(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.ErrorMessage == "" || !strings.Contains(got.ErrorMessage, "Dockerfile not found") {
		t.Errorf("error_message = %q", got.ErrorMessage)
	}
}

func TestWebhook_BadSignature(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"clone","status":"running"}`)
	r := signRequest(t, "wrong-secret", ts, body, b.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhook_StaleTimestamp(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	// Handler `now` is 1700_000_000; timestamp 6 minutes earlier is
	// outside the 5-minute freshness window.
	ts := int64(1700_000_000 - 6*60)
	body := []byte(`{"stage":"clone","status":"running"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhook_FutureTimestampOK(t *testing.T) {
	// Real-world clocks drift; we accept up to 5 minutes in either
	// direction.
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000 + 4*60) // 4 minutes ahead — should pass
	body := []byte(`{"stage":"clone","status":"running"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestWebhook_ReplaySamePayload(t *testing.T) {
	// We don't track nonces; we rely on the timestamp window to bound
	// replay risk. This test just confirms the second submission with
	// the same valid timestamp also succeeds — that's the documented
	// behavior; consumers are responsible for not double-emitting.
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"clone","status":"running"}`)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d", i, rec.Code)
		}
	}
	got, err := client.Build.Get(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Stages) != 2 {
		t.Errorf("stages = %d, want 2 (replays append)", len(got.Stages))
	}
}

func TestWebhook_BadStageOrStatus(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"made-up","status":"running"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signRequest(t, b.WebhookSecret, ts, body, b.ID))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestWebhook_MissingHeaders(t *testing.T) {
	client, b := setupBuild(t)
	h := newWebhookHandlerForTest(t, client)
	body := []byte(`{"stage":"clone","status":"running"}`)
	r := httptest.NewRequest(http.MethodPost,
		"/api/internal/builds/"+b.ID.String()+"/events",
		bytes.NewReader(body))
	r.SetPathValue("buildID", b.ID.String())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhook_UnknownBuild(t *testing.T) {
	client := newTestClient(t)
	_ = newAppFixture(t, client)
	h := newWebhookHandlerForTest(t, client)
	ts := int64(1700_000_000)
	body := []byte(`{"stage":"clone","status":"running"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signRequest(t, "any-secret", ts, body, uuid.New()))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no enumeration)", rec.Code)
	}
}
