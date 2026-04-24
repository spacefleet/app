// Package auth wraps Clerk session verification and exposes middleware that
// extracts session claims from requests and enforces org-scoped access.
//
// Sessions can originate from two sources:
//   - Clerk JWTs sent by the browser (short-lived, SDK-refreshed).
//   - Long-lived CLI bearer tokens minted by this app (see lib/cli).
//
// FromContext hides the source from handlers; they only see a *Session.
package auth

import (
	"context"
	"net/http"

	"github.com/clerk/clerk-sdk-go/v2"
)

// Source identifies how a request was authenticated. Handlers that need
// source-specific behavior should branch on this; most handlers shouldn't.
type Source string

const (
	SourceClerk Source = "clerk"
	SourceCLI   Source = "cli"
)

// SetKey configures the Clerk secret key used for JWT verification and API
// calls. Must be called once at startup before any middleware runs.
func SetKey(secretKey string) {
	clerk.SetKey(secretKey)
}

// Session is the subset of identity information we surface to handlers.
// Clerk-only fields (SessionID, OrgID, OrgSlug, OrgRole, Permissions) are
// empty for CLI-authenticated requests — CLI tokens aren't org-scoped and
// carry no session/role metadata.
type Session struct {
	Source      Source
	UserID      string
	SessionID   string
	OrgID       string
	OrgSlug     string
	OrgRole     string
	Permissions []string
}

type contextKey int

const cliSessionKey contextKey = 1

// WithCLISession stores a CLI-derived session on ctx so FromContext and
// downstream handlers can read it.
func WithCLISession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, cliSessionKey, sess)
}

// FromContext returns the authenticated session from the request context,
// or (nil, false) if the request was not authenticated. CLI-derived
// sessions are checked before Clerk — they're only ever populated by our
// middleware, while the Clerk SDK may populate its own key opportunistically.
func FromContext(ctx context.Context) (*Session, bool) {
	if s, ok := ctx.Value(cliSessionKey).(*Session); ok && s != nil {
		return s, true
	}
	claims, ok := clerk.SessionClaimsFromContext(ctx)
	if !ok || claims == nil {
		return nil, false
	}
	return &Session{
		Source:      SourceClerk,
		UserID:      claims.Subject,
		SessionID:   claims.SessionID,
		OrgID:       claims.ActiveOrganizationID,
		OrgSlug:     claims.ActiveOrganizationSlug,
		OrgRole:     claims.ActiveOrganizationRole,
		Permissions: claims.ActiveOrganizationPermissions,
	}, true
}

// writeJSONError writes a minimal JSON error body. Kept intentionally small;
// callers that need richer errors can write their own responses.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"` + code + `","message":"` + msg + `"}`))
}
