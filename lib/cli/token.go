package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/clitoken"
)

const (
	// TokenPrefix identifies CLI tokens on the wire so RequireAuth can route
	// them past Clerk verification. The `sf_` prefix has no security value —
	// the 256-bit random tail does.
	TokenPrefix = "sf_"
	// TokenTTL is the fixed lifetime of a freshly issued CLI token.
	TokenTTL = 90 * 24 * time.Hour
)

// IssueToken mints a fresh CLI token for userID, persists its sha256 hash,
// and returns the plaintext to the caller. The plaintext is never stored.
func (s *Service) IssueToken(ctx context.Context, userID, name string) (string, *ent.CLIToken, error) {
	plaintext, hash, err := newToken()
	if err != nil {
		return "", nil, err
	}
	t, err := s.ent.CLIToken.Create().
		SetUserID(userID).
		SetTokenHash(hash).
		SetName(name).
		SetExpiresAt(time.Now().Add(TokenTTL)).
		Save(ctx)
	if err != nil {
		return "", nil, err
	}
	return plaintext, t, nil
}

// VerifyToken resolves a bearer token to its stored CLIToken row, rejecting
// unknown, expired, or revoked tokens. On success it bumps last_used_at —
// errors from that write are swallowed so auth doesn't fail for observability.
func (s *Service) VerifyToken(ctx context.Context, plaintext string) (*ent.CLIToken, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return nil, ErrInvalidToken
	}
	hash := sha256Sum(plaintext)
	t, err := s.ent.CLIToken.Query().Where(clitoken.TokenHashEQ(hash)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	now := time.Now()
	if !t.ExpiresAt.After(now) {
		return nil, ErrTokenExpired
	}
	if t.RevokedAt != nil {
		return nil, ErrTokenRevoked
	}
	_, _ = s.ent.CLIToken.UpdateOneID(t.ID).SetLastUsedAt(now).Save(ctx)
	return t, nil
}

// ListTokens returns every non-deleted CLIToken for userID, including
// revoked ones so the UI can show history. Sorted newest first.
func (s *Service) ListTokens(ctx context.Context, userID string) ([]*ent.CLIToken, error) {
	return s.ent.CLIToken.Query().
		Where(clitoken.UserIDEQ(userID)).
		Order(ent.Desc(clitoken.FieldCreatedAt)).
		All(ctx)
}

// RevokeToken marks id as revoked if and only if it belongs to userID.
// Already-revoked tokens are a no-op (idempotent).
func (s *Service) RevokeToken(ctx context.Context, userID string, id uuid.UUID) error {
	t, err := s.ent.CLIToken.Query().
		Where(clitoken.IDEQ(id), clitoken.UserIDEQ(userID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrInvalidToken
		}
		return err
	}
	if t.RevokedAt != nil {
		return nil
	}
	return s.ent.CLIToken.UpdateOneID(t.ID).SetRevokedAt(time.Now()).Exec(ctx)
}

func newToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", nil, errors.New("generate token: " + err.Error())
	}
	plaintext = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(plaintext))
	return plaintext, h[:], nil
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
