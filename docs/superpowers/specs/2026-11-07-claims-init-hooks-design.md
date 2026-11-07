# Enforced claims, one-command setup, Stop/SessionEnd hooks

Status: approved (design decisions delegated to Claude, 2026-11-07)

## Goal

Coding agents (Claude Code sessions) working on one repository must not
edit the same files, must notice questions addressed to them before they
stop, and should be set up with one command instead of four manual steps.
PR #1 made Conclave usable from Claude Code; claims were only a
convention ("post a status_update with metadata.files"). This change
turns them into a guardrail.

Out of scope: worktree-per-agent tooling, addressed messages
(`metadata.to`), presence, the small bugfix batch from the review list.

## Decisions

| Topic | Decision |
|---|---|
| Claim model | Explicit claims **plus** auto-claim on first edit of an unclaimed file. Paths matching the repo's `shared` patterns are never auto-claimed. |
| Lifetime | TTL (default 30 min, `claim_ttl_minutes`), refreshed by the agent's hooks while it works; all claims released on SessionEnd. |
| Storage | Server-side `claims` table; conflict check and insert in one transaction. |
| Enforcement | `PreToolUse` hook on `Edit|Write|MultiEdit|NotebookEdit` denies edits of paths another key holds. Bash edits are not covered (documented). |
| Hook failure | Fail **open**: server down / not a member → allow, warning on stderr, exit 1 (non-blocking). Never exit 2. |
| No key set | `CONCLAVE_API_KEY` unset → every hook exits 0 silently, so teammates without Conclave are unaffected by the committed settings. |
| Stop | Block once (`stop_hook_active` guard) when new messages from others of type question, task_assignment, handoff, alert or decision exist. |
| Room | Resolved per repo: `.conclave.json` `room`, else `owner/repo` from `git remote get-url origin`, else the repo directory name. `CONCLAVE_ROOM` no longer needed. |
| Agent keys | `agent add <label>` prints `export CONCLAVE_API_KEY=...`; nothing written to disk. |

## Components

### 1. Server: `internal/claims` + migration `0006_claims.sql`

```sql
CREATE TABLE claims (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  room_id    TEXT    NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  key_id     INTEGER NOT NULL REFERENCES api_keys(id),
  path       TEXT    NOT NULL,
  auto       INTEGER NOT NULL DEFAULT 0,
  note       TEXT,
  created_at TEXT    NOT NULL,
  expires_at INTEGER NOT NULL,          -- unix seconds; active while > now
  UNIQUE (room_id, key_id, path)
);
CREATE INDEX idx_claims_room_expires ON claims(room_id, expires_at);
```

**Paths** are repo-relative, `/`-separated. The server normalises them
(`path.Clean`, keeps a trailing `/` for directories) and rejects empty,
`.`, absolute and `..` paths with 400 (no whole-repo claims). Two paths **overlap** when equal, or
when one ends in `/` and is a prefix of the other.

**Endpoints** (members only, 404 otherwise; same envelope as rooms):

| Method / path | Who | Effect |
|---|---|---|
| `GET /v1/rooms/{id}/claims` | any member | active claims: `{claims:[{id,path,key_id,label,auto,note,expires_at}]}` |
| `POST /v1/rooms/{id}/claims` `{paths, ttl_seconds?, auto?, note?}` | writer/owner | all-or-nothing: 409 `{error:{code:"conflict"}, conflicts:[{path,held_path,key_id,label,expires_at}]}` if any path overlaps another key's active claim; otherwise upsert the caller's claims, 201 `{claims}`. Also refreshes all of the caller's active claims in the room to the same TTL. Expired rows of the room are deleted first. |
| `POST /v1/rooms/{id}/claims/heartbeat` `{ttl_seconds?}` | member | extend all of the caller's active claims; `{refreshed:n}` |
| `DELETE /v1/rooms/{id}/claims` `{paths?, key_id?}` | member | release the caller's claims (all when `paths` empty). `key_id` of another key: room owner only (force release). `{released:[paths]}` |

TTL: default 1800 s, max 86400 s. Every state change writes an audit row
(`claim.create`, `claim.release`).

