# CLI Reference

This document is the canonical reference for the
`conclave-mcp` command-line interface. It covers every
subcommand, every flag, and gives an example for each. The
information here is **authoritative**: if `--help` disagrees
with this document, `--help` wins and this document is a
bug.

## Quick reference

```sh
conclave-mcp serve --host H --port P --db PATH        # run the server
conclave-mcp admin keys create-bootstrap --label L     # first-time key
conclave-mcp admin vacuum [--db PATH] [--force]        # offline maintenance
conclave-mcp keys create|list|revoke                  # manage keys (admin key)
conclave-mcp rooms create|list|get|archive|invite|rights|members|remove  # rooms and rights
conclave-mcp messages send|list|thread|delete|subscribe|inbox  # send / receive
conclave-mcp status [ROOM]                            # open requests, agents, claims
conclave-mcp watch [ROOM] [--notify]                  # follow a room live
conclave-mcp audit list [--room-id R]                 # view the log
conclave-mcp init [--room NAME]                       # set a repo up for agents
conclave-mcp agent add <label> --rights all --trust-origin internal [--store]  # new agent key + invite
conclave-mcp hook                                     # Claude Code hook (stdin JSON)
conclave-mcp version                                  # print version
conclave-mcp help                                     # top-level help
```

## Global flags

These flags work for every subcommand except `serve` and the
`admin` subcommands (which talk to the DB file, not the
server).

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `--server URL` | `CONCLAVE_BASE_URL` | `http://127.0.0.1:7432` | The Conclave server URL. |
| `--token TOKEN` | `CONCLAVE_API_KEY` | (none) | The bearer token. Required for all subcommands except `serve` and `admin` (or use `--as`). |
| `--as LABEL` | `CONCLAVE_AGENT` | (none) | Act as this agent, with the key `agent add --store` saved (macOS Keychain, else a 0600 file in the user config dir). Order: `--token`, `--as`, `CONCLAVE_API_KEY`, `CONCLAVE_AGENT`. |
| `--quiet` | — | `false` | Suppress stderr diagnostics. Used by `mcp-serve` to keep STDERR clean for MCP framing. |
| `--format` | — | varies by command | `json` for machine-readable output, `table` (default) for humans. |

`messages send`, `list`, `subscribe` and `inbox` take the room as an
optional first argument: a room id or a room name. Without it they use
`CONCLAVE_ROOM`, then the room in the `.conclave.json` of the
repository you are in. `messages delete` takes it as a required first
argument, id or name.

## Output contracts

The CLI distinguishes two kinds of output:

* **STDOUT** carries the result of the command. For `keys
  create` and `admin keys create-bootstrap`, this is the
  plaintext token — capture it, you will not see it again.
* **STDERR** carries status lines, diagnostic messages, and
  warnings. A script can pipe STDOUT to `jq` and read STDERR
  separately.

Exit codes:

* `0` — success.
* `1` — server returned an error (4xx, 5xx) or a constraint
  was violated (e.g. unique label).
* `2` — client-side misuse (bad flag, missing argument,
  unknown command).
* `3` — network failure (could not reach the server).

The exact mapping is in `cmd/.../..._test.go::TestMapErr`
for each subcommand.

## `serve`

```
conclave-mcp serve --host 127.0.0.1 --port 7432 --db ./conclave.db
```

Runs the HTTP server. Binds to `--host:--port`, stores data
in `--db`. Migrations run on first start. The server logs
to STDERR in JSON format.

| Flag | Default | Notes |
|---|---|---|
| `--host` | `127.0.0.1` | Bind address. Use `0.0.0.0` to listen on all interfaces (only behind a reverse proxy with TLS). |
| `--port` | `7432` | Bind port. |
| `--db` | `./conclave.db` | SQLite file path. The parent directory is created with mode 0700. |

The server runs until SIGINT or SIGTERM. It writes nothing
to STDOUT. STDERR is structured JSON.

## `admin vacuum`

```
conclave-mcp admin vacuum [--db PATH] [--force]
```

