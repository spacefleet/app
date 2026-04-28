-- Apps and builds. Both tables ship in one migration so the apps
-- DELETE path can cascade-delete builds without a follow-up migration.
--
-- Foreign keys go through ON DELETE RESTRICT on app dependencies so
-- accidentally dropping a connected GitHub installation or AWS account
-- doesn't silently orphan apps. Build → app cascades on delete because
-- builds are owned wholly by their app.
--
-- Slug uniqueness is per (org_slug, slug). Reserved slugs (`new`,
-- `settings`, `builds`, `delete`, `api`, `admin`) are enforced by the
-- service layer rather than a check constraint — we want a friendlier
-- error than "constraint violated" and the set may grow over time.

CREATE TABLE apps (
    id UUID PRIMARY KEY,
    org_slug TEXT NOT NULL,
    name TEXT NOT NULL,
    slug TEXT NOT NULL,
    cloud_account_id UUID NOT NULL REFERENCES cloud_accounts(id) ON DELETE RESTRICT,
    github_installation_id UUID NOT NULL REFERENCES github_installations(id) ON DELETE RESTRICT,
    github_repo_full_name TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    created_by TEXT NOT NULL,
    deleting_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX apps_org_slug ON apps (org_slug);
CREATE UNIQUE INDEX apps_slug_per_org ON apps (org_slug, slug);

CREATE TABLE builds (
    id UUID PRIMARY KEY,
    app_id UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    source_ref TEXT NOT NULL,
    source_sha TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued',
    stages JSONB NOT NULL DEFAULT '[]'::jsonb,
    image_uri TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    fargate_task_arn TEXT NOT NULL DEFAULT '',
    log_group TEXT NOT NULL DEFAULT '',
    log_stream TEXT NOT NULL DEFAULT '',
    webhook_secret TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ
);

-- Listing builds by app, newest first (UI dominant read).
CREATE INDEX builds_app_id_created_at ON builds (app_id, created_at);

-- Reattach-on-startup walks every running build across all apps; an
-- index on status keeps that scan cheap even at scale.
CREATE INDEX builds_status ON builds (status);
