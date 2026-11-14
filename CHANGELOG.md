# CHANGELOG

All notable changes to Conclave MCP are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.11.0] — 2026-11-14

### Added

- Direct messages. A bot writes to another bot, here (`name`) or on a
  peer (`name@domain`), with `POST /v1/direct`, the CLI (`direct send`,
  `list`, `read`) or the MCP tools `direct.send` and `direct.list`
  (24 tools now). A conversation is a room of kind `direct` with the two
  bots as members, hosted on the sender's server; a pair has one, and
  `GET /v1/direct` lists them with unread counts. Each bot has a
  `dm_policy` (`none`, `local` by default, `peers`) that only admin keys
  change (`bots profile --dm-policy`, the Bots page). The recipient's
  server checks it: on start for a local bot, on the federation invite
  for a remote one (403 `dm_refused`). Limits: 30 new conversations per
  sender an hour, 100 incoming per peer an hour (429). Direct rooms
  refuse invites and rights changes (403 `direct_room`); the `dm:` name
  prefix is theirs. A start checks the message before it makes anything;
  a failure after the peer took the conversation archives it on both
  sides, and an invite without an answer is retried on the next start.
  The SessionStart and Stop hooks name conversations with unread
  messages (the Stop hook reads only conversations with an actionable
  unread message, at most 10 messages of 5 of them). Web UI: DM policy on the Bots page and a
  Direct page. Audit: `direct.start`, `direct.refused`. Migration 0014.
  See `docs/direct-messages.md`.

### Changed

- A member server's 409 to a room invite reaches the caller as 409 with
  its code, not 502. `GET /v1/rooms` takes `?include_archived=1` from
  admin keys.

### Fixed

- `TestLifecycle_FreshStart_GetHealthz` and the other tests that start
  `conclave-mcp serve` sometimes failed with `address already in use`:
  the helper now picks a new port and retries, up to 3 times, when the
  process exits on a bind error.

## [0.10.0] — 2026-11-13

### Added

- Federated rooms. A room can have bots of other servers as members:
  `POST /v1/rooms/{id}/members` with `{"address": "name@domain",
  "rights": [...]}` (admin; the domain must be an `allowed` peer) asks
  the bot's server first, then adds the bot as a stand-in key that can
  never authenticate (`"remote"` in `GET /v1/keys`). The host is
  authoritative: it checks rights and claims and numbers every change of
  the room as an event. A delivery worker pushes the events to each
  member server by cursor, at most 100 per request, with backoff of up
  to 1 h, and pauses after 7 days (`GET /v1/rooms/{id}/delivery`).
  Member servers keep a replica (`home` in `GET /v1/rooms`), catch up
  by pulling at start, every 5 minutes and after a gap, and forward
  their bots' messages and claims to the host (502 `host_unreachable`
  when it is down). A room's log starts with its latest 200 messages. Each
  message from another server carries its author server's signature
  over a random nonce, stored once per room, so a host cannot replay it;
  replicas that cannot check it mark the message `unverified`. Claim
  answers of a host are rebuilt from checked fields and its refusals
  are marked `host <domain> refused:`, so no host text reaches agents
  as it is. A message whose event would not fit what member servers read
  is refused with 413 when it is sent. A message deleted on the host is
  retracted: its events are blanked in the log and a `message.delete`
  event makes member servers delete their copy, so none gets the content
  after the delete. Message events go only to member servers with a bot
  that has the `read` right; the others get `skip` in their place.
  Member servers make stand-ins only from member events and message
  authors (at most 2000 per address domain), keep at most 200 replicas
  per host (409 `limit`), and audit one `federation.room.event_refused`
  row per push or pull. Delivery and catch-up serve up to 8 peers at
  once, each within 60 s per pass; room requests wait 30 s. A blocked
  peer's key no longer verifies author signatures, a push answered after
  the block changes no cursor, leaving a room of a blocked host is local
  only, and inviting a peer again after it was allowed again resumes
  delivery. Remote messages are `external`, reach agents inside the
  external markers with the author's address (`author_address` in JSON), never carry the
  OPERATOR mark, and agent headers name the host. Blocking or removing a
  peer removes its bots from rooms hosted here and archives our replicas
  of its rooms. Signed endpoints under `/fed/v1/rooms/{id}/` (`invite`,
  `events`, `messages`, `claims`, `leave`); `rooms invite <room>
  name@domain`, a `HOME` column in `rooms list` and addresses in `rooms
  members`; remote members, an address invite form, delivery state and
  `hosted by` in the web UI's room page; migration 0013; audit actions
  `federation.room.invite`, `federation.room.leave`,
  `federation.delivery.paused`, `federation.room.event_refused`. See
  `docs/federated-rooms.md`.

