package builds

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/schema"
	awsint "github.com/spacefleet/app/lib/aws"
)

// WebhookMaxBodyBytes caps the size of a stage event body we'll process.
// Real events are well under 1KB; capping at 64KB prevents a misbehaving
// (or hostile) caller from allocating arbitrary memory before HMAC
// verification runs.
const WebhookMaxBodyBytes = 64 * 1024

// WebhookFreshnessWindow is the maximum drift between a request's
// X-Spacefleet-Timestamp header and our wall clock. Five minutes mirrors
// the planning doc — long enough to absorb real clock skew, short
// enough that a captured event can't be replayed indefinitely.
const WebhookFreshnessWindow = 5 * time.Minute

// HeaderTimestamp / HeaderSignature are the HMAC envelope. Names match
// what builder/entrypoint.sh emits; change one, change both.
const (
	HeaderTimestamp = "X-Spacefleet-Timestamp"
	HeaderSignature = "X-Spacefleet-Signature"
)

// WebhookHandler serves POST /api/internal/builds/{buildID}/events.
// Mounted *outside* the auth middleware — authentication here is HMAC
// over the request body keyed on the per-build secret stored at create
// time. Every error response is 401 with the same body so a probe can't
// distinguish "build doesn't exist" from "bad signature" from "stale
// timestamp."
type WebhookHandler struct {
	ent           *ent.Client
	db            *sql.DB
	verifier      CredentialIssuer
	secretsClient SecretsFactory
	logger        *slog.Logger
	now           func() time.Time
}

// WebhookConfig collects the dependencies for the webhook handler.
//
// secretsClient + verifier are required so the handler can clean up the
// per-build GitHub-token secret on terminal events; we don't want to
// wait until the poller's next tick for that.
type WebhookConfig struct {
	Ent           *ent.Client
	DB            *sql.DB
	Verifier      CredentialIssuer
	SecretsClient SecretsFactory
	Logger        *slog.Logger

	// Now overrides the timestamp source for tests. Production callers
	// leave it nil (defaults to time.Now).
	Now func() time.Time
}

func (c WebhookConfig) Validate() error {
	if c.Ent == nil {
		return errors.New("webhook: Ent client required")
	}
	if c.DB == nil {
		return errors.New("webhook: DB required")
	}
	if c.Verifier == nil {
		return errors.New("webhook: Verifier required")
	}
	if c.SecretsClient == nil {
		return errors.New("webhook: SecretsClient factory required")
	}
	return nil
}

// NewWebhookHandler validates config and returns a ready handler.
func NewWebhookHandler(cfg WebhookConfig) (*WebhookHandler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &WebhookHandler{
		ent:           cfg.Ent,
		db:            cfg.DB,
		verifier:      cfg.Verifier,
		secretsClient: cfg.SecretsClient,
		logger:        logger,
		now:           now,
	}, nil
}

// Path returns the route pattern this handler serves. Goes into
// routes.go so the handler and its mount stay in lockstep.
const WebhookPath = "/api/internal/builds/{buildID}/events"

// stageEventBody is the wire shape the builder sends. Same fields as
// schema.StageEvent minus the timestamp — we stamp `at` with our wall
// clock when we append, so the event's clock matches the stages
// timeline even if the builder's clock drifts.
type stageEventBody struct {
	Stage  string         `json:"stage"`
	Status string         `json:"status"`
	Data   map[string]any `json:"data,omitempty"`
}

