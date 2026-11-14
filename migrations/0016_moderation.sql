-- Migration 0016: moderation and operations.
--
-- federation_keys.endorsement is, on a key made by a rotation, its
-- endorsed_by object in the server document as JSON {key_id, sig}: the
-- previous key's signature of this one. federation_peer_keys is each
-- peer's key history: the keys it was pinned to before, kept so author
-- signatures made with them still verify and a peer never goes back to
-- one. federation_peer_limits holds the operator's per-peer overrides;
-- NULL is the default, 0 refuses. blocked_bots are single remote bots
-- the operator blocked; created_by is the admin key. federation_peers.
-- last_out_at is the last successful outbound request to the peer.
-- api_keys.remote_via is the host that introduced a stand-in, NULL for
-- one made by an invite or follow of ours. Timestamps use the
-- fixed-width storage.FormatTS form.

ALTER TABLE federation_keys ADD COLUMN endorsement TEXT;
ALTER TABLE federation_peers ADD COLUMN last_out_at TEXT;
ALTER TABLE api_keys ADD COLUMN remote_via TEXT;
CREATE INDEX api_keys_remote_via ON api_keys(remote_via) WHERE remote_via IS NOT NULL;

CREATE TABLE federation_peer_keys (
    domain        TEXT NOT NULL,
    key_id        TEXT NOT NULL,
    public_key    BLOB NOT NULL,
    fingerprint   TEXT NOT NULL,
    pinned_at     TEXT NOT NULL,
    retired_at    TEXT NOT NULL,
    PRIMARY KEY (domain, key_id)
);

CREATE TABLE federation_peer_limits (
    domain                    TEXT PRIMARY KEY,
    requests_per_minute       INTEGER,
    dm_invites_per_hour       INTEGER,
    follow_requests_per_hour  INTEGER
);

CREATE TABLE blocked_bots (
    address       TEXT PRIMARY KEY,
    note          TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    created_by    INTEGER
);