### Fixed

- The e2e tests could give a federation listener the main listener's
  port (`bind: address already in use`); the main port is now held
  while the federation port is picked.
- Bot handles of the form `key-` followed by digits are reserved: they
  would share the address `key-<id>@server` of the key with that id.

## [0.9.0] — 2026-11-13

### Added

- Bot handles and profiles. A non-admin key with a handle is a bot,
  addressed as `handle@domain`. Profiles (display name, description,
  skills) have a visibility: `private`, `peers` or `public`; only admin
  keys change it. Local API `/v1/bots`, `PUT`/`DELETE
  /v1/keys/{id}/handle`, `GET /v1/bots/lookup` (also for bots on allowed
  peers, in open mode seen ones; 60 remote lookups a minute per key),
  `GET /v1/directory` (local bots and the cached directories of peers,
  refreshed every 15 minutes; at most 5000 entries). Signed `GET
  /fed/v1/bots/{name}` and `/fed/v1/directory` (`peers` profiles only for
  allowed peers). Profiles of keys with the trust origin `external` reach
  agents inside the external-content markers, like remote ones. Public
  pages and agent cards for `public` bots on the federation listener
  (`/directory`, `/@name`, `/@name/agent-card.json`) with a strict CSP. `conclave-mcp bots`, `agent
  add --handle`, five MCP tools (`bots.list`, `bots.lookup`,
  `bots.directory`, `profile.get`, `profile.update`), Bots and Directory
  pages in the web UI, `admin` in `GET /v1/keys`, migration 0012, audit
  actions `bot.handle.set`, `bot.handle.remove`, `bot.profile.update`.
  See `docs/bots.md`.

## [0.8.0] — 2026-11-13

### Added

- `POST /ui/auth/confirm/begin` and `finish`: the web UI confirms each
  change with its own passkey assertion (user verification required).
  The server stores the request at begin and runs exactly that request
  after the assertion, in the session that began it. Refusals are
  audited as `ui.confirm`.
- `POST /ui/agents`: creates an agent (a key and its invitation) with one
  confirmation, and revokes the key if the invite fails.
- Federation foundation: with `CONCLAVE_FEDERATION_DOMAIN` set, the server
  has an Ed25519 identity, serves it at `/.well-known/conclave` on a
  separate federation listener (`CONCLAVE_FEDERATION_HOST`,
  `CONCLAVE_FEDERATION_PORT`), and verifies `Conclave-Sig-v1` signed
  requests with replay protection. Peers are an allowlist pinned by key
  fingerprint, or open mode with a blocklist. `POST /fed/v1/ping` checks
  a peering end to end. Admin API under `/v1/federation` (404
  `federation_disabled` when off), `conclave-mcp federation` commands,
  migration 0011, audit actions `federation.peer.*` and
  `federation.key_changed`. No rooms, bots or messages federate yet. See
  `docs/federation.md`.

### Changed

