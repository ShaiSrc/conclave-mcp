# CHANGELOG

All notable changes to Conclave MCP are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

Makes the MCP server usable from Claude Code and adds what
coding agents on one project need to coordinate.

### Added

* `conclave-mcp messages inbox <room-id>`: prints new messages
  from other agents since the last call (per-key cursor) and
  speaks the Claude Code hook protocol (`additionalContext`).
  Setup in docs/agent-integration.md, "Coding agents on one
  project".
* `GET /v1/whoami`: the caller's own key (id, label,
  trust_origin, admin).
* Migration `0005_admin_keys.sql`: `api_keys.is_admin`. The
  bootstrap key is admin; on existing databases the oldest
  active key becomes admin.

### Fixed

* MCP: every tool now has an `inputSchema`. Claude Code rejected
  the whole tool list without it.
* MCP: `tools/call` returns MCP content blocks; tool failures
  come back as `isError` results the model can read. `ping` is
  answered. Empty 201/204 responses (invite, remove, revoke) no
  longer surface as "decode response: EOF" errors.
* MCP: `messages.subscribe` honours `since`, returns as soon as
  a message is available instead of after the full `wait`, and
  skips the caller's own messages. The SSE endpoint subscribes
  before sending its `: ok` preamble, so nothing is lost
  between backlog read and live stream.
* MCP over HTTP (`POST /v1/mcp`) uses the same dispatch as
  stdio (initialize, ping, notifications) and no longer cuts
  long `subscribe` calls off after 30 s.
* `audit.list` URL-encodes its filters.
* Security: only admin keys can create or revoke keys. Before,
  any key could mint new internal keys or revoke other agents'
  keys.
* Auth: a key's repeat requests skip argon2id (verified tokens
  are remembered as sha256 -> PHC; the DB lookup still runs, so
  revocation takes effect at once). Before, every request paid a
  19 MiB argon2id run, which queued requests under load and
  made hook-per-tool-call setups expensive.
* e2e load tests no longer call `t.Fatalf` from goroutines and
  allow 30 s per request; they failed spuriously on busy hosts.
* CLI: errors are printed to stderr; before, failing commands
  exited non-zero without a message.
* Docker image builds again (Go 1.25 builder, no `RUN` in the
  distroless stage).

## [0.1.1] — 2026-06-28

The v0.1.1 release closes the agent-to-agent communication
loop. v0.1.0 was a working HTTP+CLI server with rooms, messages,
keys, and audit, but did not yet have the trust classification
or live event stream that an agent-to-agent workflow needs.
v0.1.1 adds both, plus the operational tools to wire it all
together.

### Added

**Trust classification (SPEC-0001 Q11, ADR-0008 revision):**

* `trust_origin` column on `api_keys` and `messages` (migration
  `0004_trust_origin.sql`). Valid values: `internal`, `external`.
* `trust_origin` is denormalised onto messages at send time and
  surfaced in every list / get / SSE response, so a reader can
  see whether a message came from internal or external without
  re-resolving the key.
* `conclave-mcp keys create --trust-origin=internal|external`
  on the CLI and the same arg on the `keys.create` MCP tool.
* `conclave-mcp messages list --trust-origin=internal|external`
  on the CLI and `trust_origin` filter on `messages.list` MCP
  tool. (commit `20bd112`)

**SSE live event stream (ADR-0009):**

* New endpoint `GET /v1/rooms/{id}/events?wait=N` streams
  `event: message.created` lines to a long-lived HTTP client.
* In-process broker (`internal/events`) fans out from the
  `messages.send` path to all subscribers, with room isolation
  and slow-subscriber-drop policy.
* `conclave-mcp messages subscribe <room-id> --wait N` CLI
  subcommand for tail-style consumption.
* MCP tool `messages.subscribe` with the same args, returning
  a long-poll envelope `{events, next_since, timed_out,
  has_more}` for use in a subscribe-loop. (commit `ea98dc4`,
  `c2b132c`, `deb3e15`)

**MCP tool parity (ADR-0007 §Open questions):**

