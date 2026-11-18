# Replica keys and room invites (v0.15.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Key federated replicas by (host, room ID) so a peer cannot take another host's room ID, and let a bot refuse room invites from other servers.

**Architecture:** Replica storage and lookups in `internal/federation` (replica.go above all), `internal/rooms`, `internal/follow` and `internal/direct`; the bot setting in `internal/bots`, `cmd/bots`, `internal/mcp` and `internal/ui/assets/bots.js`. Two migrations: 0017 and 0018. The wire protocol is unchanged.

**Tech Stack:** Go 1.25, chi, modernc SQLite, vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-18-replica-keys-room-invites-design.md`. `docs/protocol.md` (version 1) stays wire-compatible.
- Never edit migrations 0001–0016. Task 1 owns `migrations/0017_replica_keys.sql`, Task 2 owns `migrations/0018_room_invites.sql`. Use ALTER TABLE ADD COLUMN and CREATE INDEX only.
- Test in Docker:
  - suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - race on touched packages with `-timeout 20m`
  - `gofmt -l .` empty, and `go vet ./...` clean
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
  - fuzz seeds keep passing
- TDD.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comment or doc mentions a person; say "operator". Never print keys.
- Do not edit `CHANGELOG.md`, README badges or `docs/security.md`. The coordinator writes those.

---

### Task 1: Replicas keyed by host and room ID

- Migration 0017: `rooms.remote_id TEXT`, plus `CREATE UNIQUE INDEX ... ON rooms(home_domain, remote_id) WHERE remote_id IS NOT NULL`.
- Two helpers in internal/federation, used everywhere, with no SQL that maps IDs by hand elsewhere:
  - one that resolves a host's wire room ID to the local replica ID: `(home_domain, COALESCE(remote_id, id))`;
  - one that gives a local replica's host and wire ID.
- Replica creation (invites of every kind: room, direct, feed): use the host's ID as the local ID when it is free. Otherwise make a new local room ID (the same generator local rooms use) and store `remote_id`. Remove the 0.14 409 `conflict` and its `held_by` audit detail at invites.
- Every inbound path resolves through the helper:
  - push, pull answers, leave and removal;
  - claims, forwarded-message answers;
  - follows and feed invites (`admitted_seq`), direct invites (`pair`);
  - block and purge, catch-up.
- Every outbound path sends the wire ID. Author signatures sign and verify with the wire ID.
- `internal/rooms` view (`GET /v1/rooms`, `GET /v1/rooms/{id}`): add `remote_id` when set. CLI `rooms list` and the room page in `internal/ui/assets/rooms.js` show the host's ID when it differs.
- Tests:
  - two hosts invite our bot under one room ID, and each replica takes only its own host's events, messages, claims, leave and purge;
  - a signature verifies in a remapped replica;
  - a feed under a taken ID;
  - existing replicas with NULL `remote_id`;
  - in the interop suite (`tests/e2e/interop_test.go`), a step where B and C both host a room with the same ID and A's bot is in both.
- Docs: `docs/federated-rooms.md` (new subsection "Room IDs of other servers"), `docs/follow.md` and `docs/direct-messages.md` where they mention 409 conflicts, `docs/cli-reference.md`, `docs/web-ui.md`.
- [ ] Commit `feat(federation): replicas keyed by host and room ID`

---

### Task 2: Room invites per bot

- Migration 0018: `bot_profiles.room_invites TEXT NOT NULL DEFAULT 'peers' CHECK (room_invites IN ('peers','none'))`.
- `internal/bots`: the profile, the patch and the view carry `room_invites`.
  - Validation: only `peers` or `none`, else 400.
  - An admin key or the bot itself may set it, as for `dm_policy`.
  - Federation answers (`/fed/v1/bots/{name}`, directory) never show it.
- `handleRoomInvite` (internal/federation/replica.go), kind `room` only:
  - `none` answers exactly as an unknown bot: same status 404, same code, same message;
  - audits locally `federation.room.invite` with error `room_invites`.
  - Keep the check one small call into the bots package, so it merges cleanly with Task 1's work on replica.go.
- CLI: the bots profile command takes `--room-invites peers|none` and prints the value.
- MCP: the profile tools take and show it where they handle `dm_policy`.
- Web UI: `internal/ui/assets/bots.js` gets a select next to the DM policy on the bot page, with the passkey-confirmed save as the other fields have.
- Tests:
  - `none` is 404 with the body of an unknown bot, and the audit row is written;
  - `peers` behaves as before;
  - direct and feed invites are unaffected;
  - validation;
  - the federation profile does not leak the setting.
- Docs: `docs/bots.md`, which recommends `none` for private bots; `docs/direct-messages.md`, where the 0.14 sentence says room invites are not gated; `docs/protocol.md`, one sentence in the invite section that a receiver may answer 404 for a bot that refuses invites; `docs/cli-reference.md`; `docs/web-ui.md`.
- [ ] Commit `feat(bots): a bot can refuse room invites from other servers`

---

### Task 3: Review, release prep, smoke test (coordinator)

- Security review of both tasks' diffs by an independent reviewer.
- `docs/security.md`: drop both accepted limits.
- CHANGELOG `[0.15.0]`; README version badge and the "current release" line.
- Browser smoke test with two live servers.
- PR.
