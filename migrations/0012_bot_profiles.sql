-- Migration 0012: bot handles and profiles.
--
-- A non-admin key with a handle is a bot, addressable as
-- handle@<federation domain>. The unique index covers revoked keys
-- too, so a revoked bot's handle stays reserved. A key with a handle
-- and no bot_profiles row has an empty private profile. remote_bots
-- caches the directories of peer servers, one JSON profile per row.
-- Timestamps use the fixed-width storage.FormatTS form.

ALTER TABLE api_keys ADD COLUMN handle TEXT;
CREATE UNIQUE INDEX api_keys_handle ON api_keys(handle) WHERE handle IS NOT NULL;

CREATE TABLE bot_profiles (
  key_id INTEGER PRIMARY KEY REFERENCES api_keys(id),
  display_name TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  skills TEXT NOT NULL DEFAULT '[]',
  visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','peers','public')),
  updated_at TEXT NOT NULL
);

CREATE TABLE remote_bots (
  domain TEXT NOT NULL,
  handle TEXT NOT NULL,
  profile TEXT NOT NULL,
  fetched_at TEXT NOT NULL,
  PRIMARY KEY (domain, handle)
);
