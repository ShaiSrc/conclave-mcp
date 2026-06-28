-- Migration 0004: trust_origin classification.
-- Implements SPEC-0001 Q11 + ADR-0008 (revised) + ADR-0009.
--
-- The trust_origin attribute lets operators distinguish
-- "internal" keys (own infrastructure, personal partners)
-- from "external" keys (invited collaborators whose agent
-- we do not trust by default). The flag is a client-side
-- hint plus an audit-trail marker; the server is a dumb
-- pipe and does not enforce any behaviour on the basis
-- of it.
--
-- Behavioural notes (see ADR-0008 §1 for the full decision):
--   * Set at api_keys creation time, defaults to 'internal'.
--   * Denormalised onto every message the key writes, at
--     send time, via a JOIN in the messages.send handler.
--   * Reclassification via `keys update --trust-origin=...`
--     is a v0.3 follow-up; v0.2 freezes the value at
--     creation.
--   * Existing rows are backfilled with the safe default
--     ('internal'). All pre-migration keys were created
--     by server startup or single-operator workflow.

-- 1. api_keys.trust_origin
ALTER TABLE api_keys
  ADD COLUMN trust_origin TEXT NOT NULL DEFAULT 'internal';

-- The CHECK constraint must be added as a separate ALTER
-- because SQLite does not support adding a CHECK directly
-- in the ADD COLUMN clause when DEFAULT is also present.
-- See https://www.sqlite.org/lang_altertable.html
-- (the "limitations" section).
--
-- We use a savepoint + table-recreate pattern to add the
-- CHECK, because SQLite cannot add a CHECK constraint to
-- an existing table either. The pattern: rename old, build
-- new with CHECK, copy data, drop old.
SAVEPOINT add_api_keys_check;
CREATE TABLE api_keys_new (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  label        TEXT NOT NULL UNIQUE,
  hash         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  revoked_at   TEXT NULL,
  last_used_at TEXT NULL,
  trust_origin TEXT NOT NULL DEFAULT 'internal'
    CHECK (trust_origin IN ('internal','external'))
);
INSERT INTO api_keys_new
  SELECT id, label, hash, prefix, created_at, revoked_at, last_used_at, trust_origin
  FROM api_keys;
DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;
CREATE UNIQUE INDEX idx_api_keys_prefix     ON api_keys(prefix);
CREATE INDEX idx_api_keys_label        ON api_keys(label);
CREATE INDEX idx_api_keys_trust_origin ON api_keys(trust_origin);
RELEASE add_api_keys_check;

-- 2. messages.trust_origin
ALTER TABLE messages
  ADD COLUMN trust_origin TEXT NOT NULL DEFAULT 'internal';

SAVEPOINT add_messages_check;
CREATE TABLE messages_new (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  room_id       TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  author_key_id INTEGER NOT NULL REFERENCES api_keys(id),
  ts            TEXT NOT NULL,
  content       TEXT NOT NULL,
  content_type  TEXT NOT NULL,
  message_type  TEXT NOT NULL DEFAULT 'system'
    CHECK (message_type IN ('question','answer','decision','handoff',
      'status_update','task_assignment','alert','system')),
  metadata      TEXT NULL,
  deleted_at    TEXT NULL,
  trust_origin  TEXT NOT NULL DEFAULT 'internal'
    CHECK (trust_origin IN ('internal','external'))
);
-- Backfill: copy author_key.trust_origin onto each existing
-- message so the denormalisation is honest from minute one.
INSERT INTO messages_new
  (id, room_id, author_key_id, ts, content, content_type,
   message_type, metadata, deleted_at, trust_origin)
SELECT
  m.id, m.room_id, m.author_key_id, m.ts, m.content, m.content_type,
  COALESCE(m.message_type, 'system'),
  m.metadata, m.deleted_at,
  COALESCE(k.trust_origin, 'internal')
FROM messages m
LEFT JOIN api_keys k ON k.id = m.author_key_id;
DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;

-- Re-create the standard indices.
CREATE INDEX idx_messages_room_ts       ON messages(room_id, ts);
CREATE INDEX idx_messages_author        ON messages(author_key_id);
CREATE INDEX idx_messages_room_deleted  ON messages(room_id, deleted_at);
-- New partial index for the trust_origin filter (see ADR-0008 §1.2).
CREATE INDEX idx_messages_room_trust_ts
  ON messages(room_id, trust_origin, ts)
  WHERE deleted_at IS NULL;
RELEASE add_messages_check;