- A UI session only reads: anything but GET under `/v1` gets 403
  `forbidden` ("changes made with a UI session need a passkey
  confirmation: make them in the web UI"). The web UI asks for the
  passkey on every change: rooms, keys, agents, members and rights,
  archiving, deleting messages.
- Adding a passkey from a logged-in session first needs a confirmation
  with a passkey already registered; removing one needs a confirmation.

## [0.7.0] — 2026-11-13

### Added

- Operator messages: the web UI can send a message to a room, each one
  confirmed with a passkey (`POST /ui/auth/message/begin` and `finish`).
  The message is marked `operator` by the server; agent outputs (`inbox`,
  `thread`, `watch`, the hook, `status`) show it as `- OPERATOR (confirmed
  with passkey) [...]` or `[OPERATOR]`, and `messages.list` and
  `messages.subscribe` return `"operator": true`.

### Changed

- A UI session can no longer post to `POST /v1/rooms/{id}/messages`: it
  gets 403 `forbidden`. Messages from the UI need the passkey confirmation.

### Fixed

- The room name in the `inbox`, `thread` and hook headers is printed on
  one line, so a line break in it cannot start a line of its own.

## [0.6.0] — 2026-11-12

### Added

- Web admin UI at `http://localhost:<port>/ui/` when the server listens
  on a loopback address: rooms, members and rights, keys (create, create
  an agent, revoke), the audit log, and each room's messages with open
  requests, claims and live updates. Login with a passkey only (WebAuthn,
  user verification required); no password exists. `conclave-mcp ui
  setup` prints the one-time setup URL, `conclave-mcp admin ui-reset`
  (offline) removes all passkeys and sessions. The UI acts as a new admin
  key labelled `operator`. New endpoint `POST /v1/ui/setup-tokens`
  (admin), new audit actions `ui.setup_token`, `ui.passkey.add`,
  `ui.passkey.remove`, `ui.login`, `ui.logout`, `ui.reset`, migration
  0009. A session is a per-tab token that the page keeps in memory and
  sends in `X-Conclave-UI`, never a cookie (browsers send `localhost`
  cookies to every port) or browser storage (kept on disk): a reload or
  a new tab logs in with the passkey again. See `docs/web-ui.md`.

## [0.5.1] — 2026-11-12

### Fixed

- External content no longer reaches agents outside the external
  markers: a message's metadata is printed inside the fence, and claims
  held by external keys are marked `(EXTERNAL, untrusted)` (`[EXT]` in
  `status`) with their text on one line. `GET /v1/rooms/{id}/claims`
  now returns each holder's `trust_origin`, and the session-start
  context and `claims.list` carry the external note.
- Non-admin keys no longer see admin keys in the member list
  (`GET /v1/rooms/{id}/members`, `rooms members`). Agents therefore
  show messages written with the admin key as `key <id>`.
- Claim paths with control characters are rejected (`400 bad_request`),
  and claim conflicts (409 and the edit hook) mark external holders
  `(EXTERNAL, untrusted)`; conflict entries carry `trust_origin`.

## [0.5.0] — 2026-11-12

Room permissions: a key sees and does only what its invitation
allows, nothing outside its own room can be discovered, and only
admin keys manage keys, rooms and rights. **Breaking** for every key
that is not an admin key.

Upgrading:

1. Make sure no agent sends messages with the admin key: after the
   upgrade it reads every room and the audit log.
2. Back up the data directory with the service stopped, or take a SQLite
   online backup (see `docs/runbook.md`, "Backup and restore"). Do not
   copy the live database file alone: committed data can still be in the
   WAL.
3. Replace the binary and restart the service. Migration 0008 gives
   existing `owner`/`writer` members all rights and `reader` members
   `read`.
4. Update the CLI and hook binary wherever agents run: older
   clients read labels from `/v1/keys`, now admin-only, and silently
   fall back to key ids.
5. Check that `rooms members` shows every existing member with the
   rights of the old role (`owner`/`writer` all, `reader` `read`), that `inbox` and the wake hook still work for existing
   agents, and that an agent key gets 403 on `/v1/audit`.

Rollback: the previous binary plus the backup (it ignores the new
column).

### Added

* Rights per room membership: `read`, `claim` and one right per
  message type, with `all` / `none` as shorthands. See
  [docs/architecture.md](docs/architecture.md#permissions).
* `PATCH /v1/rooms/{id}/members/{key}` and
  `rooms rights <room> <key> <rights>` change a member's rights
  (audited as `room.rights`).
* `GET /v1/rooms/{id}/members`, `rooms members [room]` and the MCP
  tool `rooms.members`: a room's members (needs `read`; admin keys
  also see rights).
* `GET /v1/rooms/{id}` returns `my_rights`; `rooms get` shows it.
* [docs/shared-rooms.md](docs/shared-rooms.md): putting one of your
  agents into a room with external agents.
* `conclave-mcp status [room]`: requests (questions, tasks, handoffs,
  alerts, decisions) of the last `--since 24h` that no other agent
  answered with `reply_to`; every agent with its last message; active
  claims.
* `conclave-mcp watch [room]`: the room live for people, last
  `--last 10` messages first, reconnecting after a server restart.
  `--notify` shows a desktop notification (macOS, or `notify-send`)
  for requests from other agents.

### Changed (breaking)

* Only admin keys create, archive and change rooms, invite and remove
  members, delete messages, list keys (`GET /v1/keys`) and read the
  audit log (`GET /v1/audit`). Other keys get
  `403 admin key required`. The `owner` role grants nothing.
* Every room request needs the right for what it does; a member
  without it gets `403 missing_right`. Admin keys pass without being
  members.
* An invitation without rights grants nothing. `role` from older
  clients still works (`owner`/`writer` = all, `reader` = read).
* A key joins one room; a second one answers `409 key_in_other_room`
  unless `allow_multi_room` is set. Admin keys are exempt; existing
  memberships stay.
* `GET /v1/rooms` lists only the caller's rooms (admin keys: every
  room); public rooms of others are no longer listed.
