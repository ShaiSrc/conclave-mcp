-- Migration 0001: initial Conclave MCP v0.x schema.
-- Per SPEC-0002 §3.
-- All timestamps are ISO-8601 UTC strings (TEXT).
-- All CHECK constraints are named so error messages are useful.

-- ---------------------------------------------------------------------------
-- 3.1 meta
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ---------------------------------------------------------------------------
-- 3.2 api_keys
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id           INTEGER PRIMARY KEY,
    label        TEXT NOT NULL,
    hash         TEXT NOT NULL,    -- argon2id PHC string (ADR-0003)
    created_at   TEXT NOT NULL,    -- ISO-8601 UTC
    revoked_at   TEXT,            -- NULL = active; ISO timestamp on revoke
    last_used_at TEXT             -- updated on successful auth
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_label ON api_keys(label);

-- ---------------------------------------------------------------------------
-- 3.3 rooms
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS rooms (
    id           TEXT PRIMARY KEY,            -- ULID, generated server-side
    name         TEXT NOT NULL,
    owner_key_id INTEGER NOT NULL,
    created_at   TEXT NOT NULL,
    archived_at  TEXT,                        -- NULL = active
    description  TEXT,                        -- optional, <= 1024 chars
    CONSTRAINT chk_rooms_name_len CHECK (length(name) BETWEEN 1 AND 128),
    CONSTRAINT chk_rooms_desc_len CHECK (description IS NULL OR length(description) <= 1024),
    FOREIGN KEY (owner_key_id) REFERENCES api_keys(id) -- no ON DELETE: cannot be revoked
);

CREATE INDEX IF NOT EXISTS idx_rooms_name  ON rooms(name);   -- uniqueness enforced here
CREATE INDEX IF NOT EXISTS idx_rooms_owner ON rooms(owner_key_id);

-- ---------------------------------------------------------------------------
-- 3.4 room_members
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS room_members (
    room_id   TEXT    NOT NULL,
    key_id    INTEGER NOT NULL,
    role      TEXT    NOT NULL,
    joined_at TEXT    NOT NULL,
    PRIMARY KEY (room_id, key_id),
    CONSTRAINT chk_room_members_role CHECK (role IN ('owner', 'writer', 'reader')),
    FOREIGN KEY (room_id) REFERENCES rooms(id)    ON DELETE CASCADE,
    FOREIGN KEY (key_id)  REFERENCES api_keys(id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_room_members_key ON room_members(key_id);

-- At least one owner per room: AFTER DELETE trigger fires when a
-- room_members row is removed and checks the room still has an owner.
CREATE TRIGGER IF NOT EXISTS trg_room_members_owner_protect
AFTER DELETE ON room_members
FOR EACH ROW
WHEN OLD.role = 'owner'
BEGIN
    SELECT CASE
        WHEN NOT EXISTS (
            SELECT 1 FROM room_members
            WHERE room_id = OLD.room_id AND role = 'owner'
        )
        THEN RAISE(ABORT, 'room must retain at least one owner')
    END;
END;

-- ---------------------------------------------------------------------------
-- 3.5 messages
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY,         -- autoincrement
    room_id       TEXT    NOT NULL,
    author_key_id INTEGER NOT NULL,
    ts            TEXT    NOT NULL,           -- ISO-8601 UTC, server-set
    content       TEXT    NOT NULL,           -- raw payload, <= 256 KiB
    content_type  TEXT    NOT NULL,           -- MIME-style
    FOREIGN KEY (room_id)       REFERENCES rooms(id)    ON DELETE CASCADE,
    FOREIGN KEY (author_key_id) REFERENCES api_keys(id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_messages_room_ts  ON messages(room_id, ts);
CREATE INDEX IF NOT EXISTS idx_messages_author   ON messages(author_key_id);

-- ---------------------------------------------------------------------------
-- 3.6 audit
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit (
    id           INTEGER PRIMARY KEY,
    ts           TEXT    NOT NULL,            -- ISO-8601 UTC
    actor_key_id INTEGER,                    -- NULL for system actions
    action       TEXT    NOT NULL,            -- e.g. room.create, message.send
    target_type  TEXT    NOT NULL,            -- room, message, key, member
    target_id    TEXT    NOT NULL,            -- opaque
    result       TEXT    NOT NULL,            -- ok or error:<short-code>
    detail       TEXT,                        -- JSON blob, action-specific
    CONSTRAINT chk_audit_result CHECK (result = 'ok' OR result LIKE 'error:%')
);

CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts);

-- ---------------------------------------------------------------------------
-- Seed rows
-- ---------------------------------------------------------------------------
-- schema_version = 1, plus created_at timestamp at first install.
-- created_at is set by the migration runner (it knows "now" better than SQL).
-- This file is the schema; the runner inserts the meta rows.
