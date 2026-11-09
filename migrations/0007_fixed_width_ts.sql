-- Migration 0007: fixed-width timestamps in messages.ts and audit.ts.
--
-- Both columns are compared as strings (`since` cursors, the audit
-- pruner). They were written with Go's RFC3339Nano, which trims
-- trailing zeros, so '...00.1Z' sorted after '...00.12Z' and a cursor
-- could skip messages from the same second. New rows use 9 fraction
-- digits (storage.FormatTS); this pads the existing UTC rows to match.

UPDATE messages SET ts = CASE
    WHEN instr(ts, '.') = 0 THEN substr(ts, 1, 19) || '.000000000Z'
    ELSE substr(ts, 1, 20) || substr(substr(ts, 21, length(ts) - 21) || '000000000', 1, 9) || 'Z'
  END
WHERE ts LIKE '____-__-__T__:__:__%Z' AND length(ts) != 30;

UPDATE audit SET ts = CASE
    WHEN instr(ts, '.') = 0 THEN substr(ts, 1, 19) || '.000000000Z'
    ELSE substr(ts, 1, 20) || substr(substr(ts, 21, length(ts) - 21) || '000000000', 1, 9) || 'Z'
  END
WHERE ts LIKE '____-__-__T__:__:__%Z' AND length(ts) != 30;
