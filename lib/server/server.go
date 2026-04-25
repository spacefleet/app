package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/cache"
	"github.com/spacefleet/app/lib/cli"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/lib/db"
	"github.com/spacefleet/app/lib/github"
)

// New wires all runtime dependencies (Postgres, Redis, the CLI auth
// service) and returns a ready-to-serve *http.Server. Closing those
// dependencies is registered with Server.RegisterOnShutdown so callers
// only have to drive the HTTP lifecycle.
func New(cfg *config.Config) (*http.Server, error) {
	auth.SetKey(cfg.ClerkSecretKey)

	sqlDB, entClient, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Bounded connect timeout — if Redis is down at boot we want a clear
	// error, not a hanging process.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	redisClient, err := cache.Open(pingCtx, cfg.RedisURL)
	if err != nil {
		_ = entClient.Close()
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open redis: %w", err)
	}

	cliSvc := cli.NewService(entClient, redisClient)

	var ghSvc *github.Service
	if cfg.GitHubAppConfigured() {
		app, err := github.NewApp(cfg.GitHubAppID, cfg.GitHubAppSlug, cfg.GitHubAppPrivateKey)
		if err != nil {
			_ = entClient.Close()
			_ = sqlDB.Close()
			_ = redisClient.Close()
			return nil, fmt.Errorf("github app: %w", err)
		}
		ghSvc = github.NewService(entClient, app)
	} else {
		// Fail open: the GitHub flows return a clear "not configured"
		// error at request time. Useful for local dev where you might
		// not have an App registered yet.
		log.Print("github app not configured (set GITHUB_APP_ID, GITHUB_APP_SLUG, GITHUB_APP_PRIVATE_KEY[_PATH] to enable)")
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           buildHandler(cfg, cliSvc, ghSvc),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	srv.RegisterOnShutdown(func() {
		_ = entClient.Close()
		_ = sqlDB.Close()
		_ = redisClient.Close()
	})
	return srv, nil
}

// buildHandler composes the full HTTP handler tree given pre-built deps.
// Either service may be nil — middleware rejects CLI tokens when cliSvc
// is missing, and the GitHub routes return a clear error when ghSvc is
// missing. Both behaviors keep route-level tests usable without a DB.
func buildHandler(cfg *config.Config, cliSvc *cli.Service, ghSvc *github.Service) http.Handler {
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, cliSvc, ghSvc)
	return logRequests(mux)
}
