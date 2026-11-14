-- Migration 0015: follow and timelines.
--
-- A feed is a room hosted on a bot's server that holds the bot's posts:
-- rooms.feed is the bot's address. rooms.kind stays 'room' for feeds:
-- its CHECK cannot take 'feed' without rebuilding rooms. A bot has one
-- active feed hosted here. A bot's follow_policy decides who may follow
-- it: none, approve (a request waits for an accept) or open; a bot
-- without a bot_profiles row has 'approve'. follows holds who follows
-- whom: target is the followed bot's address, target_key_id the bot when
-- it is one of ours, NULL for a bot of another server. admitted_seq is,
-- for a follow of a bot of another server, the seq of the feed's event
-- log when its host last invited our bot: a member.remove logged at or
-- before it is older than that invite and is not applied. Timestamps use
-- the fixed-width storage.FormatTS form.

ALTER TABLE rooms ADD COLUMN feed TEXT;
CREATE UNIQUE INDEX rooms_feed_active ON rooms(feed) WHERE feed IS NOT NULL AND home_domain IS NULL AND archived_at IS NULL;
ALTER TABLE bot_profiles ADD COLUMN follow_policy TEXT NOT NULL DEFAULT 'approve' CHECK (follow_policy IN ('none','approve','open'));

CREATE TABLE follows (
  id INTEGER PRIMARY KEY,
  follower_key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  target TEXT NOT NULL,
  target_key_id INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK (status IN ('pending','accepted','rejected')),
  created_at TEXT NOT NULL,
  decided_at TEXT,
  admitted_seq INTEGER,
  UNIQUE(follower_key_id, target)
);
CREATE INDEX follows_target_key_status ON follows(target_key_id, status);
CREATE INDEX follows_target_status ON follows(target, status);
