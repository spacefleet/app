package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/builds"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/lib/db"
	"github.com/spacefleet/app/lib/github"
	"github.com/spacefleet/app/lib/pulumi"
	"github.com/spacefleet/app/lib/queue"
)

// runWorker is `spacefleet worker`. The worker is the long-lived
// consumer of River jobs and the supervisor of in-flight Fargate
// tasks. Two responsibilities:
//
//  1. River workers process build / destroy_app jobs.
//  2. The poller goroutine reattaches to running builds, applies the
//     polling backstop, and times them out at the BuildTimeout ceiling.
//
// The two-process split (HTTP + worker) is documented in CLAUDE.md.
// Anything build- or pulumi-related runs here, never in the API
// process — that way the API is stateless and horizontally
// scalable.
func runWorker(_ []string) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("worker: load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Open ent + raw *sql.DB. The raw handle is what AppendStage uses
	// to write the jsonb stage events atomically — ent doesn't expose
	// a stable raw-exec path on the generated client.
	sqlDB, entClient, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("worker: open db: %v", err)
	}
	defer entClient.Close()
	defer sqlDB.Close()

	// River pool. Sized for the worker's concurrency.
	rpool, err := queue.Open(ctx, cfg.DatabaseURL, cfg.WorkerConcurrency)
	if err != nil {
		log.Fatalf("worker: open river pool: %v", err)
	}
	defer rpool.Close()

	migrated, err := queue.Migrate(ctx, rpool)
	if err != nil {
		log.Fatalf("worker: migrate: %v", err)
	}
	if len(migrated) == 0 {
		log.Print("worker: river migrations up to date")
	} else {
		for _, m := range migrated {
			log.Printf("worker: applied river migration %d (%s) in %s", m.Version, m.Name, m.Duration)
		}
	}

	workers := queue.NewWorkers()

	// Optional dependencies — if any of these fail to load we log and
	// continue. The worker still runs (e.g. River no-op processing),
	// but build/destroy work surfaces a clear error in the relevant
	// stage event.
	var ghSvc *github.Service
	if cfg.GitHubAppConfigured() {
		app, err := github.NewApp(cfg.GitHubAppID, cfg.GitHubAppSlug, cfg.GitHubAppPrivateKey)
		if err != nil {
			log.Fatalf("worker: github app: %v", err)
		}
		ghSvc = github.NewService(entClient, app)
	} else {
		log.Print("worker: github app not configured (builds and destroys will fail)")
	}

	var verifier *awsint.Verifier
	if cfg.AWSConfigured() {
		bootCtx, bootCancel := context.WithTimeout(ctx, 5*time.Second)
		v, vErr := awsint.NewVerifier(bootCtx, cfg.AWSPlatformAccountID)
		bootCancel()
		if vErr != nil {
			log.Fatalf("worker: aws verifier: %v", vErr)
		}
		verifier = v
	} else {
		log.Print("worker: aws onboarding not configured (builds and destroys will fail)")
	}

	// Pulumi orchestrator: needs the state backend + builder image.
	var orchestrator *pulumi.Orchestrator
	if cfg.BuildPipelineConfigured() && verifier != nil {
		o, err := pulumi.NewOrchestrator(pulumi.BackendConfig{
			Bucket:    cfg.StateBucket,
			Region:    cfg.StateBucketRegion,
			KMSKeyARN: cfg.StateKMSKeyARN,
		}, verifier, cfg.BuilderImage)
		if err != nil {
			log.Fatalf("worker: orchestrator: %v", err)
		}
		orchestrator = o
	} else {
		log.Print("worker: build pipeline not fully configured (set SPACEFLEET_PUBLIC_URL, SPACEFLEET_STATE_*, SPACEFLEET_BUILDER_IMAGE)")
	}

	// Build worker. Only registered if every dependency is present.
	if orchestrator != nil && ghSvc != nil && verifier != nil {
		buildWorker, err := builds.NewWorker(builds.WorkerConfig{
			Ent:           entClient,
			DB:            sqlDB,
			Orchestrator:  orchestrator,
			Verifier:      verifier,
			GitHub:        ghSvc,
			ECSClient:     ecsFactory,
			SecretsClient: secretsFactory,
			PublicURL:     cfg.PublicURL,
			BuildTimeout:  cfg.BuildTimeout,
			Stdout:        os.Stdout,
			Stderr:        os.Stderr,
		})
		if err != nil {
			log.Fatalf("worker: build worker: %v", err)
		}
		queue.RegisterBuildWorker(workers, buildWorker)
		log.Print("worker: build worker registered")
	} else {
		log.Print("worker: build worker NOT registered (missing dependencies)")
	}

	// destroy_app worker is only registered when we can actually run
	// pulumi destroy. If the build pipeline isn't configured, the API
	// gates destroy_resources=true requests upstream — no jobs will
	// arrive here for an unregistered kind.
	if orchestrator != nil && verifier != nil {
		destroyer := newDestroyAppRunner(entClient, orchestrator)
		queue.RegisterDestroyAppWorker(workers, destroyer)
		log.Print("worker: destroy_app worker registered")
	} else {
		log.Print("worker: destroy_app worker NOT registered (missing dependencies)")
	}

	client, err := queue.NewClient(rpool, queue.Config{
		WorkerMode:  true,
		Concurrency: cfg.WorkerConcurrency,
		Workers:     workers,
		Logger:      slog.Default(),
	})
	if err != nil {
		log.Fatalf("worker: new client: %v", err)
	}

	if err := client.Start(ctx); err != nil {
		log.Fatalf("worker: start: %v", err)
	}
	log.Printf("worker: started (concurrency=%d, build_timeout=%s)", cfg.WorkerConcurrency, cfg.BuildTimeout)

	// Poller — runs alongside River. Started in a goroutine so its
	// 30s tick doesn't block job processing. Cancellation via the
	// shared ctx drains both at shutdown.
	if orchestrator != nil && verifier != nil {
		poller, err := builds.NewPoller(builds.PollerConfig{
			Ent:           entClient,
			DB:            sqlDB,
			Verifier:      verifier,
			ECSClient:     ecsFactory,
			SecretsClient: secretsFactory,
			BuildTimeout:  cfg.BuildTimeout,
			Logger:        slog.Default(),
		})
		if err != nil {
			log.Fatalf("worker: poller: %v", err)
		}
		go poller.Run(ctx)
		log.Print("worker: build poller started")
	}

	// Heartbeat loop: emit an info-level log every 30s so deployments
	// without ECS/k8s health checks still have a clear "this worker is
	// alive" signal in the log stream.
	go heartbeat(ctx, 30*time.Second)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("worker: shutting down")

	cancel() // stop heartbeat + poller

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("worker: stop: %v", err)
	}
	log.Println("worker: stopped")
}

// ecsFactory / secretsFactory are the small adapters the worker and
// poller use to construct AWS clients from a freshly-assumed session.
// We keep the shape simple: take creds, return the SDK client. Real
// callers go straight to lib/aws; tests substitute mocks.
func ecsFactory(ctx context.Context, c awsint.SessionCreds) (awsint.ECSClient, error) {
	return awsint.NewECSClient(ctx, c)
}

func secretsFactory(ctx context.Context, c awsint.SessionCreds) (awsint.SecretsClient, error) {
	return awsint.NewSecretsClient(ctx, c)
}

// heartbeat emits a log line every interval until ctx fires. Cheap, but
// invaluable when triaging a worker that mysteriously stopped picking up
// jobs — silence is hard to interpret, presence-with-cadence isn't.
func heartbeat(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Print("worker: heartbeat")
		}
	}
}
