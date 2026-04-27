package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/spacefleet/app/lib/api"
	"github.com/spacefleet/app/lib/apps"
	"github.com/spacefleet/app/lib/auth"
	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/builds"
	"github.com/spacefleet/app/lib/cli"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/lib/github"
	"github.com/spacefleet/app/ui"
)

// Paths under /api/* that skip authentication entirely. /api/cli/auth/exchange
// is public because the CLI calls it without a token (that's the whole point
// of the exchange).
var publicAPIPaths = []string{
	"/api/health",
	"/api/cli/auth/exchange",
}

func registerRoutes(mux *http.ServeMux, cfg *config.Config, cliSvc *cli.Service, ghSvc *github.Service, awsSvc *awsint.Service, appsSvc *apps.Service, buildsSvc *builds.Service, logsCtrl *builds.LogsController, webhookHandler *builds.WebhookHandler) {
	// Internal build webhook. Mounted *before* the OpenAPI routes so
	// the more-specific path wins; mounted *outside* the auth
	// middleware so the builder Fargate task can call us with HMAC
	// auth instead of a Clerk session. The handler does its own
	// authentication via X-Spacefleet-Signature.
	if webhookHandler != nil {
		mux.HandleFunc("POST "+builds.WebhookPath, webhookHandler.ServeHTTP)
	}

	// API routes are generated from api/openapi.yaml and mounted under /api/*.
	// oapi-codegen applies middlewares in reverse, so the last entry wraps
	// outermost: RequireAuth runs first, then RequireOrg, then the handler.
	api.HandlerWithOptions(api.NewStrictHandler(api.NewServer(cliSvc, ghSvc, awsSvc, appsSvc, buildsSvc, logsCtrl), nil), api.StdHTTPServerOptions{
		BaseRouter: mux,
		Middlewares: []api.MiddlewareFunc{
			api.MiddlewareFunc(auth.RequireOrg(cliMemberChecker(cliSvc))),
			api.MiddlewareFunc(auth.RequireAuth(publicAPIPaths, cliTokenVerifier(cliSvc))),
		},
	})

	// Public config exposed to the browser as `window.appConfig`. Only
	// pre-approved, non-secret values go here — it ships to every client.
	mux.HandleFunc("/config.js", appConfigHandler(cfg))

	// Everything else is the SPA (or its static assets).
	mux.Handle("/", ui.Handler())
}

// cliTokenVerifier adapts the cli.Service for lib/auth. nil-safe: when the
// service isn't wired (e.g. in route-level tests), any CLI-prefixed token
// is rejected rather than crashing.
func cliTokenVerifier(svc *cli.Service) auth.CLITokenVerifier {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, plaintext string) (*auth.Session, error) {
		t, err := svc.VerifyToken(ctx, plaintext)
		if err != nil {
			return nil, err
		}
		return &auth.Session{Source: auth.SourceCLI, UserID: t.UserID}, nil
	}
}

// cliMemberChecker adapts cli.Service.UserCanAccessOrg so RequireOrg can use
// it for CLI-authenticated requests.
func cliMemberChecker(svc *cli.Service) auth.OrgMemberChecker {
	if svc == nil {
		return func(_ context.Context, _, _ string) (bool, error) {
			return false, errors.New("cli auth not configured")
		}
	}
	return svc.UserCanAccessOrg
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