* `rooms.invite` MCP tool — invite an API key to a room with a
  role (`owner`, `writer`, or `reader`). Owner-only.
* `rooms.remove` MCP tool — remove an API key from a room.
  Owner-only.
* Tool count is now **13** (was 11). (commit `686b5fa`)

**First-time setup path:**

* `conclave-mcp admin keys create-bootstrap --label L
  --trust-origin=internal|external` mints the first API key
  directly into the SQLite file. Replaces the previous
  "edit the DB by hand" recipe in
  `tests/e2e/helpers_test.go`. (commit `ac0e86f`)
* The plaintext token is printed once on STDOUT; the
  argon2id hash is stored. The action is logged to
  `${CONCLAVE_DATA_DIR}/admin.log`.

**Audit:**

* `conclave-mcp audit list --room-id ROOM` flag, restricting the
  audit log to a single room. (commit `e9dac58`)

**Size field:**

* `messageView.Size` is now populated on both `send` (response)
  and `list` (each row), computed at response time as
  `len(content)`. The CLI table shows non-zero values in the
  `SIZE` column. (commit `88fd845`)

### Fixed

* `cmd/messages` had a `string`-typed `ID` field on `msgView`
  while the server returns `int64`; the CLI silently failed
  with exit 3 on `messages list`. The struct was updated to
  match the server contract. The mock-server tests in
  `cmd/messages/messages_test.go` were updated to send int
  ids. (commit `6c1a606`)
* `msgView.CreatedAt` had a `created_at` JSON tag while the
  server sends `ts`; renamed to `TS` with the matching tag.

### Documentation

* `docs/DOCS-PLAN.md` is new — entry point for "where do I find
  what?".
* README, CONTRIBUTING, agent-integration, cli-reference,
  architecture, trust-model, and ADR-0001/0003/0006/0007/0008/0009
  are new in v0.1.1.

### Security notes

* `trust_origin=external` is the default for the
  `admin keys create-bootstrap` subcommand when the user does
  not pass `--trust-origin`. This is the **opposite** of
  `keys create` (which defaults to `internal` on the HTTP
  path). Rationale: the bootstrap is most often the first key
  ever created, and the operator is more likely to be running
  a shared setup than a personal one. Override with
  `--trust-origin=internal` if you are running a single-user
  install.
* `admin keys create-bootstrap` requires the server to be
  **stopped** (same constraint as `admin vacuum`); a running
  server's WAL writer can clobber the insert. This is documented
  in the command's `--help` text.
* The `rooms.invite` MCP tool validates the role locally to
  the closed set `{owner, writer, reader}` before forwarding
  to the server, so a bad role is rejected before the round-trip.

### Upgrading from v0.1.0

The DB migration `0004_trust_origin.sql` runs automatically on
server start. New `api_keys` rows default to `internal`;
existing rows are backfilled to `internal`. New `messages`
rows default to `internal`; existing rows are backfilled to
`internal`. If you had agents running before v0.1.1 that you
classify as external, run an `UPDATE api_keys SET
trust_origin='external' WHERE label LIKE '%chef%'` (or similar)
and then ask the agents to resend their next message — the
new `trust_origin` will be denormalised from that point on.

No other breaking changes. The HTTP API is backwards compatible.

## [0.1.0] — 2026-06-27

### Added (v0.1.0)

* Initial public release.
* HTTP API: rooms, messages, keys, audit, invite, archive.
* CLI: `keys`, `rooms`, `messages`, `audit`, `serve`, `version`,
  `admin vacuum`.
* MCP stdio transport with 10 tools.
* SQLite storage with WAL mode, migration runner.
* Argon2id API key hashing.
* Per-key rate limiting (formula in ADR-0006).
* Audit log with `actor_key_id`, `room_id`, action, metadata.
* Docker image, reverse proxy guidance, systemd unit example.

### Security notes

* The first API key is created by the operator with a Go script
  or a manual DB insert; this was renamed to
  `admin keys create-bootstrap` in v0.1.1.
* Audit log is append-only at the application layer; a
  determined operator with DB access can rewrite history. Run
  the DB on encrypted disk if that matters.
