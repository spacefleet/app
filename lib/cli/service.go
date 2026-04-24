// Package cli owns persistence and business logic for the CLI auth flow:
// issuing and verifying long-lived CLI tokens, the short-lived auth codes
// that bridge the browser approval step, and the Redis-cached Clerk
// membership lookup used to authorize cross-org CLI requests.
package cli

import (
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/spacefleet/app/ent"
)

// Service bundles the dependencies the CLI auth flow needs at runtime.
type Service struct {
	ent   *ent.Client
	redis *redis.Client
}

func NewService(entClient *ent.Client, redisClient *redis.Client) *Service {
	return &Service{ent: entClient, redis: redisClient}
}

// Sentinel errors. Callers should distinguish "not found / bad credential"
// (return 401) from internal failures (return 500).
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
	ErrTokenRevoked = errors.New("token revoked")

	ErrInvalidCode  = errors.New("invalid code")
	ErrCodeExpired  = errors.New("code expired")
	ErrCodeConsumed = errors.New("code already consumed")
	ErrPKCEMismatch = errors.New("verifier does not match challenge")
)
