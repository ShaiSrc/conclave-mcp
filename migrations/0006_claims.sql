-- Migration 0006: path claims for coding agents sharing a room.
-- See docs/superpowers/specs/2026-11-07-claims-init-hooks-design.md.

CREATE TABLE IF NOT EXISTS claims (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  room_id    TEXT    NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  key_id     INTEGER NOT NULL REFERENCES api_keys(id),
  path       TEXT    NOT NULL,
  auto       INTEGER NOT NULL DEFAULT 0,
  note       TEXT,
  created_at TEXT    NOT NULL,
  expires_at INTEGER NOT NULL, -- unix seconds; active while > now
  UNIQUE (room_id, key_id, path)
);

CREATE INDEX IF NOT EXISTS idx_claims_room_expires ON claims(room_id, expires_at);
