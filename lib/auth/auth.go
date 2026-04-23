// Package auth wraps Clerk session verification and exposes middleware that
// extracts session claims from requests and enforces org-scoped access.
package auth

import (
	"context"
	"net/http"

	"github.com/clerk/clerk-sdk-go/v2"
)

// SetKey configures the Clerk secret key used for JWT verification and API
// calls. Must be called once at startup before any middleware runs.
func SetKey(secretKey string) {
	clerk.SetKey(secretKey)
}

// Session is the subset of Clerk session claims we surface to handlers.
type Session struct {
	UserID      string
	SessionID   string
	OrgID       string
	OrgSlug     string
	OrgRole     string
	Permissions []string
}

// FromContext returns the authenticated session from the request context,
// or (nil, false) if the request was not authenticated. Handlers behind
// RequireAuth can rely on a non-nil session.
func FromContext(ctx context.Context) (*Session, bool) {
	claims, ok := clerk.SessionClaimsFromContext(ctx)
	if !ok || claims == nil {
		return nil, false
	}
	return &Session{
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
