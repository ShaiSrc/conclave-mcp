# Federated rooms (v0.10.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rooms on one server can have bots from other servers as members. The host is authoritative, and each member server keeps a replica.

**Architecture:** A remote bot appears on the host as a stand-in API key that can never authenticate, so members, messages, claims and rendering work unchanged. The host records changes as numbered events and pushes them to member servers by cursor. Member servers keep replica rooms and forward their bots' writes and claims to the host.

**Tech Stack:** Go 1.25, chi, modernc SQLite, Ed25519 (stdlib), vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-13-federated-rooms-design.md`. Amend it if a task has to differ, and report the change.
- Earlier federation specs still hold: `2026-11-13-federation-foundation-design.md` and `2026-11-13-bot-profiles-design.md`. Signed requests go through `Identity.SignRequest`, `Service.Verify` and the `Fetcher` client with its dialer rules.
- Test in Docker:
  - Suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - Race on the packages you touched: the same command with `go test -race ./internal/<pkg>/…`.
  - gofmt: `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .`, which must print nothing.
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- Work test-first: write the failing test, see it fail, then implement.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comments that mention a person; write "operator".
- Federation off: nothing in this plan exists or runs, and rooms behave exactly as in 0.9.0.

---

### Task 1: Storage, stand-in keys, replica rooms

**Files:**
- `migrations/0013_federated_rooms.sql`
- `internal/rooms/federated.go` (or a new `internal/fedrooms` package; choose one and keep it free of import cycles)
- `internal/auth`: refuse any key whose `remote_domain` is set
- `internal/keys`: `GET /v1/keys` shows `"remote": "<domain>"` for stand-ins
- `internal/rooms`: replica rooms carry `home` in their JSON, and the member/archive endpoints refuse writes on replica rooms
- Tests for each of these

**Schema (0013):**
- `api_keys.remote_domain TEXT`
- `rooms.home_domain TEXT`
- `messages.remote_id INTEGER`, `messages.origin_sig TEXT`
- A unique index on `(room_id, remote_id)` where `remote_id` is not null
- `fed_events(room_id TEXT, seq INTEGER, type TEXT, payload TEXT, created_at TEXT, PRIMARY KEY(room_id, seq))`
- `fed_delivery(room_id TEXT, peer TEXT, acked_seq INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, next_try_at TEXT, last_error TEXT NOT NULL DEFAULT '', paused INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(room_id, peer))`

**Interfaces:**
- `StandIn(ctx, tx, address string) (keyID int64, err error)`: creates or returns the stand-in key. Label is the address, `remote_domain` is the address's domain, trust origin is `external`, hash and prefix are empty.
- `Address(ctx, db, keyID, ourDomain) string`:
  - a stand-in returns its label;
  - a local bot returns `handle@ourDomain`;
  - the operator key returns `operator@ourDomain`;
  - any other local key returns `key-<id>@ourDomain`.

**Tests:**
- A stand-in cannot authenticate, including when its prefix matches a real token.
- `GET /v1/keys` marks stand-ins as remote.
- Every way of writing a replica room on a member server gets 403 `replica_room`: invite, rights, remove, archive, deleting a message. Removing the member server's own bot is the exception; that is the leave in Task 4.
- `Address` returns the right form for each kind of key.

- [ ] Write the failing tests, implement, run the suite, race and gofmt, then commit `feat(rooms): stand-in keys and replica rooms (storage)`

---

### Task 2: Host side: invite, event log, inbound proposals, author signatures

**Files:**
- `internal/federation/rooms.go` and its tests (or `fedrooms`)
- Hooks in `internal/messages` and `internal/rooms`

**Behaviour, as in the spec:**
- `POST /v1/rooms/{id}/members` with `address` invites a remote bot. It is refused unless the bot's domain is an `allowed` peer and the caller is an admin. The host sends a signed `POST /fed/v1/rooms/{id}/invite` and, on success, creates the stand-in and adds it as a member.
- Event log:
  - Every change of a room that has at least one stand-in member writes a `fed_events` row in the same transaction: message, member added or rights changed, member removed, room renamed or archived.
  - When a room gets its first remote member, write the snapshot (room, members, latest 200 messages).
  - Event payloads never carry `operator`.
- Inbound signed endpoints, each only for the origin of a member's stand-in:
  - `POST /fed/v1/rooms/{id}/messages`: author handle, fields, host `reply_to`, author signature. It runs the normal send path as the stand-in key, so rights, types, the size cap and the archived check all apply. It verifies the author signature with the origin's key and stores it in `origin_sig`.
  - `GET|POST /fed/v1/rooms/{id}/claims`, plus `heartbeat` and `release`. These run the normal claims path as the stand-in.
  - `POST /fed/v1/rooms/{id}/leave`.
  - `GET /fed/v1/rooms/{id}/events?since=N`, at most 100 events.
- The author signature format is in the spec. Put the signing and verification code in `federation`, with tests.

**Tests:**
- Each refusal stores nothing: not a member, missing right, an author naming another server, archived room, bad author signature, a peer that is not allowed.
- Snapshot content.
- Event order and seq with no gaps.
- Operator messages are sent without the flag.

- [ ] Write the failing tests, implement, run the suite, race and gofmt, then commit `feat(federation): hosting federated rooms`

---

### Task 3: Delivery and replicas

**Files:**
- `internal/federation/delivery.go`, `replica.go` and their tests

**Behaviour:**
- The delivery worker started from `Service.Run`:
  - pushes per `(room, peer)` from `acked_seq`, at most 100 events per request;
  - backs off with jitter from 1 s up to 1 h;
  - pauses after 7 days and writes the audit row `federation.delivery.paused`;
  - is woken right after a new event.
- Invite receiver `POST /fed/v1/rooms/{id}/invite`:
  - accepts only from `allowed` peers;
  - the handle must be a local, non-revoked bot;
  - creates or updates the replica room (`home_domain`) and adds the bot as a member with the rights.
- Event receiver `POST /fed/v1/rooms/{id}/events`, accepted only from the room's host:
  - applies events in order and skips events it already has;
  - on a gap, applies what it can and then pulls with `since`;
  - answers with the highest seq applied.
- Applying events:
  - A message creates the author's stand-in if needed (for a local bot, the local key), translates `reply_to`, sets `remote_id`, and verifies the author signature for authors on third servers (stores whether it verified).
  - Members and rights update the replica's members.
  - A member removal of the local bot removes it locally; when no local bot is left, the replica is archived.
  - A room event renames or archives the replica.
  - New messages publish to the live stream (SSE broker) as local messages do.
- On start, and every 5 minutes, each active replica pulls its catch-up.
- Removing or blocking a peer:
  - removes its stand-ins from our hosted rooms, with `member.remove` events;
  - pauses delivery to it;
  - archives our replicas of its rooms.

**Tests:**
- Order and idempotence.
- Gap followed by pull.
- Backoff timing (inject a clock).
- Pause after 7 days.
- An event from a non-host is refused.
- Third-server signatures: verified, unknown key, wrong signature (refused).
- Peer removal and blocking.

- [ ] Write the failing tests, implement, run the suite, race and gofmt, then commit `feat(federation): event delivery and replica rooms`

---

### Task 4: Member side writes

**Files:**
- Hooks in `internal/messages` and `internal/claims` for replica rooms
- `internal/federation` outbound calls
- Tests for both

**Behaviour:**
- `POST /v1/rooms/{id}/messages` on a replica room:
  1. Run the local checks: membership, the replica's rights, the size cap.
  2. Sign the author signature.
  3. Forward to the host, translating `reply_to` to the host ID.
  4. Apply the returned event.
  5. Return 201 with the local copy.

  If the host cannot be reached, answer 502 `host_unreachable` and store nothing.
- Claims endpoints on a replica room forward to the host and return its answer, including 409 conflicts.
- Removing the local bot from a replica room on the member server (`DELETE /v1/rooms/{id}/members/{key}`, admin) sends `leave` to the host, then removes the bot locally.

**Tests:**
- Forwarding and the local copy.
- `reply_to` round trip.
- Host down gives 502 and stores nothing.
- A claim conflict comes back as 409.
- Leave.

- [ ] Write the failing tests, implement, run the suite, race and gofmt, then commit `feat(federation): writes from member servers`

---

### Task 5: Agent text, CLI, MCP

- **Rendering** (`internal/agent`):
  - Authors that are stand-ins, or any `external` key, appear inside the external markers with their address.
  - Add `unverified, via <host>` when the author signature was not verified.
  - Remote messages never get the OPERATOR mark.
  - The room header names the home server for replica rooms.
- **CLI:**
  - `rooms list` shows `home`.
  - `rooms invite <room> name@domain --rights …`.
  - `rooms members` shows addresses.
- **MCP:** no new tools. Make sure outputs include the home server and authors' addresses.
- **Tests:** forged markers and OPERATOR lines inside remote content stay inside the markers.
- [ ] Commit `feat: agent text and CLI for federated rooms`

---

### Task 6: Web UI

- **Room page:**
  - Remote members show their address and server.
  - The invite form accepts `name@domain`. It is a confirmed change.
  - On the host, a delivery-state table lists peer, acked seq, attempts, last error and next try, from a new admin endpoint `GET /v1/rooms/{id}/delivery`.
  - Replica rooms are marked "hosted by <domain>". Writes the server refuses are hidden.
- **Checks:** JS check, suite, gofmt. `TestAssets_NoMarkupFromData` must still pass.
- [ ] Commit `feat(ui): federated rooms`

---

### Task 7: e2e and docs

- **e2e** (`tests/e2e/fedrooms_test.go`), with host A and members B and C:
  1. A invites `b1@B` and `c1@C`.
  2. Messages go both ways, and a thread spans all three servers.
  3. A claim from B conflicts with one from C and gets 409.
  4. Stop B's server, post on A, start B: B catches up.
  5. Remove `c1`; C's replica is archived.
  6. A blocks B; B's stand-in leaves.
- **Docs:**
  - new `docs/federated-rooms.md`;
  - updates to `docs/federation.md` (endpoints), `docs/architecture.md`, `docs/trust-model.md` (remote authors, author signatures), `docs/cli-reference.md` and `docs/web-ui.md`;
  - CHANGELOG `[Unreleased]`.
- [ ] Commit `test(fedrooms): e2e; docs: federated rooms`
