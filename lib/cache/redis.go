// Package cache exposes a shared *redis.Client.
package cache

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Open parses a redis:// URL and returns a ready client. Callers own the
// client and must Close it at shutdown.
func Open(ctx context.Context, url string) (*redis.Client, error) {
	if url == "" {
		return nil, fmt.Errorf("REDIS_URL is empty")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return c, nil
}
