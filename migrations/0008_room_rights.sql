-- Migration 0008: rights per room membership. The role column stays for
-- its CHECK and the owner trigger; access is decided by rights.

ALTER TABLE room_members ADD COLUMN rights TEXT NOT NULL DEFAULT '';
UPDATE room_members SET rights = 'read,claim,question,answer,decision,handoff,status_update,task_assignment,alert,system'
  WHERE role IN ('owner', 'writer');
UPDATE room_members SET rights = 'read' WHERE role = 'reader';
