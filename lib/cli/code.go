package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/cliauthcode"
)

// CodeTTL is how long an approval code is valid. Long enough for a user to
// complete a round-trip through the browser, short enough that a leaked code
// is almost always dead on arrival.
const CodeTTL = 5 * time.Minute

// CreateCode records the user's approval grant (name + PKCE challenge) and
// returns a one-time code for the browser to redirect back to the CLI.
func (s *Service) CreateCode(ctx context.Context, userID, name, challenge string) (string, error) {
	plaintext, hash, err := newCode()
	if err != nil {
		return "", err
	}
	_, err = s.ent.CLIAuthCode.Create().
		SetUserID(userID).
		SetCodeHash(hash).
		SetChallenge(challenge).
		SetName(name).
		SetExpiresAt(time.Now().Add(CodeTTL)).
		Save(ctx)
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// ConsumeCode verifies the PKCE verifier and marks the code as used, all in
// one transaction to prevent double-consume races. Returns the consumed row
// so the caller can mint a token with the same user_id + name.
func (s *Service) ConsumeCode(ctx context.Context, code, verifier string) (*ent.CLIAuthCode, error) {
	tx, err := s.ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func() { _ = tx.Rollback() }

	hash := sha256Sum(code)
	c, err := tx.CLIAuthCode.Query().Where(cliauthcode.CodeHashEQ(hash)).Only(ctx)
	if err != nil {
		rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInvalidCode
		}
		return nil, err
	}

	now := time.Now()
	if c.ConsumedAt != nil {
		rollback()
		return nil, ErrCodeConsumed
	}
	if !c.ExpiresAt.After(now) {
		rollback()
		return nil, ErrCodeExpired
	}
	if computeChallenge(verifier) != c.Challenge {
		rollback()
		return nil, ErrPKCEMismatch
	}

	consumed, err := tx.CLIAuthCode.UpdateOneID(c.ID).SetConsumedAt(now).Save(ctx)
	if err != nil {
		rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return consumed, nil
}

// computeChallenge matches the value the CLI generated: base64url-nopadding
// of sha256(verifier).
func computeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func newCode() (plaintext string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", nil, err
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(plaintext))
	return plaintext, h[:], nil
}
