-- Migration 0009: web admin UI (passkey login).
--
-- ui_credentials holds the operator's passkeys: public keys only, no
-- secret. flags is the authenticator-data flags byte from registration;
-- the WebAuthn library refuses a login whose backup-eligible flag differs
-- from the stored one. ui_sessions and ui_setup_tokens store only the
-- SHA-256 of their tokens, so a read of the database yields nothing
-- usable.

CREATE TABLE ui_credentials (
    id            TEXT PRIMARY KEY,      -- base64url credential id
    public_key    BLOB NOT NULL,
    sign_count    INTEGER NOT NULL DEFAULT 0,
    transports    TEXT NOT NULL DEFAULT '',
    flags         INTEGER NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    last_used_at  TEXT
);

CREATE TABLE ui_sessions (
    token_hash    TEXT PRIMARY KEY,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

CREATE TABLE ui_setup_tokens (
    token_hash    TEXT PRIMARY KEY,
    expires_at    TEXT NOT NULL,
    used_at       TEXT
);
