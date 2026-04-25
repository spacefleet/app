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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
