package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spacefleet/app/lib/api"
	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/ui"
)

// Paths under /api/* that skip authentication entirely.
var publicAPIPaths = []string{"/api/health"}

func registerRoutes(mux *http.ServeMux, cfg *config.Config) {
	// API routes are generated from api/openapi.yaml and mounted under /api/*.
	// oapi-codegen applies middlewares in reverse, so the last entry wraps
	// outermost: RequireAuth runs first, then RequireOrg, then the handler.
	api.HandlerWithOptions(api.NewStrictHandler(api.NewServer(), nil), api.StdHTTPServerOptions{
		BaseRouter: mux,
		Middlewares: []api.MiddlewareFunc{
			api.MiddlewareFunc(auth.RequireOrg()),
			api.MiddlewareFunc(auth.RequireAuth(publicAPIPaths...)),
		},
	})

	// Public config exposed to the browser as `window.appConfig`. Only
	// pre-approved, non-secret values go here — it ships to every client.
	mux.HandleFunc("/config.js", appConfigHandler(cfg))

	// Everything else is the SPA (or its static assets).
	mux.Handle("/", ui.Handler())
}

func appConfigHandler(cfg *config.Config) http.HandlerFunc {
	payload, err := json.Marshal(map[string]string{
		"clerkPublishableKey": cfg.ClerkPublishableKey,
	})
	if err != nil {
		panic(err)
	}
	body := fmt.Sprintf("window.appConfig=%s;", payload)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(body))
	}
}
