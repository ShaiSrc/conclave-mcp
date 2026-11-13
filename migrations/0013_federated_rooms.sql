-- Migration 0013: federated rooms.
--
-- A stand-in key represents a bot of another server: label = its
-- address name@domain, remote_domain = its server, trust origin
-- external, empty hash and prefix. The auth lookup skips every key
-- with remote_domain, so a stand-in never authenticates. The prefix
-- index becomes partial, so stand-ins (all with prefix '') do not
-- collide and none can take a local key's prefix.
--
-- A replica room has home_domain = its host. A replica's message keeps
-- the host's id in remote_id; origin_sig is the author server's
-- signature. fed_events is the host's per-room event log; fed_delivery
-- is the cursor of each member server. Timestamps use the fixed-width
-- storage.FormatTS form.

ALTER TABLE api_keys ADD COLUMN remote_domain TEXT;
DROP INDEX idx_api_keys_prefix;
CREATE UNIQUE INDEX idx_api_keys_prefix ON api_keys(prefix) WHERE remote_domain IS NULL;

ALTER TABLE rooms ADD COLUMN home_domain TEXT;

ALTER TABLE messages ADD COLUMN remote_id INTEGER;
ALTER TABLE messages ADD COLUMN origin_sig TEXT;
CREATE UNIQUE INDEX messages_room_remote_id ON messages(room_id, remote_id) WHERE remote_id IS NOT NULL;

CREATE TABLE fed_events (
    room_id    TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    type       TEXT NOT NULL CHECK (type IN ('message','member','member.remove','room')),
    payload    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (room_id, seq)
);

CREATE TABLE fed_delivery (
    room_id     TEXT NOT NULL,
    peer        TEXT NOT NULL,
    acked_seq   INTEGER NOT NULL DEFAULT 0,
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_try_at TEXT,
    last_error  TEXT NOT NULL DEFAULT '',
    paused      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (room_id, peer)
);
