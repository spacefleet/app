package auth

import (
	"context"
	"net/http"
	"strings"

	clerkhttp "github.com/clerk/clerk-sdk-go/v2/http"
)

// cliTokenPrefix marks a bearer token as a CLI credential rather than a
// Clerk JWT, letting the middleware route to the right verifier without
// attempting both.
const cliTokenPrefix = "sf_"

// CLITokenVerifier resolves a CLI bearer token to a ready-to-use Session,
// or returns an error explaining why the token is invalid. Implemented by
// lib/cli so the auth package stays decoupled from persistence.
type CLITokenVerifier func(ctx context.Context, plaintext string) (*Session, error)

// OrgMemberChecker returns true if userID can act on orgSlug. Called only
// for CLI-authenticated requests; Clerk sessions carry the active org in
// their claims and take the fast path.
type OrgMemberChecker func(ctx context.Context, userID, orgSlug string) (bool, error)

// RequireAuth verifies the Authorization header on every request that
// isn't in publicPaths. Bearer tokens with the CLI prefix go through
// verifyCLI; everything else falls back to Clerk JWT verification.
//
// Responses: 401 when no credential is provided or verification fails.
func RequireAuth(publicPaths []string, verifyCLI CLITokenVerifier) func(http.Handler) http.Handler {
	public := make(map[string]struct{}, len(publicPaths))
	for _, p := range publicPaths {
		public[p] = struct{}{}
	}
	clerkVerify := clerkhttp.WithHeaderAuthorization()

	return func(next http.Handler) http.Handler {
		// Clerk-authenticated branch: Clerk's middleware populates its own
		// context key; we then enforce that a session actually landed there.
		protectedClerk := clerkVerify(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := FromContext(r.Context()); !ok {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid session token")
				return
			}
			next.ServeHTTP(w, r)
		}))

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := public[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}

			if token := bearerToken(r); strings.HasPrefix(token, cliTokenPrefix) {
				if verifyCLI == nil {
					writeJSONError(w, http.StatusUnauthorized, "unauthorized", "cli auth not configured")
					return
				}
				sess, err := verifyCLI(r.Context(), token)
				if err != nil {
					writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid cli token")
					return
				}
				next.ServeHTTP(w, r.WithContext(WithCLISession(r.Context(), sess)))
				return
			}

			protectedClerk.ServeHTTP(w, r)
		})
	}
}

// RequireOrg enforces that the authenticated session has access to the
// :orgSlug segment in URLs of the form /api/orgs/{slug}/*. Clerk sessions
// match against the session's active org; CLI sessions delegate to
// checkMember (which should consult Clerk memberships).
//
// Responses: 401 when no session is on the context, 403 when access is
// denied, 502 when the membership lookup fails upstream.
func RequireOrg(checkMember OrgMemberChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := orgSlugFromPath(r.URL.Path)
			if slug == "" {
				next.ServeHTTP(w, r)
				return
			}
			sess, ok := FromContext(r.Context())
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing session")
				return
			}

			switch sess.Source {
			case SourceClerk:
				if sess.OrgSlug != slug {
					writeJSONError(w, http.StatusForbidden, "org_mismatch", "session organization does not match URL")
					return
				}
			case SourceCLI:
				if checkMember == nil {
					writeJSONError(w, http.StatusInternalServerError, "misconfigured", "membership checker missing")
					return
				}
				allowed, err := checkMember(r.Context(), sess.UserID, slug)
				if err != nil {
					writeJSONError(w, http.StatusBadGateway, "upstream_error", "membership check failed")
					return
				}
				if !allowed {
					writeJSONError(w, http.StatusForbidden, "not_member", "user is not a member of this organization")
					return
				}
			default:
				writeJSONError(w, http.StatusInternalServerError, "unknown_source", "session source not recognized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// orgSlugFromPath returns the slug segment from /api/orgs/{slug}/... paths,
// or "" when the path doesn't match that shape.
func orgSlugFromPath(path string) string {
	const prefix = "/api/orgs/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	slug, _, _ := strings.Cut(rest, "/")
	return slug
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
