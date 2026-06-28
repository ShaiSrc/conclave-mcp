-- ---------------------------------------------------------------------------
-- 0003 — InterAgent v1.0 → Conclave v0.x feature audit (ADR-0007)
--
-- Adds the columns required by the Phase-51 work that resolves
-- the spec-vs-impl gaps in ADR-0007:
--
--   rooms.visibility       (SPEC-0001 Q6)
--   messages.message_type  (SPEC-0001 Q7, optional intent tag)
--   messages.metadata      (SPEC-0002 §3.5.1, optional JSON blob)
--   messages.deleted_at    (SPEC-0001 Q8, soft-delete)
--   audit.room_id          (SPEC-0001 Q10, denormalised for ?room_id= filter)
--
-- All columns are additive (NOT NULL with safe DEFAULT or NULL
-- allowed) so the migration is non-breaking on existing data:
-- old rooms are private, old messages have NULL message_type and
-- metadata and are not deleted, old audit rows have NULL room_id
-- (which means "not in a room", e.g. system key creates).
-- ---------------------------------------------------------------------------

ALTER TABLE rooms
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private';
-- Enforce the visibility enum in SQL so a bad client cannot
-- write 'public-ish'. SQLite has no ALTER TABLE ADD CONSTRAINT,
-- so the CHECK is added via a table-rebuild dance.
-- We use the simpler approach: a separate table-check index that
-- fails on bad values via a trigger. (See triggers below.)
CREATE INDEX IF NOT EXISTS idx_rooms_visibility ON rooms(visibility);

ALTER TABLE messages
    ADD COLUMN message_type TEXT NULL;
ALTER TABLE messages
    ADD COLUMN metadata     TEXT NULL;
ALTER TABLE messages
    ADD COLUMN deleted_at   TEXT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_room_deleted
    ON messages(room_id, deleted_at);
-- The metadata column is JSON. We don't enforce that in SQL
-- (modernc.org/sqlite does not ship with json1 enabled in
-- every build). The application layer validates.

ALTER TABLE audit
    ADD COLUMN room_id TEXT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_room_ts
    ON audit(room_id, ts);

-- ---------------------------------------------------------------------------
-- rooms.visibility CHECK via a trigger. SQLite's ALTER TABLE
-- cannot add a CHECK constraint, but a trigger can reject
-- bad inserts/updates. This is the documented workaround
-- (https://www.sqlite.org/lang_createtrigger.html).
-- ---------------------------------------------------------------------------
CREATE TRIGGER IF NOT EXISTS trg_rooms_visibility_check_insert
BEFORE INSERT ON rooms
FOR EACH ROW
WHEN NEW.visibility NOT IN ('private', 'public')
BEGIN
    SELECT RAISE(ABORT, 'rooms.visibility must be private or public');
END;

CREATE TRIGGER IF NOT EXISTS trg_rooms_visibility_check_update
BEFORE UPDATE ON rooms
FOR EACH ROW
WHEN NEW.visibility NOT IN ('private', 'public')
BEGIN
    SELECT RAISE(ABORT, 'rooms.visibility must be private or public');
END;

-- ---------------------------------------------------------------------------
-- message_type CHECK via a trigger. NULL is allowed (means
-- "unstructured"). Allowed values are the 8 InterAgent
-- intent types from SPEC-0001 Q7.
-- ---------------------------------------------------------------------------
CREATE TRIGGER IF NOT EXISTS trg_messages_message_type_check
BEFORE INSERT ON messages
FOR EACH ROW
WHEN NEW.message_type IS NOT NULL
     AND NEW.message_type NOT IN (
         'question', 'answer', 'decision', 'handoff',
         'status_update', 'task_assignment', 'alert', 'system'
     )
BEGIN
    SELECT RAISE(ABORT, 'messages.message_type not in allowed set');
END;
