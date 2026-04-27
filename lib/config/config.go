package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr                string
	Env                 string
	ClerkPublishableKey string
	ClerkSecretKey      string
	DatabaseURL         string
	RedisURL            string

	// GitHub App credentials. The platform is parameterized over these so
	// hosted Spacefleet ships its own App and self-hosters register theirs;
	// no code path may hardcode a particular installation.
	GitHubAppID            int64
	GitHubAppSlug          string
	GitHubAppPrivateKey    []byte
	GitHubAppWebhookSecret string

	// AWSPlatformAccountID is the 12-digit account that customers' IAM
	// trust policies will allow sts:AssumeRole from — i.e. the AWS account
	// running this Spacefleet instance. Hosted Spacefleet sets it to its
	// own; self-hosters set it to whichever account they run in.
	//
	// AWSCFNTemplateURL points at a publicly fetchable CloudFormation
	// template that creates the integration role. Hosted Spacefleet
	// publishes one to its CDN; self-hosters host their own (or fork ours).
	// Both must be configured for the AWS onboarding flow to be enabled.
	AWSPlatformAccountID string
	AWSCFNTemplateURL    string

	// PublicURL is the externally-reachable base URL for this Spacefleet
	// install. Builder Fargate tasks POST webhook events back here; in
	// development this typically points at an ngrok-style tunnel.
	PublicURL string

	// State backend for Pulumi. One bucket per Spacefleet installation;
	// per-cloud-account KMS keys live inside it. Required to run any
	// build (the worker bails at startup if these are unset and a build
	// is attempted) but not to serve the HTTP API alone.
	StateBucket       string
	StateBucketRegion string
	StateKMSKeyARN    string

	// BuilderImage is the digest-pinned reference to the builder image
	// that Fargate tasks pull. The release pipeline injects the default
	// at link time via -ldflags; this env var overrides for local dev.
	BuilderImage string

	// WorkerConcurrency is the maximum number of River jobs the worker
	// process executes in parallel. Default 4.
	WorkerConcurrency int

	// BuildTimeout is the absolute hard ceiling on a single build, after
	// which the worker StopTasks the Fargate task and marks the build
	// failed. Default 60m.
	BuildTimeout time.Duration
}

// DefaultBuilderImage is the builder image reference the binary ships
// with. Release builds overwrite this via -ldflags
// "-X github.com/spacefleet/app/lib/config.DefaultBuilderImage=...". A
// stock `go build` leaves it empty, so the worker either picks up
// SPACEFLEET_BUILDER_IMAGE or BuildPipelineConfigured() returns false.
var DefaultBuilderImage = ""

func Load() (*Config, error) {
	cfg := &Config{
		Addr:                   getenv("ADDR", ":8080"),
		Env:                    getenv("ENV", "development"),
		ClerkPublishableKey:    os.Getenv("CLERK_PUBLISHABLE_KEY"),
		ClerkSecretKey:         os.Getenv("CLERK_SECRET_KEY"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		RedisURL:               os.Getenv("REDIS_URL"),
		GitHubAppSlug:          os.Getenv("GITHUB_APP_SLUG"),
		GitHubAppWebhookSecret: os.Getenv("GITHUB_APP_WEBHOOK_SECRET"),
		AWSPlatformAccountID:   os.Getenv("AWS_PLATFORM_ACCOUNT_ID"),
		AWSCFNTemplateURL:      os.Getenv("AWS_CFN_TEMPLATE_URL"),
		PublicURL:              os.Getenv("SPACEFLEET_PUBLIC_URL"),
		StateBucket:            os.Getenv("SPACEFLEET_STATE_BUCKET"),
		StateBucketRegion:      os.Getenv("SPACEFLEET_STATE_BUCKET_REGION"),
		StateKMSKeyARN:         os.Getenv("SPACEFLEET_STATE_KMS_KEY_ARN"),
		BuilderImage:           getenv("SPACEFLEET_BUILDER_IMAGE", DefaultBuilderImage),
	}

	if v := os.Getenv("GITHUB_APP_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_ID: %w", err)
		}
		cfg.GitHubAppID = id
	}

	concurrency, err := parsePositiveInt("SPACEFLEET_WORKER_CONCURRENCY", 4)
	if err != nil {
		return nil, err
	}
	cfg.WorkerConcurrency = concurrency

	timeout, err := parseDuration("SPACEFLEET_BUILD_TIMEOUT", 60*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg.BuildTimeout = timeout

	pem, err := loadGitHubPrivateKey()
	if err != nil {
		return nil, err
	}
	cfg.GitHubAppPrivateKey = pem

	return cfg, nil
}

// loadGitHubPrivateKey resolves the App's PEM. Two ways to set it:
// GITHUB_APP_PRIVATE_KEY contains the literal PEM (multi-line env var),
// GITHUB_APP_PRIVATE_KEY_PATH points at a file on disk. Path wins if both
// are set — secrets-on-disk is the more common deployment shape.
func loadGitHubPrivateKey() ([]byte, error) {
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_PATH: %w", err)
		}
		return data, nil
	}
	if pem := os.Getenv("GITHUB_APP_PRIVATE_KEY"); pem != "" {
		return []byte(pem), nil
	}
	return nil, nil
}

// GitHubAppConfigured reports whether enough is present to drive the
// GitHub App flow. Routes that depend on it fail closed with a clear
// error instead of crashing or silently accepting requests.
func (c *Config) GitHubAppConfigured() bool {
	return c.GitHubAppID != 0 && c.GitHubAppSlug != "" && len(c.GitHubAppPrivateKey) > 0
}

// AWSConfigured reports whether the AWS onboarding flow can be served.
// Both the platform account ID and the CFN template URL are required —
// the URL embeds the account ID into the customer's stack, and without
// either the Quick Create link is meaningless.
func (c *Config) AWSConfigured() bool {
	return c.AWSPlatformAccountID != "" && c.AWSCFNTemplateURL != ""
}

// StateBackendConfigured reports whether the build pipeline's Pulumi
// state backend has been wired up. The HTTP server runs without it, but
// the worker refuses to dispatch a build until all three are set.
func (c *Config) StateBackendConfigured() bool {
	return c.StateBucket != "" && c.StateBucketRegion != "" && c.StateKMSKeyARN != ""
}

// BuildPipelineConfigured reports whether the worker has every value it
// needs to actually run a build end-to-end (state backend, builder
// image, public URL for webhooks).
func (c *Config) BuildPipelineConfigured() bool {
	return c.StateBackendConfigured() && c.BuilderImage != "" && c.PublicURL != ""
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parsePositiveInt reads an integer env var, falling back to fallback
// when unset. Zero or negative values are rejected so a typo doesn't
// silently disable the worker.
func parsePositiveInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: must be > 0, got %d", key, v)
	}
	return v, nil
}

// parseDuration reads a duration env var (Go duration syntax: 60m, 1h),
// falling back to fallback when unset.
func parseDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: must be > 0, got %s", key, v)
	}
	return v, nil
}
