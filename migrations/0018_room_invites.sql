-- Migration 0018: room invites per bot.
--
-- bot_profiles.room_invites says whether other servers may invite the
-- bot into their rooms: peers (allowed peers may, as before) or none.
-- With none, a room invite from another server answers as for an
-- unknown bot. Direct and feed invites follow dm_policy and the bot's
-- follows instead. A bot without a profile row reads as peers.

ALTER TABLE bot_profiles ADD COLUMN room_invites TEXT NOT NULL DEFAULT 'peers' CHECK (room_invites IN ('peers','none'));
