package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Build is one execution of the build pipeline against an app + commit.
//
// `stages` is an append-only JSONB array of stage events. The current
// stage is reconstructable as "the latest entry whose status is
// 'running'." We deliberately don't denormalize the current stage onto
// a column — too easy to drift from the array.
//
// `webhook_secret` stores the plaintext per-build HMAC key used to
// authenticate webhook events from the in-cloud builder. It's marked
// Sensitive() so ent omits it from logs, queries can still produce it
// for HMAC verification, and it's deleted with the row when the build
// is gone. (See BUILD_PIPELINE.md > "Internal webhook" for the
// reasoning behind plaintext-with-Sensitive over hash-at-rest.)
type Build struct {
	ent.Schema
}

func (Build) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.UUID("app_id", uuid.UUID{}),
		field.String("source_ref").NotEmpty(),
		field.String("source_sha").Optional(),
		field.String("status").NotEmpty().Default("queued"),
		field.JSON("stages", []StageEvent{}).Default([]StageEvent{}),
		field.String("image_uri").Optional(),
		field.String("image_digest").Optional(),
		field.String("fargate_task_arn").Optional(),
		field.String("log_group").Optional(),
		field.String("log_stream").Optional(),
		field.String("webhook_secret").NotEmpty().Sensitive(),
		field.String("error_message").Optional(),
		field.String("created_by").NotEmpty(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("started_at").Optional().Nillable(),
		field.Time("ended_at").Optional().Nillable(),
	}
}

func (Build) Indexes() []ent.Index {
	return []ent.Index{
		// Listing builds by app, newest first, is the dominant
		// read pattern. Same column order as the user-facing UI.
		index.Fields("app_id", "created_at"),
		// Reattach-on-startup needs to find every running build
		// across all apps; index status to keep that scan cheap.
		index.Fields("status"),
	}
}

// StageEvent is one entry in Build.stages. Order matches the wire
// shape so JSON marshal/unmarshal is symmetric. `Data` is intentionally
// loose — different stage names carry different payloads (image_uri,
// error message, etc.) and we don't want to fight the schema as the
// builder evolves.
type StageEvent struct {
	Name   string         `json:"name"`
	Status string         `json:"status"`
	At     time.Time      `json:"at"`
	Data   map[string]any `json:"data,omitempty"`
}