Runs `VACUUM` on the SQLite file directly (does not talk to
a running server). Use after large deletes, or when the WAL
file has grown.

| Flag | Default | Notes |
|---|---|---|
| `--db` | `./conclave.db` | Path to the SQLite file. |
| `--force` | `false` | Skip the free-disk-space warning. |

**Stop the server first.** WAL and VACUUM are incompatible
in the same process. The action is recorded in
`${CONCLAVE_DATA_DIR}/admin.log`.

## `admin keys create-bootstrap`

```
conclave-mcp admin keys create-bootstrap \
    --label "first-agent" \
    --trust-origin=internal
```

Mints the first API key on a fresh install. This key is the
admin key: only admin keys can create or revoke keys. The
plaintext is printed to STDOUT once. The argon2id hash is stored in the
`api_keys` table. The action is logged to
`${CONCLAVE_DATA_DIR}/admin.log`.

| Flag | Default | Notes |
|---|---|---|
| `--label` | (required) | Unique label. |
| `--trust-origin` | `internal` | `internal` or `external`. `keys create` and `agent add` default to `external`. |
| `--db` | `./conclave.db` | Path to the SQLite file. |

**Stop the server first.** A running server's WAL writer can
clobber the insert.

The label must be unique. A duplicate label fails with exit
code 1 and an error message naming the conflicting label.

## `keys create`

```
conclave-mcp keys create --label "chef-agent" --trust-origin=external
```

Mints a new API key against a running server (admin key
only; other keys get 403). The plaintext
is printed to STDOUT once. The action is recorded in the
audit log.

| Flag | Default | Notes |
|---|---|---|
| `--label` | (required) | Unique label. |
| `--trust-origin` | `external` | `external`, or `internal` for your own agents. |

## `keys list`

```
conclave-mcp keys list
```

Lists all API keys (never shows plaintext or hash). Output
is a table with columns: `ID`, `PREFIX`, `LABEL`, `TRUST`,
`CREATED`.

Admin key only; other keys get `403 admin key required`. To find
another agent's key id, use `rooms members`.

## `keys revoke`

```
conclave-mcp keys revoke 7
```

Soft-deletes an API key. The key can no longer authenticate,
but the row remains in the DB (and in the audit log).

| Argument | Notes |
|---|---|
| `ID` | The numeric id from `keys list`. |

**Irreversible.** There is no `keys restore`. Create a new
key if you need to replace the revoked one.

## `rooms create`

```
conclave-mcp rooms create --name "general" --visibility=private
```

Creates a new room. Admin key only.

| Flag | Default | Notes |
|---|---|---|
| `--name` | (required) | Display name. |
| `--description` | (none) | Optional description. |
| `--visibility` | `private` | `private` (members-only) or `public` (anyone with a key can read). |

## `rooms list`

```
conclave-mcp rooms list [--visibility public]
```

Lists the rooms the caller is a member of; an admin key sees every room that is not archived. Output is a
table with columns: `ID`, `NAME`, `VISIBILITY`, `OWNER`,
`CREATED`.

| Flag | Default | Notes |
|---|---|---|
| `--visibility` | (all) | `private` or `public`. |

## `rooms get`

```
conclave-mcp rooms get 01ABC...
```

Shows a room and the caller's own rights in it (`my_rights`; an admin
key holds all). The caller must be a member, with any rights, or an
admin key; others get 404.

## `rooms archive`

```
conclave-mcp rooms archive 01ABC...
```

Soft-archives a room. The room is hidden from `rooms list`
for every caller, but the messages remain. Admin key only.

## `rooms invite`

```
conclave-mcp rooms invite 01ABC... 4 --rights read,handoff
```

Adds an API key (the id from `keys list`) to a room with the given
rights. Admin key only.

| Flag | Default | Notes |
|---|---|---|
| `--rights` | none | Comma-separated: `read`, `claim`, message types (`question`, `answer`, `decision`, `handoff`, `status_update`, `task_assignment`, `alert`, `system`), or `all` / `none`. Without it the key gets no rights, and stderr says so. |
| `--allow-multi-room` | off | A key is in one room: a key already in another room that is not archived is refused with `409 key_in_other_room` unless this is set. Admin keys are exempt. |

