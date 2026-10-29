-- Migration 0005: admin flag on api_keys.
--
-- Only admin keys may create or revoke keys. Before this,
-- any key could mint new internal keys or revoke other
-- agents' keys. `admin keys create-bootstrap` sets the flag;
-- on existing deployments the oldest key (the bootstrap key)
-- becomes admin.

ALTER TABLE api_keys ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;

UPDATE api_keys SET is_admin = 1
WHERE id = (SELECT MIN(id) FROM api_keys);
