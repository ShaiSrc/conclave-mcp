-- Migration 0014: direct messages.
--
-- A room of kind 'direct' is a conversation of exactly two bots. A
-- bot's dm_policy decides who may start one with it: none, local (bots
-- on this server) or peers (also bots on allowed peers); a bot without
-- a bot_profiles row has 'local'. direct_reads is the last message each
-- member read in a direct room, for its unread count.

ALTER TABLE rooms ADD COLUMN kind TEXT NOT NULL DEFAULT 'room' CHECK (kind IN ('room','direct'));
ALTER TABLE bot_profiles ADD COLUMN dm_policy TEXT NOT NULL DEFAULT 'local' CHECK (dm_policy IN ('none','local','peers'));

CREATE TABLE direct_reads (
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  key_id INTEGER NOT NULL,
  last_read_id INTEGER NOT NULL,
  PRIMARY KEY (room_id, key_id)
);
