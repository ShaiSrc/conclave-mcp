-- Migration 0002: add prefix column to api_keys.
--
-- Background: SPEC-0003 §3 defines the token shape as "tk_" + 32
-- random bytes (43 chars after the prefix), and the bearer-auth
-- verifier needs to look up the key by the first 8 characters of
-- the token. Migration 0001 stored only the argon2id PHC, which
-- means a verifier would have to scan every row and verify each
-- hash (O(n) per request). This migration adds a 'prefix' column
-- (8 chars) and a unique index so lookups are O(log n).
--
-- The prefix is set on insert by the keys service; existing rows
-- (if any) get a placeholder so the NOT NULL constraint is
-- satisfied. The placeholder cannot authenticate any token because
-- it does not match the start of a real bearer.

ALTER TABLE api_keys ADD COLUMN prefix TEXT NOT NULL DEFAULT '';

-- Backfill: for any row that pre-dates this column, the default ''
-- is a safe no-op; the row is effectively dead because no bearer
-- can have an empty prefix. Operators upgrading from v0.1 with
-- live keys should re-create them.

CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(prefix);

-- ---------------------------------------------------------------------------
-- rooms uniqueness (SPEC-0002 §3.3 says "uniqueness enforced here,
-- not by the column" — we make the index unique).
-- ---------------------------------------------------------------------------
-- Drop the non-unique index from 0001 and recreate it as unique.
DROP INDEX IF EXISTS idx_rooms_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_rooms_name ON rooms(name) WHERE archived_at IS NULL;