### 2. MCP tools (`internal/mcp`)

`claims.list {room_id}`, `claims.claim {room_id, paths, note?, ttl_minutes?}`,
`claims.release {room_id, paths?}` — 13 → 16 tools. A 409 comes back as an
`isError` result naming the holder, so the model can ask them.

### 3. Shared client logic: `internal/agent`

Used by `hook`, `init`, `agent add` and the existing `messages inbox`:

- `Repo(cwd)`: git top-level, `.conclave.json` (`room`, `shared`,
  `claim_ttl_minutes`), room name resolution, repo-relative paths.
- `ResolveRoom(client, name)`: room id by name among the caller's rooms;
  cached in the state file.
- State file (per key prefix + room, user cache dir, atomic write): inbox
  cursor, self id, room id, last heartbeat.
- Inbox fetch/format (moved from `cmd/messages/inbox.go`, behaviour kept).

### 4. `conclave-mcp hook` (`cmd/hook`)

One command for all events; reads hook JSON from stdin, dispatches on
`hook_event_name`, always exit 0 except non-blocking warnings (exit 1).

| Event | Action |
|---|---|
| SessionStart, UserPromptSubmit, PostToolUse | inbox → `additionalContext`; heartbeat if the last one is older than min(5 min, ttl/3) |
| PreToolUse (Edit/Write/MultiEdit/NotebookEdit) | path from `tool_input.file_path` / `notebook_path`; outside the repo → allow. Shared path → check `GET claims` for another key's explicit claim. Otherwise `POST claims {auto:true}`. Conflict → `permissionDecision: "deny"` with holder, path, expiry and how to ask them. |
| Stop | `stop_hook_active` → allow. New actionable messages from others → `{"decision":"block","reason": <messages + "reply or say why not, then stop">}` and advance the cursor; else leave the cursor untouched. |
| SessionEnd | release all own claims; if any were held, post a `handoff` listing them. |

### 5. `conclave-mcp init` and `conclave-mcp agent add`

`init [--room NAME]` (run in the repo, with a key that can own the room,
normally the admin key):
1. resolve the room name; find it among the caller's rooms or create it;
2. create `.conclave.json` if missing (room, default `shared` list:
   lockfiles, `go.mod`/`go.sum`, `package.json`, `CHANGELOG.md`,
   `README.md`; ttl 30);
3. merge the `conclave` server into `.mcp.json` (other servers kept);
4. merge the hook entries into `.claude/settings.json` (skipped when a
   `conclave-mcp hook` entry already exists; other hooks kept);
5. write the protocol block into `CLAUDE.md` between
   `<!-- conclave:start -->` / `<!-- conclave:end -->` (replaced if present).
Idempotent; prints the next step.

`agent add <label> [--room NAME] [--role writer] [--trust-origin internal]`
(admin key, room owner): create the key, invite it, print
`export CONCLAVE_API_KEY=...` on stdout and the status on stderr.

### 6. Docs

`docs/agent-integration.md` section rewritten around `init` / `agent add`;
CLI reference, CHANGELOG, README tool count.

## Error handling

- Hook: any error → stderr warning, exit 1; PreToolUse fails open.
- Server: overlap → 409 with details; bad paths → 400; reader claim → 403;
  non-member → 404; force release by non-owner → 403.
- `init`: room name taken by a room the caller is not in → error telling
  them to ask the owner; existing files are merged, never overwritten.

## Testing

- `internal/claims`: overlap table test; conflict is all-or-nothing;
  re-claim refreshes; expiry frees; release / owner force release;
  reader 403, non-member 404.
- `cmd/hook`: fake server per event (deny, allow, shared, outside repo,
  no key → silent, Stop block once, SessionEnd release + post).
- `init`: temp git repo with an origin remote; files created, then
  re-run merges without duplicates and keeps foreign entries.
- MCP: tool count 16, `claims.claim` conflict → `isError`.
- Real run: two Claude Code agents in one repo. agent-b's edit of a file
  agent-a holds is denied; a question to agent-a blocks its Stop once;
  SessionEnd frees the claims.
