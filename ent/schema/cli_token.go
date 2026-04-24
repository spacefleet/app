package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CLIToken is a long-lived bearer credential issued to a user's CLI.
// Plaintext is generated once at exchange time and never stored — only the
// sha256 of the token is persisted in TokenHash.
type CLIToken struct {
	ent.Schema
}

func (CLIToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("user_id").NotEmpty(),
		field.Bytes("token_hash").MaxLen(32).NotEmpty().Unique(),
		field.String("name").NotEmpty(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("expires_at"),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
	}
}

func (CLIToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
	}
}
