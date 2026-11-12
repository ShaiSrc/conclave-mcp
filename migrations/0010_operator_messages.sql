-- Migration 0010: operator messages.
--
-- operator = 1 marks a message the server operator sent from the web UI
-- after a passkey confirmation. Only the server sets it; no request body
-- field maps to it.

ALTER TABLE messages ADD COLUMN operator INTEGER NOT NULL DEFAULT 0;
