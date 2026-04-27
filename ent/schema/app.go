package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// App is a deployable unit owned by a Clerk org. One repo, one
// Dockerfile, one cloud-account binding. The slug is generated from
// the name on create and is immutable for the app's lifetime — rename
// support comes later, after we know which surfaces would need to
// follow a slug change.
//
// Foreign keys to cloud_accounts and github_installations are enforced
// at the SQL layer (see the migration). We don't model them as ent
// edges in v1 because we never traverse the relationship inside ent —
// the service layer looks both rows up directly to validate ownership
// before insertion, and the only cross-row constraint we care about is
// "row exists in same org," which a join wouldn't simplify.
//
// `default_branch` is captured from GitHub at create time and not
// auto-refreshed. Builds default to this ref but the user can override
// per build, so a stale default branch only affects the convenience
// path — not correctness.
type App struct {
	ent.Schema
}

func (App) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("org_slug").NotEmpty(),
		field.String("name").NotEmpty().MaxLen(200),
		field.String("slug").NotEmpty().MaxLen(50),
		field.UUID("cloud_account_id", uuid.UUID{}),
		field.UUID("github_installation_id", uuid.UUID{}),
		field.String("github_repo_full_name").NotEmpty(),
		field.String("default_branch").NotEmpty(),
		field.String("created_by").NotEmpty(),
		field.Time("deleting_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (App) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("org_slug"),
		index.Fields("org_slug", "slug").Unique(),
	}
}
