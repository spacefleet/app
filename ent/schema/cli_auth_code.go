package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// CLIAuthCode is a short-lived, single-use grant created when a signed-in
// user approves a CLI. The CLI exchanges the code plus its PKCE verifier for
// a real token at /api/cli/auth/exchange.
type CLIAuthCode struct {
	ent.Schema
}

func (CLIAuthCode) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("user_id").NotEmpty(),
		field.Bytes("code_hash").MaxLen(32).NotEmpty().Unique(),
		field.String("challenge").NotEmpty(),
		field.String("name").NotEmpty(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("expires_at"),
		field.Time("consumed_at").Optional().Nillable(),
	}
}
