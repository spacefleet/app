package auth

import (
	"net/http"
	"strings"

	clerkhttp "github.com/clerk/clerk-sdk-go/v2/http"
)

// RequireAuth verifies a Clerk session JWT on the Authorization header and
// populates the request context with session claims. Requests to any path
// in publicPaths bypass verification entirely.
//
// Responses: 401 when no token is provided or verification fails.
func RequireAuth(publicPaths ...string) func(http.Handler) http.Handler {
	public := make(map[string]struct{}, len(publicPaths))
	for _, p := range publicPaths {
		public[p] = struct{}{}
	}
	// Clerk's middleware handles signature verification, JWKS fetching and
	// caching, and expiry checks. We just layer a public-path bypass on top.
	verify := clerkhttp.WithHeaderAuthorization()

	return func(next http.Handler) http.Handler {
		protected := verify(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			protected.ServeHTTP(w, r)
		})
	}
}

// RequireOrg enforces that the authenticated session's active organization
// slug matches the :orgSlug segment in URLs of the form /api/orgs/{slug}/*.
// Requests that don't match that prefix pass through unchanged, so this
// middleware is safe to layer over every /api/* route.
//
// Responses: 401 when no session is on the context, 403 when the slug on
// the session doesn't match the URL.
func RequireOrg() func(http.Handler) http.Handler {
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
			if sess.OrgSlug != slug {
				writeJSONError(w, http.StatusForbidden, "org_mismatch", "session organization does not match URL")
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
