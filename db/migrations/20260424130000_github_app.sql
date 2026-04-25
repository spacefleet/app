-- Schema for the GitHub App integration.
--
-- github_installations records a GitHub App installation that an org has
-- connected to Spacefleet. Scope is the Clerk org slug. We never store
-- access tokens here — they're minted on demand from the App private key
-- and the row's installation_id (1h TTL, see lib/github).
--
-- github_install_states is the short-lived CSRF token created when a user
-- initiates an install. Bound to the originating user so a stolen state
-- can't redirect a victim's install into the attacker's org.

CREATE TABLE github_installations (
    id UUID PRIMARY KEY,
    org_slug TEXT NOT NULL,
    installation_id BIGINT NOT NULL UNIQUE,
    account_login TEXT NOT NULL,
    account_type TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    suspended_at TIMESTAMPTZ
);

CREATE INDEX github_installations_org_slug ON github_installations (org_slug);

CREATE TABLE github_install_states (
    id UUID PRIMARY KEY,
    state_hash BYTEA NOT NULL UNIQUE,
    org_slug TEXT NOT NULL,
    user_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);