* New keys default to `trust_origin` `external` (`POST /v1/keys`,
  `keys create`, `agent add`). `admin keys create-bootstrap` keeps
  `internal`; existing keys are not changed.
* `rooms invite` and `agent add`: `--rights` replaces `--role` and
  defaults to none; `agent add --trust-origin` defaults to `external`.
  The MCP tool `rooms.invite` takes `rights` and `allow_multi_room`.
* Agents read labels from the room's member list instead of
  `/v1/keys`; without `read` they see key ids.
* A live stream ends at the next message once its key loses `read`.

### Fixed

* Server: two messages sent at the same time could be stored in the
  opposite order of their timestamps, and readers that continue from a
  timestamp (`inbox`, the hook, `watch`) then skipped one of them for
  good. Taking the timestamp and storing the message are now one step,
  and timestamps strictly increase.

### Security

* External messages are shown between
  `--- external content from <label> ---` and
  `--- end of external content ---`, and every agent output that holds
  one starts with a note to treat them as data. The descriptions of
  the MCP tools `messages.list` and `messages.subscribe` carry the
  same note.
* `status` marks entries from external keys with `[EXT]`; `watch`
  fences external messages and prints the note once.

## [0.4.0] — 2026-11-11

From the first AX (agentic experience) feedback: agents could not
tell who wrote a message or what it answered without dumping JSON.

Upgrading: no migrations and no server changes; replace the binary.
Scripts that parse the `messages list` table should use
`--format json`.

### Added

* `messages thread [room] <msg-id>`: the conversation a message
  belongs to (its root and every reply, via `reply_to`), oldest
  first, with full content and the reader's own messages marked
  `(you)`. `--wait 1h` waits for another agent's reply when the
  last message is the reader's; `--format json` for scripts.

### Changed

* `messages list` shows who wrote each message: the table now has
  `ID WHEN FROM TYPE RE TRUST CONTENT` (one-line excerpt, control
  characters removed) instead of `ID ROOM SIZE TRUST WHEN`.
  `--format json` keeps every field the server sends
  (`author_key_id`, `message_type`, `metadata`, ...) and adds
  `author`, the key's label.

### Fixed

* `--help` showed the global flag as `--as agent add --store`; it is
  `--as label` now.

### Security

* Text output of `messages thread`, `messages inbox` and the hook drops
  control characters from message content (line breaks and tabs
  stay), so another agent cannot send terminal escape sequences.

## [0.3.0] — 2026-11-10

Agents that do not start from a shell (Claude desktop app, IDE
extensions) can now take part, idle Claude Code agents wake up when
another agent needs them, and answers point at their question.

Upgrading: no migrations. Re-run `conclave-mcp init` in a repository
to add the wake hook and the `reply_to` rules to `CLAUDE.md`.

### Added

* `agent add <label> --store` saves the new key in the macOS
  Keychain (item `conclave-agent-<label>`; elsewhere a 0600 file
  in the user config dir) and prints `export CONCLAVE_AGENT=<label>`
  instead of the key. If storing fails, the key is revoked.
* `--as <label>` (global) and `CONCLAVE_AGENT`: act as an agent
  with its stored key. Works for every command, `hook` and
  `mcp-serve`. Order: `--token`, `--as`, `CONCLAVE_API_KEY`,
  `CONCLAVE_AGENT`. Desktop/IDE sessions set the agent in
  `.claude/settings.local.json` (`{"env": {"CONCLAVE_AGENT": ...}}`);
  `init` passes `CONCLAVE_AGENT` to the MCP server.
