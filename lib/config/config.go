package config

import (
	"fmt"
	"os"
	"strconv"
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
}

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
	}

	if v := os.Getenv("GITHUB_APP_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_ID: %w", err)
		}
		cfg.GitHubAppID = id
	}

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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
