-- Migration 0011: federation.
--
-- federation_keys holds the server's Ed25519 keys, private key included:
-- the database is already the secret store (mode 0600). The newest row
-- without retired_at is the current key. federation_peers is the peer
-- policy, one row per domain, with the peer's pinned public key (NULL
-- for a domain blocked before it was ever seen). federation_nonces
-- remembers each signed request's nonce until it expires, so a restart
-- does not reopen the replay window. Timestamps use the fixed-width
-- storage.FormatTS form.

CREATE TABLE federation_keys (
    key_id        TEXT PRIMARY KEY,
    public_key    BLOB NOT NULL,
    private_key   BLOB NOT NULL,
    created_at    TEXT NOT NULL,
    retired_at    TEXT
);

CREATE TABLE federation_peers (
    domain        TEXT PRIMARY KEY,
    status        TEXT NOT NULL CHECK (status IN ('allowed','blocked','seen')),
    fingerprint   TEXT NOT NULL DEFAULT '',
    public_key    BLOB,
    note          TEXT NOT NULL DEFAULT '',
    added_at      TEXT NOT NULL,
    last_seen_at  TEXT
);

CREATE TABLE federation_nonces (
    origin        TEXT NOT NULL,
    nonce         TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    PRIMARY KEY (origin, nonce)
);

CREATE INDEX federation_nonces_expires ON federation_nonces(expires_at);
