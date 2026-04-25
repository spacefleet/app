package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// GithubInstallation records a GitHub App installation an org has connected
// to Spacefleet. Scope is the Clerk org slug; we don't store any GitHub
// secrets here — installation tokens are minted on demand from the App
// private key plus this row's installation_id.
//
// Type name is "Github" (not "GitHub") so ent's snake_case rule produces
// `github_installations` instead of splitting at the embedded H.
type GithubInstallation struct {
	ent.Schema
}

func (GithubInstallation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("org_slug").NotEmpty(),
		field.Int64("installation_id").Unique(),
		field.String("account_login").NotEmpty(),
		field.String("account_type").NotEmpty(),
		field.Int64("account_id"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("suspended_at").Optional().Nillable(),
	}
}

func (GithubInstallation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("org_slug"),
	}
}
