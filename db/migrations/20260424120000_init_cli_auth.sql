-- Initial schema for CLI authentication.
--
-- cli_tokens holds long-lived bearer credentials for the Spacefleet CLI.
-- Only sha256(token) is stored; plaintext is returned once at exchange.
-- cli_auth_codes is the short-lived PKCE grant created when a user approves
-- a CLI from the browser; consumed exactly once at /api/cli/auth/exchange.

CREATE TABLE cli_tokens (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX cli_tokens_user_id ON cli_tokens (user_id);

CREATE TABLE cli_auth_codes (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    code_hash BYTEA NOT NULL UNIQUE,
    challenge TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);
