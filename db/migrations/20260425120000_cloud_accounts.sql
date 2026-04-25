-- Cloud account onboarding (AWS today; provider column reserved for
-- GCP/Azure later without a schema change).
--
-- See lib/aws/service.go for the status state machine. The (org_slug,
-- provider, label) uniqueness mirrors the ent index — labels are
-- per-org human-friendly identifiers, so collisions inside an org would
-- be a UI nightmare. We don't enforce uniqueness on (org_slug, provider,
-- account_id) at the DB level because account_id is empty during the
-- pending phase; the service layer rejects duplicate completed accounts
-- as a query-time guard.

CREATE TABLE cloud_accounts (
    id UUID PRIMARY KEY,
    org_slug TEXT NOT NULL,
    provider TEXT NOT NULL DEFAULT 'aws',
    label TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    role_arn TEXT NOT NULL DEFAULT '',
    external_id TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    last_verified_at TIMESTAMPTZ,
    last_verification_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX cloud_accounts_org_slug ON cloud_accounts (org_slug);
CREATE UNIQUE INDEX cloud_accounts_label_per_org ON cloud_accounts (org_slug, provider, label);
