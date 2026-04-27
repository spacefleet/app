package server

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/spacefleet/app/lib/apps"
	"github.com/spacefleet/app/lib/auth"
	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/builds"
	"github.com/spacefleet/app/lib/cache"
	"github.com/spacefleet/app/lib/cli"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/lib/db"
	"github.com/spacefleet/app/lib/github"
	"github.com/spacefleet/app/lib/queue"
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

	// Verifier loads the platform's default AWS credential chain once;
	// reused by the onboarding service, the logs controller, and the
	// webhook handler. If creds aren't loadable we still wire awsSvc so
	// start/list/delete routes work — only Verify surfaces the error.
	var verifier *awsint.Verifier
	if cfg.AWSConfigured() {
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 5*time.Second)
		v, vErr := awsint.NewVerifier(bootCtx, cfg.AWSPlatformAccountID)
		bootCancel()
		if vErr != nil {
			log.Printf("aws verifier disabled: %v", vErr)
		} else {
			verifier = v
		}
	} else {
		log.Print("aws onboarding not configured (set AWS_PLATFORM_ACCOUNT_ID and AWS_CFN_TEMPLATE_URL to enable)")
	}

	var awsSvc *awsint.Service
	if cfg.AWSConfigured() {
		awsSvc = awsint.NewService(entClient, verifier, cfg.AWSPlatformAccountID, cfg.AWSCFNTemplateURL)
	}

	// Insert-only River client so the HTTP API can enqueue
	// destroy_app jobs without competing with the worker for jobs.
	// Sized for inserts only — the worker process keeps its own
	// pool sized for its concurrency. We open it eagerly so a bad
	// DSN fails at boot, not at first request.
	queuePool, err := queue.Open(context.Background(), cfg.DatabaseURL, 1)
	if err != nil {
		_ = entClient.Close()
		_ = sqlDB.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("open queue pool: %w", err)
	}
	queueClient, err := queue.NewClient(queuePool, queue.Config{})
	if err != nil {
		queuePool.Close()
		_ = entClient.Close()
		_ = sqlDB.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("queue client: %w", err)
	}

	// The apps service needs a RepoLookup (to capture default
	// branches at create time) and a DestroyEnqueuer for the
	// destroy_app River job. Each may be nil — the service surfaces
	// a clear "not configured" error at request time when something
	// downstream is unavailable.
	//
	// We gate the destroy enqueuer on BuildPipelineConfigured because
	// the worker only registers the destroy_app handler when the
	// pipeline is fully wired. Enqueuing into a queue with no handler
	// would land the row in deleting_at forever.
	var destroyEnqueuer apps.DestroyEnqueuer
	if cfg.BuildPipelineConfigured() {
		destroyEnqueuer = queueClient
	}
	appsSvc := apps.NewService(entClient, ghSvc, destroyEnqueuer)

	// Build pipeline: the API needs a builds.Service to expose
	// /api/orgs/{org}/apps/{app}/builds, and a *builds.WebhookHandler
	// to authenticate the per-build callbacks from the Fargate task.
	// Both are best-effort: if the build pipeline isn't fully
	// configured (state backend, public URL), the API surface
	// degrades to "service unavailable" rather than crashing.
	var buildsSvc *builds.Service
	if ghSvc != nil {
		buildsSvc = builds.NewService(entClient, ghSvc, queueClient)
	}

	// Build-logs controller. Independent of buildsSvc — it only needs
	// AWS to assume into the customer's account and read CloudWatch.
	// We don't gate on PublicURL (CloudWatch reads need no callback URL)
	// or on the GitHub App being configured; reading logs against an
	// existing build is the same regardless of whether new builds can
	// be enqueued.
	var logsCtrl *builds.LogsController
	if verifier != nil {
		ctrl, err := builds.NewLogsController(builds.LogsConfig{
			Ent:        entClient,
			Verifier:   verifier,
			LogsClient: func(ctx context.Context, c awsint.SessionCreds) (awsint.LogsClient, error) { return awsint.NewLogsClient(ctx, c) },
		})
		if err != nil {
			log.Printf("logs controller disabled: %v", err)
		} else {
			logsCtrl = ctrl
		}
	}

	var webhookHandler *builds.WebhookHandler
	if verifier != nil && cfg.PublicURL != "" {
		handler, err := builds.NewWebhookHandler(builds.WebhookConfig{
			Ent:           entClient,
			DB:            sqlDB,
			Verifier:      verifier,
			SecretsClient: func(ctx context.Context, c awsint.SessionCreds) (awsint.SecretsClient, error) { return awsint.NewSecretsClient(ctx, c) },
			Logger:        slog.Default(),
		})
		if err != nil {
			log.Printf("webhook handler disabled: %v", err)
		} else {
			webhookHandler = handler
		}
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           buildHandler(cfg, cliSvc, ghSvc, awsSvc, appsSvc, buildsSvc, logsCtrl, webhookHandler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	srv.RegisterOnShutdown(func() {
		queuePool.Close()
		_ = entClient.Close()
		_ = sqlDB.Close()
		_ = redisClient.Close()
	})
	return srv, nil
}

// buildHandler composes the full HTTP handler tree given pre-built deps.
// Any of the services may be nil — middleware rejects CLI tokens when
// cliSvc is missing, and the GitHub/AWS routes return a clear error
// when their service is nil. Keeps route-level tests usable without a
// real DB or external creds.
func buildHandler(cfg *config.Config, cliSvc *cli.Service, ghSvc *github.Service, awsSvc *awsint.Service, appsSvc *apps.Service, buildsSvc *builds.Service, logsCtrl *builds.LogsController, webhookHandler *builds.WebhookHandler) http.Handler {
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, cliSvc, ghSvc, awsSvc, appsSvc, buildsSvc, logsCtrl, webhookHandler)
	return logRequests(mux)
}
