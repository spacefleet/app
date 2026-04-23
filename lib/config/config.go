package config

import "os"

type Config struct {
	Addr                string
	Env                 string
	ClerkPublishableKey string
	ClerkSecretKey      string
}

func Load() *Config {
	return &Config{
		Addr:                getenv("ADDR", ":8080"),
		Env:                 getenv("ENV", "development"),
		ClerkPublishableKey: os.Getenv("CLERK_PUBLISHABLE_KEY"),
		ClerkSecretKey:      os.Getenv("CLERK_SECRET_KEY"),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
