package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// GithubInstallState is the short-lived CSRF token created when a user
// initiates a GitHub App install. Persisting it server-side (rather than
// signing a stateless token) lets us bind the install to the originating
// user and one-shot it on completion.
//
// See github_installation.go for why the type spells it "Github".
type GithubInstallState struct {
	ent.Schema
}

func (GithubInstallState) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.Bytes("state_hash").MaxLen(32).NotEmpty().Unique(),
		field.String("org_slug").NotEmpty(),
		field.String("user_id").NotEmpty(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("expires_at"),
		field.Time("consumed_at").Optional().Nillable(),
	}
}