// ServeHTTP is the entry point. The handler authenticates, writes the
// stage event, and (when the event is terminal) flips the build status
// + cleans up the GitHub-token secret.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeUnauth(w)
		return
	}

	// Path parsing: r.PathValue is Go 1.22's stdlib router. Routes
	// with the {buildID} segment register via mux.HandleFunc.
	idStr := r.PathValue("buildID")
	if idStr == "" {
		writeUnauth(w)
		return
	}
	buildID, err := uuid.Parse(idStr)
	if err != nil {
		writeUnauth(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, WebhookMaxBodyBytes+1))
	if err != nil {
		writeUnauth(w)
		return
	}
	if len(body) > WebhookMaxBodyBytes {
		writeUnauth(w)
		return
	}

	tsHeader := r.Header.Get(HeaderTimestamp)
	sigHeader := r.Header.Get(HeaderSignature)
	if tsHeader == "" || sigHeader == "" {
		writeUnauth(w)
		return
	}

	tsUnix, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		writeUnauth(w)
		return
	}
	ts := time.Unix(tsUnix, 0)
	now := h.now()
	skew := now.Sub(ts)
	if skew < 0 {
		skew = -skew
	}
	if skew > WebhookFreshnessWindow {
		writeUnauth(w)
		return
	}

	row, err := h.ent.Build.Get(r.Context(), buildID)
	if err != nil {
		writeUnauth(w)
		return
	}
	if row.WebhookSecret == "" {
		writeUnauth(w)
		return
	}

	// Constant-time signature compare.
	signedPayload := tsHeader + "." + string(body)
	mac := hmac.New(sha256.New, []byte(row.WebhookSecret))
	mac.Write([]byte(signedPayload))
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(sigHeader)
	if err != nil {
		writeUnauth(w)
		return
	}
	if !hmac.Equal(expected, got) {
		writeUnauth(w)
		return
	}

	var ev stageEventBody
	if err := json.Unmarshal(body, &ev); err != nil {
		// We already authenticated, so leak the parse error as 400 —
		// not 401. Misbehaving builder, not an attacker.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !validStage(ev.Stage) || !validStatus(ev.Status) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Append the stage event. If the build is already terminal (a late
	// retry from the builder, say), we acknowledge with 200 and drop
	// the event — letting the builder retry into the timestamp window
	// would just produce 5 minutes of noise.
	stage := stageEventFromBody(ev, now)
	if err := AppendStage(r.Context(), h.db, buildID, stage); err != nil {
		if errors.Is(err, ErrBuildTerminal) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		h.logger.ErrorContext(r.Context(), "webhook: append stage", "build_id", buildID, "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	// Terminal transitions drive top-level status. push.succeeded is
	// the only "build went all the way" signal; any failed event ends
	// the build.
	switch {
	case ev.Stage == StagePush && ev.Status == StatusSucceeded:
		if err := h.markBuildSucceeded(r.Context(), row, ev.Data); err != nil {
			h.logger.ErrorContext(r.Context(), "webhook: mark succeeded", "build_id", buildID, "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		go h.bestEffortCleanupSecret(buildID)
	case ev.Status == StatusFailed:
		errMsg := "build failed"
		if ev.Data != nil {
			if e, ok := ev.Data["error"].(string); ok && e != "" {
				errMsg = e
			}
		}
		if err := h.markBuildFailed(r.Context(), row, ev.Stage, errMsg); err != nil {
			h.logger.ErrorContext(r.Context(), "webhook: mark failed", "build_id", buildID, "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		go h.bestEffortCleanupSecret(buildID)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (h *WebhookHandler) markBuildSucceeded(ctx context.Context, b *ent.Build, data map[string]any) error {
	now := h.now().UTC()
	upd := h.ent.Build.UpdateOneID(b.ID).
		SetStatus(BuildStatusSucceeded).
		SetEndedAt(now)
	if data != nil {
		if uri, ok := data["image_uri"].(string); ok {
			upd = upd.SetImageURI(uri)
		}
		if dig, ok := data["image_digest"].(string); ok {
			upd = upd.SetImageDigest(dig)
		}
	}
	_, err := upd.Save(ctx)
	return err
}

func (h *WebhookHandler) markBuildFailed(ctx context.Context, b *ent.Build, stage, errMsg string) error {
	now := h.now().UTC()
	_, err := h.ent.Build.UpdateOneID(b.ID).
		SetStatus(BuildStatusFailed).
		SetErrorMessage(truncate(stage+": "+errMsg, 4000)).
		SetEndedAt(now).
		Save(ctx)
	return err
}

// bestEffortCleanupSecret deletes the per-build GitHub-token secret
// after a terminal event. Runs in a goroutine because the webhook
// handler should respond quickly; the secret cleanup involves a fresh
// AssumeRole + Secrets Manager call. Failures are logged; the poller
// will catch up if this somehow misses.
func (h *WebhookHandler) bestEffortCleanupSecret(buildID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	row, err := h.ent.Build.Get(ctx, buildID)
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret load build", "build_id", buildID, "err", err)
		return
	}
	app, err := h.ent.App.Get(ctx, row.AppID)
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret load app", "build_id", buildID, "err", err)
		return
	}
	ca, err := h.ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret load cloud account", "build_id", buildID, "err", err)
		return
	}
	region := ca.Region
	if region == "" {
		region = "us-east-1"
	}
	envMap, err := h.verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-cleanup-"+shortID(buildID))
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret assume role", "build_id", buildID, "err", err)
		return
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret creds", "build_id", buildID, "err", err)
		return
	}
	secretsClient, err := h.secretsClient(ctx, creds)
	if err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret client", "build_id", buildID, "err", err)
		return
	}
	name := awsint.BuildTokenSecretName(app.ID.String(), buildID.String())
	if err := awsint.DeleteBuildTokenSecret(ctx, secretsClient, name); err != nil {
		h.logger.ErrorContext(ctx, "webhook: cleanup secret", "build_id", buildID, "err", err)
	}
}

// stageEventFromBody promotes the wire-shape body to a schema.StageEvent
// stamped with our wall clock.
func stageEventFromBody(body stageEventBody, now time.Time) schema.StageEvent {
	return schema.StageEvent{
		Name:   body.Stage,
		Status: body.Status,
		At:     now.UTC(),
		Data:   body.Data,
	}
}

// validStage / validStatus enforce the wire-side enum at the boundary,
// so a typo in the builder doesn't propagate into the timeline.
func validStage(s string) bool {
	switch s {
	case StageReconcile, StagePrepare, StageDispatch, StageClone, StageBuild, StagePush:
		return true
	}
	return false
}

func validStatus(s string) bool {
	switch s {
	case StatusRunning, StatusSucceeded, StatusFailed:
		return true
	}
	return false
}

// writeUnauth writes the same opaque 401 for every authentication
// failure mode. Differential responses help attackers enumerate; this
// keeps the response identical for "build not found", "bad signature",
// "stale timestamp", and "missing headers."
func writeUnauth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"unauthorized","message":"unauthorized"}`))
}

// SignWebhook is the helper the builder shell script's openssl call
// also implements. We expose it so the test driver and any future Go-
// based builder can compute signatures without reimplementing the
// envelope shape. ts is the unix timestamp the builder will send;
// secret is the per-build HMAC key (plaintext).
func SignWebhook(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
