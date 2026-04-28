package config

import (
	"testing"
	"time"
)

// TestLoadDefaults verifies the values Load picks when no env is set.
// The HTTP server still has to come up cleanly with an unfilled config —
// the build pipeline is opt-in, not required.
func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.Env != "development" {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.WorkerConcurrency != 4 {
		t.Errorf("WorkerConcurrency = %d, want 4", cfg.WorkerConcurrency)
	}
	if cfg.BuildTimeout != 60*time.Minute {
		t.Errorf("BuildTimeout = %s, want 1h", cfg.BuildTimeout)
	}
	if cfg.StateBackendConfigured() {
		t.Error("StateBackendConfigured returned true with no state vars")
	}
	if cfg.BuildPipelineConfigured() {
		t.Error("BuildPipelineConfigured returned true with no state vars")
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SPACEFLEET_PUBLIC_URL", "https://app.example.com")
	t.Setenv("SPACEFLEET_STATE_BUCKET", "spacefleet-state")
	t.Setenv("SPACEFLEET_STATE_BUCKET_REGION", "us-west-2")
	t.Setenv("SPACEFLEET_STATE_KMS_KEY_ARN", "arn:aws:kms:us-west-2:111122223333:key/abcd")
	t.Setenv("SPACEFLEET_BUILDER_IMAGE", "ghcr.io/spacefleet/builder:test@sha256:deadbeef")
	t.Setenv("SPACEFLEET_WORKER_CONCURRENCY", "8")
	t.Setenv("SPACEFLEET_BUILD_TIMEOUT", "30m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicURL != "https://app.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if !cfg.StateBackendConfigured() {
		t.Error("StateBackendConfigured returned false with all three vars set")
	}
	if !cfg.BuildPipelineConfigured() {
		t.Error("BuildPipelineConfigured returned false with full env")
	}
	if cfg.WorkerConcurrency != 8 {
		t.Errorf("WorkerConcurrency = %d, want 8", cfg.WorkerConcurrency)
	}
	if cfg.BuildTimeout != 30*time.Minute {
		t.Errorf("BuildTimeout = %s, want 30m", cfg.BuildTimeout)
	}
}

func TestLoadRejectsBadConcurrency(t *testing.T) {
	for _, v := range []string{"0", "-1", "abc"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SPACEFLEET_WORKER_CONCURRENCY", v)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for concurrency=%q", v)
			}
		})
	}
}

func TestLoadRejectsBadTimeout(t *testing.T) {
	for _, v := range []string{"0s", "-5m", "not-a-duration"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SPACEFLEET_BUILD_TIMEOUT", v)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for timeout=%q", v)
			}
		})
	}
}

// TestStateBackendConfigured asserts the all-or-nothing rule: a stack
// missing any of the three is treated as not configured. The worker
// surfaces a clear error rather than half-running.
func TestStateBackendConfigured(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
		region string
		kms    string
		want   bool
	}{
		{"none", "", "", "", false},
		{"bucket only", "b", "", "", false},
		{"missing kms", "b", "us-east-1", "", false},
		{"all three", "b", "us-east-1", "arn:...", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{StateBucket: tc.bucket, StateBucketRegion: tc.region, StateKMSKeyARN: tc.kms}
			if got := c.StateBackendConfigured(); got != tc.want {
				t.Errorf("StateBackendConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuilderImageOverridesDefault confirms the env var beats the
// link-time default. (We can't test the link-time default itself
// without rebuilding the binary, but we can confirm the precedence.)
func TestBuilderImageOverridesDefault(t *testing.T) {
	clearEnv(t)
	saved := DefaultBuilderImage
	t.Cleanup(func() { DefaultBuilderImage = saved })
	DefaultBuilderImage = "ghcr.io/spacefleet/builder:linktime@sha256:abc"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BuilderImage != "ghcr.io/spacefleet/builder:linktime@sha256:abc" {
		t.Errorf("BuilderImage = %q, want link-time default", cfg.BuilderImage)
	}

	t.Setenv("SPACEFLEET_BUILDER_IMAGE", "ghcr.io/spacefleet/builder:override@sha256:def")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BuilderImage != "ghcr.io/spacefleet/builder:override@sha256:def" {
		t.Errorf("BuilderImage = %q, want env override", cfg.BuilderImage)
	}
}

// clearEnv unsets every key Load reads so tests start from a known
// baseline. Setenv with t.Setenv inside tests will repopulate as needed.
func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"ADDR",
		"ENV",
		"CLERK_PUBLISHABLE_KEY",
		"CLERK_SECRET_KEY",
		"DATABASE_URL",
		"REDIS_URL",
		"GITHUB_APP_ID",
		"GITHUB_APP_SLUG",
		"GITHUB_APP_WEBHOOK_SECRET",
		"GITHUB_APP_PRIVATE_KEY",
		"GITHUB_APP_PRIVATE_KEY_PATH",
		"AWS_PLATFORM_ACCOUNT_ID",
		"AWS_CFN_TEMPLATE_URL",
		"SPACEFLEET_PUBLIC_URL",
		"SPACEFLEET_STATE_BUCKET",
		"SPACEFLEET_STATE_BUCKET_REGION",
		"SPACEFLEET_STATE_KMS_KEY_ARN",
		"SPACEFLEET_BUILDER_IMAGE",
		"SPACEFLEET_WORKER_CONCURRENCY",
		"SPACEFLEET_BUILD_TIMEOUT",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}
