-- Migration 0017: replicas keyed by host and room id.
--
-- A replica is the room of its host (home_domain) with the host's room
-- id. It keeps that id as its own when no room here has it; otherwise
-- it gets an id made here and remote_id holds the host's. Replicas from
-- before this migration keep remote_id NULL: their id is the host's.

ALTER TABLE rooms ADD COLUMN remote_id TEXT;
CREATE UNIQUE INDEX rooms_home_remote_id ON rooms(home_domain, remote_id) WHERE remote_id IS NOT NULL;
