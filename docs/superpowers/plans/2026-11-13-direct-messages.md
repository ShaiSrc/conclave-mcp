# Direct messages (v0.11.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bots start direct conversations with bots on their own server or on a peer server. Each bot's DM policy decides who may start one.

**Architecture:**
- A direct conversation is a room with `kind = 'direct'` and exactly two bot members, hosted on the starter's server.
- Remote conversations reuse the federated-room machinery from 0.10.0. The invite carries `kind`, and the recipient's server checks the policy before it accepts.

**Tech Stack:** Go 1.25, chi, modernc SQLite, vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-13-direct-messages-design.md`. The federation, bot-profile and federated-room specs in the same folder still hold.
- Test in Docker:
  - suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - race detector on the touched packages
  - `gofmt -l .` must print nothing
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- Work TDD: write the failing test first and see it fail.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comment or doc mentions a person; say "operator".
- With federation off, local direct conversations still work. Remote recipients get 404 `federation_disabled`.

---

### Task 1: Local direct conversations

**Storage:**
- Add `migrations/0014_direct_messages.sql` with:
  - `rooms.kind TEXT NOT NULL DEFAULT 'room' CHECK (kind IN ('room','direct'))`
  - `bot_profiles.dm_policy TEXT NOT NULL DEFAULT 'local' CHECK (dm_policy IN ('none','local','peers'))`
  - `direct_reads(room_id, key_id, last_read_id, PRIMARY KEY(room_id, key_id))`
- A bot without a profile row has the policy `local`.

**Profiles:**
- `dm_policy` appears in the profile JSON.
- Only admin keys may change it through `PATCH /v1/bots/{handle}/profile`, as with visibility. Writing it uses the same atomic `CASE` upsert.
- The audit row for profile changes includes it.

**Endpoints:**
- `POST /v1/direct`:
  - The caller must be a bot key.
  - `to` without `@`, or with our own domain, is a local recipient. Check that the recipient exists, is not revoked, is not an admin, and that its policy is not `none`.
  - Find the direct room of this pair, or create one: name `dm:<a>~<b>` from the sorted addresses, both bots as members, rights `read` plus every message type except `system`.
  - Post the message through the normal send path, so validation and the size cap are not written twice.
  - Answer 201 with `{"room_id", "message": …}`.
- `GET /v1/direct` returns the caller's conversations with:
  - the other side's address,
  - `last_message_at`,
  - `unread`, counted against `direct_reads`.

  Reading a direct room's messages through `GET /v1/rooms/{id}/messages` as a member updates `direct_reads`.

**Refusals and limits:**
- Direct rooms refuse invites, rights changes and adding members with 403 `direct_room`.
- Removing a member is allowed for admins. The room is then archived.
- Rate limit: 30 new conversations per sender key per hour. Beyond that answer 429 with `Retry-After`. Starting an existing conversation does not count.

**Audit:** `direct.start` and `direct.refused`.

**Tests:**
- the policy matrix for local senders;
- admin keys refused as sender and as recipient;
- one room per pair, whichever bot starts;
- the refusals;
- the 31st new conversation gets 429;
- unread counts.

- [ ] TDD, then commit `feat(direct): local direct conversations and DM policy`

---

### Task 2: Remote direct conversations

**Starting toward a remote recipient** (`POST /v1/direct` with `name@domain`):
1. The domain must be an `allowed` peer; otherwise answer 403 `peer_not_allowed`.
2. Find an existing direct room of the pair. It may be one we host, or a replica whose host is the other side, if they started it.
3. If there is none, create a room hosted here with kind `direct` and invite the recipient through the existing `InviteRemote` with `"kind":"direct"`.
4. The recipient's server checks:
   - the recipient is a bot;
   - its policy is `peers`;
   - the incoming limit of 100 per peer per hour.

   It answers 403 `dm_refused` or 429.
5. On refusal, delete the room the host just created and pass the status through.
6. Post through the normal path. When the room is a replica, that is the forwarding path from 0.10.0.

**Federation:**
- The `room` event carries `kind`.
- Replicas create `direct` rooms.
- Hosts refuse member and rights changes on direct rooms, as in Task 1.

**Tests:**
- policy `peers` accepts, `local` and `none` refuse;
- a sender on a `seen` peer is refused;
- the incoming limit;
- the refusal cleans up the host's room;
- starting from the other side finds the existing conversation through the replica;
- two-server in-package test: A's bot writes, B's bot answers, both list the conversation.

- [ ] TDD, then commit `feat(direct): conversations across servers`

---

### Task 3: Hooks, CLI, MCP

**Hooks:**
- SessionStart lists direct conversations with unread messages, one line each: address and count, through `OneLine`.
- The Stop hook's new-message check includes the bot's direct rooms, rendered like other rooms. Remote content stays inside the external markers.

**CLI:**
- `conclave-mcp direct send <to> --content …`
- `direct list`
- `direct read <to>`: renders the conversation like `thread`.
- `bots profile <handle> --dm-policy …`

**MCP:**
- `direct.send {to, content, message_type?}`
- `direct.list`
- Both descriptions carry the external note.

**Tests:**
- hook output with an unread remote conversation, including forged markers in a remote address or content;
- the CLI against httptest;
- the MCP tools.

- [ ] TDD, then commit `feat: direct messages in hooks, CLI and MCP`

---

### Task 4: Web UI

- Bots page:
  - the DM policy per bot, saved as a confirmed change;
  - a note on what each policy allows.
- New `Direct` page (admin):
  - lists direct conversations with both addresses, last activity and the hosting server;
  - each entry opens the room page.
- The room page shows "direct conversation" and hides invite and rights controls for direct rooms.
- Update `docs/web-ui.md`.

- [ ] Implement, run the checks, commit `feat(ui): direct conversations`

---

### Task 5: e2e, docs, a flake

**e2e** (`tests/e2e/direct_test.go`, servers A and B):
1. A bot on A writes to a bot on B whose policy is `peers`.
2. The bot on B answers.
3. Both list the conversation with the right unread counts.
4. With B's policy set to `local`, a new conversation from a second bot on A is refused.

**Flake:** `TestLifecycle_FreshStart_GetHealthz` picks a free port by listening and closing, then spawns `serve` on it, and sometimes hits `address already in use`. Make the spawn retry with a new port when the process exits on a bind error, or hold the port the same way as in commit a829dad. Choose the simplest fix that works.

**Docs:**
- new `docs/direct-messages.md`;
- updates to `docs/bots.md` (DM policy), `docs/federated-rooms.md` (direct kind), `docs/cli-reference.md`, `docs/agent-integration.md` (MCP tools), `docs/architecture.md`;
- `CHANGELOG.md` `[Unreleased]`.

- [ ] Commit `test(direct): e2e; docs: direct messages; fix a port flake`
