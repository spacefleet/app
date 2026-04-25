package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CloudAccount is one cloud-provider account an org has connected to
// Spacefleet. Today only AWS is supported (provider="aws"); the column
// exists so the same table accommodates GCP/Azure later without a
// schema migration.
//
// External ID is the cross-account confused-deputy mitigation: it goes
// into the customer's IAM trust policy at stack-launch time and is
// presented on every sts:AssumeRole. Treated as a secret — surfaced once
// at start, not listed afterwards.
//
// Status transitions (no enum on the column to avoid migration churn
// when we add states):
//
//	pending    — row exists, external ID issued, customer hasn't pasted
//	             a role ARN yet
//	connected  — role ARN persisted and verification probe succeeded
//	error      — verification failed (last_verification_error has detail)
//	disabled   — operator-paused; we keep the row but stop reconciling
type CloudAccount struct {
	ent.Schema
}

func (CloudAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("org_slug").NotEmpty(),
		field.String("provider").NotEmpty().Default("aws"),
		field.String("label").NotEmpty(),
		// Provider account identifier. For AWS this is the 12-digit
		// account ID; empty until completion since we discover it from
		// the role ARN the customer pastes back.
		field.String("account_id").Optional(),
		field.String("role_arn").Optional(),
		// External ID is shown once and presented on every AssumeRole.
		// Stored at full length (base32 of 32 random bytes ~52 chars).
		field.String("external_id").NotEmpty().Sensitive(),
		field.String("region").Optional(),
		field.String("status").NotEmpty().Default("pending"),
		field.Time("last_verified_at").Optional().Nillable(),
		field.String("last_verification_error").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (CloudAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("org_slug"),
		// Once an account is connected, (org_slug, provider, account_id)
		// is unique. account_id is empty during pending so the unique
		// index is conditional — relax to a plain query-time guard
		// rather than a partial index, which ent's portable schema
		// doesn't model cleanly.
		index.Fields("org_slug", "provider", "label").Unique(),
	}
}
