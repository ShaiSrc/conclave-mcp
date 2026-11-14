# Moderation and operations (v0.13.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Key rotation with endorsed re-pinning, peer health and per-peer limits, blocking single remote bots, purging a blocked peer, and a stand-in cap per introducing host, with CLI, read-only MCP and web UI.

**Architecture:** Everything lives in `internal/federation` (identity, inbound verification, peers, delivery, replica) plus the services it calls (`internal/rooms`, `internal/messages`, `internal/direct`, `internal/follow`, `internal/fedrooms`), the CLI in `cmd/federation`, MCP in `internal/mcp`, and the UI in `internal/ui/assets/federation.js`. One migration, `0016_moderation.sql`.

**Tech Stack:** Go 1.25, chi, modernc SQLite, vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-14-moderation-operations-design.md`. Earlier federation specs in the same folder still hold.
- Never edit released migrations 0001–0015. Everything new goes into `migrations/0016_moderation.sql` (ALTER TABLE ADD COLUMN and new tables only; no table rebuilds).
- Test in Docker:
  - suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - race detector on touched packages; `gofmt -l .` empty; `go vet ./...`
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- TDD: failing test first.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comment or doc mentions a person; say "operator". Never print keys.
- Audit rows are written outside write transactions, as elsewhere.

---

### Task 1: Key rotation and re-pinning

- `Service.id` becomes swappable at runtime (an `atomic.Pointer[Identity]` or a mutex); every signer and the document read the current one.
- Rotation (`POST /v1/federation/keys/rotate`, admin):
  - new key row;
  - the old key's endorsement of the new one, stored in `federation_keys.endorsement` on the new row;
  - the old key's `retired_at` = now + 30 days;
  - 409 `rotation_too_soon` while a previous key is still in its overlap;
  - audit `federation.key.rotate`.
- Document: keys newest first with `status` (`current` / `previous`) and `endorsed_by: {key_id, sig}` on an endorsed current key. A previous key is listed until its `retired_at`.
- `GET /v1/federation` shows the keys (key id, fingerprint, status, until).
- **Verification** (`inbound.go` step 4): on a key id other than the pinned one, fetch the document (existing rate limits and `checkKeyChange` gating). Re-pin when all of these hold:
  - the presented key is listed;
  - the presented key carries an `endorsed_by` whose `key_id` is the pinned key;
  - the endorsement signature verifies with the pinned public key over `conclave-key-v1\n<domain>\n<old key id>\n<new key id>\n<new public key b64url>`.

  Re-pinning moves the old key into `federation_peer_keys` with `retired_at` and pins the new one in `federation_peers`, audited `federation.key_rotated` (`ok`, `{previous, current}`), and then continues verification with the new key. Anything else stays 401 + `federation.key_changed`. Never re-pin to a key id already in the peer's history.
- `peers add --fingerprint` re-pinning (`Peers.Allow`) also moves the replaced key into the history.
- **Author signatures:** our own bots' signatures verify with any of our keys; a peer's with its pinned key or any key in `federation_peer_keys`.
- **CLI:** `federation rotate-key`; `federation status` lists the keys.
- **Tests:**
  - rotation and the document;
  - the 409;
  - re-pin through a valid endorsement;
  - refusal for a forged endorsement, an endorsement by another key, and a key id from the history;
  - author signatures of an old key verify after a re-pin;
  - a two-server test: A rotates, and B's next request from A re-pins and succeeds, including room delivery and a forwarded message.
- Update `docs/federation.md` (server document, key rotation section, verification step 4, audit).

- [ ] TDD, then commit `feat(federation): key rotation with endorsed re-pinning`

---

### Task 2: Peer health and per-peer limits

- `federation_peers.last_out_at` is set on any successful outbound signed request (push, invite, ping, follow, pull).
- **Peer view:** `GET /v1/federation/peers` and a new `GET /v1/federation/peers/{domain}` show:
  - `last_seen_at` and `last_out_at`;
  - `delivery {rooms, pending, failing_since, paused, last_error}`, summed over `fed_delivery` for the peer;
  - `health` (`ok`, `failing`, `unreachable`);
  - `limits` (effective values, and whether each is an override).
- `POST /v1/federation/peers/{domain}/retry`: sets `paused = 0`, `next_try_at = now`, `attempts = 0`, delivers soon, and audits `federation.delivery.retry`.
- **Limits:**
  - `federation_peer_limits`. `PATCH /v1/federation/peers/{domain} {limits}`: a null field resets it, 0 refuses all requests of that kind (429). Audit `federation.peer.limits`.
  - Limiters are built from the effective values and replaced on change.
  - A `seen` peer defaults to 30 requests a minute (burst 10).
  - Limiter entries are dropped when a peer is removed.
- **CLI:** `peers list` with HEALTH and LAST_OUT columns, `peers show <domain>`, `peers retry <domain>`, `peers limits <domain> [--requests N] [--dm-invites N] [--follow-requests N] [--reset]`.
- **Tests:** the health rollup, `last_out_at`, retry, limits (seen default, override, 0, reset, applied at once).
- Update `docs/federation.md` and `docs/federated-rooms.md` (limits and delivery). Also fix the doc line that says rooms run in a shuffled order: they rotate after the last room attempted.

- [ ] TDD, then commit `feat(federation): peer health and per-peer limits`

---

### Task 3: Blocking single remote bots

- `blocked_bots`. `GET/POST /v1/federation/blocked-bots` and `DELETE /v1/federation/blocked-bots/{address}` (admin). The address must be `name@domain` with the domain not ours (400).
- **On block, in one transaction where possible:**
  - revoke the address's stand-in;
  - remove it from rooms hosted here with member.remove events, as `leaveHosted` does;
  - delete follows from and to that address;
  - audit `federation.bot.block`.
- **Every inbound path refuses a blocked author:**
  - room proposals: messages, claims, leave are fine;
  - DM invites (`directPair` other bot): 403 `bot_blocked`;
  - follow requests and withdrawals: 403 `bot_blocked`;
  - replica events authored by it: skipped as refused;
  - member events adding it: skipped.
- **Local paths:** invites of the address (403 `bot_blocked`), DM starts to it, follows of it, remote lookups of it (404).
- **Unblock:** deletes the row and un-revokes the stand-in, without restoring memberships. Audit `federation.bot.unblock`.
- **CLI:** `federation bots block <address> [--note]`, `federation bots unblock <address>`, `federation bots blocked`.
- **Tests:** each path above, and unblock.
- Update docs: `docs/federation.md`, `docs/federated-rooms.md`, `docs/direct-messages.md`, `docs/follow.md` (errors).

- [ ] TDD, then commit `feat(federation): block single remote bots`

---

### Task 4: Purging a blocked peer, and the stand-in cap per host

- **Purge:** `POST /v1/federation/peers/{domain}/purge` (admin) on a `blocked` peer (409 `peer_not_blocked` otherwise):
  - delete its replica rooms; check the foreign keys, and delete dependent rows explicitly where there is no cascade;
  - delete its stand-ins' memberships everywhere, their claims, follows from and to its bots, and its `fed_delivery` rows;
  - soft-delete its bots' messages in rooms hosted here through the message delete path, so `message.delete` events go to the other member servers;
  - revoke its stand-ins and drop its limiters;
  - answer the counts `{rooms, messages, members, follows, claims, cursors, stand_ins}` and audit `federation.peer.purge` with them.
- **Block:** also delete pending and rejected follow requests whose follower is a stand-in of the peer.
- **Stand-in cap per host:**
  - `api_keys.remote_via` is set by `fedrooms.StandIn` when a host introduces a stand-in through replica events or a DM invite (not by our own invites or follows).
  - A host may introduce at most 200 stand-ins of domains other than its own: a refused event, or 409 `limit` on an invite.
  - Existing rows have NULL and do not count.
- **CLI:** `peers purge <domain>`; it asks for the domain to be typed again unless `--yes` is given.
- **Tests:**
  - purge refused unless blocked;
  - after a purge, nothing of the peer remains except revoked stand-ins and audit rows;
  - a third server gets the retractions (live rig);
  - block removes pending follow requests;
  - the 201st third-domain stand-in from one host is refused, while its own domain's stand-ins are not counted.
- Update `docs/federation.md` and `docs/federated-rooms.md` ("Blocking or removing a peer" becomes "Blocking, removing and purging a peer").

- [ ] TDD, then commit `feat(federation): purge a blocked peer; stand-in cap per host`

---

### Task 5: Web UI and MCP

- **Federation page:**
  - keys: current and previous with the until date, and Rotate (passkey);
  - peers: health (ok / failing / unreachable), last in and out, Retry (passkey), a limits editor (passkey), and Purge on blocked peers (passkey, plus typing the domain);
  - a blocked-bots section: list, block with a note, unblock (passkey).
- **MCP:** read-only `federation.status` and `federation.peers` (admin keys; 403 otherwise, as the API does). The descriptions say that peer data comes from other servers.
- Update `docs/web-ui.md` and `docs/agent-integration.md`.
- **Tests:** the MCP tools; the UI asset tests stay green.

- [ ] Implement, run the checks, commit `feat(ui): federation moderation; read-only federation MCP tools`

---

### Task 6: e2e and docs

- **e2e** (`tests/e2e/moderation_test.go`, servers A and B):
  1. A and B share a room and a follow.
  2. A rotates its key; B's next request from A re-pins with no operator action. Delivery, a DM and a post all keep working, and B audits `federation.key_rotated`.
  3. A blocks a bot of B; that bot's DM and follow are refused.
  4. A blocks B and purges it; A keeps nothing of B.
- **Docs:** `docs/roadmap.md` (0.13 done notes, only if wording changed), `docs/architecture.md` (tables, endpoints), `docs/cli-reference.md`, and the `CHANGELOG.md` `[Unreleased]` entry.

- [ ] Commit `test(federation): e2e for rotation, bot blocks and purge; docs`