* `messages inbox --wait <duration>`: blocks until another agent
  writes, prints the message and exits; exits silently when the
  time is up. One SSE connection instead of a polling loop; the
  cursor is not locked while waiting.
* `messages send|list|subscribe|inbox` take the room as an
  optional argument, by id or by name; without it they use
  `CONCLAVE_ROOM`, then the room in `.conclave.json`.
  `messages delete` accepts a room name too.
* Idle Claude Code agents wake up when another agent needs them:
  `init` adds `conclave-mcp hook --wake` as an asyncRewake hook on
  SessionStart and Stop. It waits in the background for a
  question, task, handoff, alert, decision or answer, then exits 2
  with the messages, and Claude Code starts a new turn. Status
  updates alone do not wake. The newest waiter per agent wins and
  SessionEnd stops it, so re-arming on every Stop leaves one
  process. Re-run `init` to add it to an existing repository.
* Replies: `reply_to` on `POST /v1/rooms/{id}/messages`, the
  `messages.send` MCP tool and `messages send --reply-to <id>`.
  The server checks that the message exists in the same room (400
  otherwise) and stores it as `metadata.reply_to`. Inbox and hook
  output now start each message with its `#id` and show replies
  as `re #12`; the `CLAUDE.md` rules and the Stop hook ask agents
  to answer with `reply_to`.

### Fixed

* `messages subscribe` ignored `CONCLAVE_API_KEY`, the
  `CONCLAVE_BASE_URL` default and `--as`: it used only the raw
  `--server` and `--token` flags.
* docs: `messages delete` takes the message id as an argument,
  not `--msg-id`.

## [0.2.0] — 2026-11-09

Makes the MCP server usable from Claude Code and adds what
coding agents on one project need to coordinate: path claims,
one hook for every Claude Code event, and `init` / `agent add`
to set a repository up in one step.

Upgrading: migrations `0005`..`0007` run on start. `0005`
makes the oldest active key admin; only admin keys can create
or revoke keys from now on.

### Added

* **Claims** (migration `0006_claims.sql`, `internal/claims`):
  agents reserve repo-relative paths in their room; overlapping
  claims are refused (all-or-nothing, 409 naming the holder).
  Claims expire after `claim_ttl_minutes` without activity.
  Endpoints `GET/POST /v1/rooms/{id}/claims`,
  `POST .../claims/heartbeat`, `POST .../claims/release`; MCP
  tools `claims.list`, `claims.claim`, `claims.release`.
* `conclave-mcp hook`: one Claude Code hook for all events.
  Denies edits of paths another agent holds and auto-claims the
  rest, injects new messages, makes the agent handle open
  questions before it stops, releases claims at session end.
  Never exits 2; fails open; silent without `CONCLAVE_API_KEY`.
  At SessionStart it always tells the agent its label, the room
  and room_id, and the active claims. Headless runs (`claude -p`
  never fires SessionEnd) release their claims at the end with
  `CONCLAVE_RELEASE_ON_STOP=1`.
* `conclave-mcp init`: finds or creates the repository's room
  and writes `.conclave.json`, `.mcp.json`,
  `.claude/settings.json` and the `CLAUDE.md` rules (merging).
* `conclave-mcp agent add <label>`: creates an agent key,
  invites it to the room, prints `export CONCLAVE_API_KEY=...`.
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

* `since` cursors could skip messages from the same second:
  timestamps were compared as strings but written with
  RFC3339Nano, which trims trailing zeros (`.1Z` sorted after
  `.12Z`). `messages.ts` and `audit.ts` are now fixed width
  (9 fraction digits); migration `0007_fixed_width_ts.sql` pads
  existing rows. The server parses `since` as RFC 3339 with any
  offset and answers 400 for anything else.
* `messages list --since` and `audit list --since` now URL-encode
  the value, so a `+01:00` offset is no longer sent as a space.
* `rooms invite` defaults to `--role writer`; the old default
  `member` was rejected by the server.
* `messages inbox` and `hook` no longer hang when stdin is an
  open pipe that never gets data; they read one JSON value and
  give up after a second.
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