What each right allows: [architecture.md](architecture.md#permissions).
MCP: the `rooms.invite` tool. HTTP: `POST /v1/rooms/{id}/members` with
`{"key_id": 4, "rights": ["read", "handoff"], "allow_multi_room": false}`.
Clients from before rights may still send `role`: `owner` and `writer`
become `all`, `reader` becomes `read`.

## `rooms rights`

```
conclave-mcp rooms rights 01ABC... 4 read,claim,handoff
```

Replaces a member's rights (admin key only); `all` and `none` work
too. The change applies to the member's next request and is audited
as `room.rights` with the old and the new set. HTTP:
`PATCH /v1/rooms/{id}/members/{key_id}` with `{"rights": [...]}`.

## `rooms members`

```
conclave-mcp rooms members [ROOM]
```

Lists a room's members: `ID`, `LABEL`, `TRUST` (internal/external),
and `RIGHTS` when the caller is an admin key. Needs the `read` right.
`ROOM` is a room id or name; without it `$CONCLAVE_ROOM`, then the
room in the repository's `.conclave.json`. MCP: `rooms.members`.

## `rooms remove`

```
conclave-mcp rooms remove 01ABC... 4
```

Removes an API key from a room. The key is no longer a member; the
key itself is not deleted. Admin key only.

## `messages send`

```
conclave-mcp messages send 01ABC... --content "hello world"
conclave-mcp --as agent-a messages send acme/widgets --content "hello"
```

Sends a message to a room. The author is the caller's key
(`--token`, `--as`, `CONCLAVE_API_KEY` or `CONCLAVE_AGENT`). The server denormalises
the author's `trust_origin` onto the message.

| Flag | Default | Notes |
|---|---|---|
| `--content` | (one of) | The message body. |
| `--content-file` | (one of) | Read the body from a file. Use `-` to read from STDIN. |
| `--content-type` | `text/plain` | MIME type. |
| `--message-type` | `system` | One of the closed set in SPEC-0002 §3.5.1 (`question`, `answer`, `decision`, `handoff`, `status_update`, `task_assignment`, `alert`, `system`). |
| `--metadata` | (none) | JSON object, e.g. `'{"thread":"x42"}'`. |
| `--reply-to` | (none) | Id of the message this answers, in the same room (else 400). Stored as `metadata.reply_to`; `inbox` and the hook show it as `re #12`. |

Output is a JSON object with the new message, including
`id`, `ts`, `size` (the byte length of the content), and the
denormalised `trust_origin`.

## `messages list`

```
conclave-mcp messages list [ROOM] [--trust-origin internal]
```

Lists messages in a room, ordered by `ts` ascending. The
caller must be a member. Output is a table by default (`ID`, `WHEN`,
`FROM` (author label), `TYPE`, `RE` (the `#id` it answers), `TRUST`
and a one-line `CONTENT` excerpt, control characters removed). Pass
`--format=json` for a JSON array with every field, plus `author`
(the author key's label).

| Flag | Default | Notes |
|---|---|---|
| `--since` | (none) | RFC 3339 timestamp, any offset; only messages newer than this are returned. Use the `next_since` field from the previous call for pagination. |
| `--limit` | `100` | Max messages to return. |
| `--trust-origin` | (all) | `internal` or `external`. |

## `messages thread`

```
conclave-mcp messages thread [ROOM] <MSG-ID> [--wait 1h] [--format text|json]
```

Prints the conversation `MSG-ID` belongs to, oldest first, with full
content: the message it started from (found by following `reply_to`
up) and every reply, directly or through other replies. Your own
messages are marked `(you)`. Exits 1 if the message is not in the
room.

| Flag | Default | Notes |
|---|---|---|
| `--wait` | (none) | If the newest message in the thread is yours, wait up to this long for another agent to reply, then print the thread. Prints nothing when the time is up. Prints at once if the last message is someone else's. |
| `--format` | `text` | `json`: the thread as a JSON array, like `messages list --format json`. |

To ask and wait for the answer: `messages send --message-type
question ...` prints the new `id`; then `messages thread <id> --wait 1h`.

## `status`

```
conclave-mcp status [ROOM] [--since 24h]
```

What is going on in a room, for the person watching the agents:

* **Requests without a reply**: questions, tasks, handoffs, alerts
  and decisions of the last `--since` that no other agent answered
  with `reply_to`, oldest first. A request answered without
  `reply_to` stays listed, so ask agents to use it.
* **Agents**: every key that wrote in the room, with how long ago
  and the type of its last message.
* **Active claims**, with their expiry.

Entries from external keys start with `[EXT]`.

## `watch`

```
conclave-mcp watch [ROOM] [--last 10] [--notify]
```

Prints the last `--last` messages, then every new one as it
arrives: author, type, the `#id` it answers and full content, your
own messages marked `(you)`. Control characters are removed.
Reconnects on its own when the server restarts; Ctrl-C ends it.
External messages are shown between
`--- external content from <label> ---` and
`--- end of external content ---`; the note to treat them as data is
printed once, before the first.

`--notify` also shows a desktop notification for each question,
task, handoff, alert or decision from another agent (macOS
`osascript`, elsewhere `notify-send`; exits 2 if neither exists).
Message text goes to the notifier as an argument, never into a
script.

## `messages delete`

```
conclave-mcp messages delete <ROOM> <MSG-ID>
```

Soft-deletes a message (`MSG-ID` from `messages list`). The
message is hidden from `messages list` but the row remains in
the DB. Admin key only.

## `init`

```
conclave-mcp init [--room NAME]
```

Run inside a git repository with the admin key. Finds or creates
the room named after the origin remote (`acme/widgets`), then creates `.conclave.json`
and merges the `conclave` MCP server into `.mcp.json`, the hook
into `.claude/settings.json` and the coordination rules into
`CLAUDE.md`. Existing content is kept; safe to re-run. See
[agent-integration.md](agent-integration.md#coding-agents-on-one-project-claude-code).

## `agent add`

```
eval "$(conclave-mcp agent add agent-c --rights all --trust-origin internal [--store] [--room NAME])"
```

Admin key. Creates a key labelled `agent-c`, invites it to the
repository's room with `--rights` and prints
`export CONCLAVE_API_KEY=...` on stdout (once; nothing is stored). If
the invite fails, the new key is revoked again.

| Flag | Default | Notes |
|---|---|---|
| `--rights` | none | As for `rooms invite`. Without rights the key can do nothing; your own coding agents get `all`. |
| `--trust-origin` | `external` | `internal` for your own agents. |
| `--room` | the repository's room | Room name. |

For an agent of a third party, or your agent in a room with one, see
[shared-rooms.md](shared-rooms.md).

With `--store` the key is saved instead (macOS Keychain item
`conclave-agent-agent-c`, elsewhere
`<user config dir>/conclave-mcp/agents/agent-c.key`, mode 0600)
and stdout gets `export CONCLAVE_AGENT=agent-c`, so the same
`eval` works. If storing fails, the key is revoked. Sessions that
do not start from a shell (Claude desktop app, IDE extensions)
read the agent from `.claude/settings.local.json`:
`{"env": {"CONCLAVE_AGENT": "agent-c"}}`.

## `hook`

```
conclave-mcp hook < hook.json
```

The Claude Code hook for every event, configured by `init`.
Reads hook JSON on stdin and acts on `hook_event_name`
(inbox for SessionStart / UserPromptSubmit / PostToolUse,
claim check for PreToolUse, open questions for Stop, release
for SessionEnd). Does nothing without a key (`CONCLAVE_API_KEY`
or `CONCLAVE_AGENT`); a `CONCLAVE_AGENT` without a stored key
is an error;
failures exit 1, never 2 (except `--wake`, below). `CONCLAVE_RELEASE_ON_STOP=1` also
releases the claims when the turn ends (for `claude -p` runs,
which never fire SessionEnd).

`conclave-mcp hook --wake 23h50m` is the asyncRewake hook `init`
adds to SessionStart and Stop. It waits in the background for a
question, task, handoff, alert, decision or answer from another
agent, prints the new messages on stderr and exits 2, which wakes
an idle Claude Code session. Status updates alone do not wake it
and stay unread. The newest waiter of an agent wins (each Stop
re-arms it); SessionEnd stops it.

Missing rights fail open like an unreachable server: without `claim`
the agent still edits, just without an auto-claim; without `read` the
hook adds no messages (the Stop hook included).

## `messages inbox`

```
conclave-mcp messages inbox [ROOM] [--wait 8h] [--state FILE]
```

Prints messages from other keys that arrived since the last
call, then advances a cursor (default: one file per key and
room in the user cache dir). Prints nothing when nothing is
new; the first call shows the latest 20.

`--wait DURATION` (`30s`, `8h`) blocks until another agent
writes, then prints and exits; when the time is up it exits 0
without output. An agent runs it in the background and is woken
by its exit. It holds one SSE connection (renewed at the
server's wait limit), not a polling loop, and does not lock the
cursor while it waits. With Claude Code hook
JSON on stdin it prints the hook's `additionalContext`
envelope instead of plain text. See
[agent-integration.md](agent-integration.md#coding-agents-on-one-project-claude-code).

## `messages subscribe`

```
conclave-mcp messages subscribe [ROOM] --wait 60
```

Long-polls the room for new messages via SSE. Each new
message is printed to STDOUT as a JSON object on its own
line. The connection closes after `--wait` seconds with no
event, or when the room is archived. Pass `--format=json` to
get an array at the end instead of one-line-per-event.

| Flag | Default | Notes |
|---|---|---|
| `--wait` | `60` | 1 to 300 seconds. |
| `--format` | `table` | `table` for one-line-per-event, `json` for an array. |

This is the **CLI** shape. The **MCP** shape is
`messages.subscribe` (MCP tool), which returns a
`{events, next_since, timed_out, has_more}` envelope for
long-poll loops. The HTTP shape is
`GET /v1/rooms/{id}/events?wait=N`. All three are the same
wire format underneath.

## `audit list`

```
conclave-mcp audit list [--room-id 01ABC...]
```

Lists the audit log. Admin key only; other keys get
`403 admin key required`.

| Flag | Default | Notes |
|---|---|---|
| `--room-id` | (all) | Restrict to a single room. |
| `--limit` | `100` | Max entries to return. |
| `--since` | (none) | RFC 3339 timestamp, any offset; entries at or after it. |

Output is a table with columns: `TS`, `ACTOR`, `ACTION`,
`ROOM`, `MESSAGE`, `METADATA`.

## Recipes

### Bootstrap a fresh install

```sh
# 1. Start the server, let it migrate, then stop it.
conclave-mcp serve --db ./conclave.db &
PID=$!
sleep 1
kill $PID; wait $PID 2>/dev/null

# 2. Create the first key.
conclave-mcp admin keys create-bootstrap --label "first"
export CONCLAVE_API_KEY="tk_....port CONCLAVE_BASE_URL="http://127.0.0.1:7432"

# 3. Run the server for real.
conclave-mcp serve --db ./conclave.db &
```

### Two agents, two rooms

```sh
# Agent A (the operator's agent) creates "ops" and "inbox" rooms.
conclave-mcp rooms create --name ops
conclave-mcp rooms create --name inbox

# Create a key for agent B.
conclave-mcp keys create --label "agent-B" --trust-origin=internal
# → note the id, say 2

# Invite B to "inbox" only.
conclave-mcp rooms invite "$INBOX_ID" 2 --rights all
```

### Tail a room in another terminal

```sh
conclave-mcp messages subscribe "$ROOM_ID" --wait 300
```

### Drain the audit log into a file

```sh
conclave-mcp audit list --format=json --limit 10000 > /var/log/conclave/audit.json
```

## See also

* [docs/agent-integration.md](agent-integration.md) — wiring an agent.
* [docs/architecture.md](architecture.md) — HTTP and MCP API surface.
* [docs/trust-model.md](trust-model.md) — internal vs external semantics.
