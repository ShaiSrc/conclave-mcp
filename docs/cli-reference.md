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
conclave-mcp keys create|list|revoke                  # manage keys
conclave-mcp rooms create|list|get|archive|invite|remove  # manage rooms
conclave-mcp messages send|list|delete|subscribe|inbox  # send / receive
conclave-mcp audit list [--room-id R]                 # view the log
conclave-mcp init [--room NAME]                       # set a repo up for agents
conclave-mcp agent add <label> [--store]              # new agent key + invite
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

`messages send`, `list`, `subscribe` and `inbox` take the room as an
optional first argument: a room id or a room name. Without it they use
`CONCLAVE_ROOM`, then the room in the `.conclave.json` of the
repository you are in. `messages delete` takes it as a required first
argument, id or name.
| `--quiet` | — | `false` | Suppress stderr diagnostics. Used by `mcp-serve` to keep STDERR clean for MCP framing. |
| `--format` | — | varies by command | `json` for machine-readable output, `table` (default) for humans. |

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
| `--trust-origin` | `external` | `internal` or `external`. Note: this is the **opposite** default of `keys create` (which defaults to `internal`). |
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
| `--trust-origin` | `internal` | `internal` or `external`. |

## `keys list`

```
conclave-mcp keys list
```

Lists all API keys (never shows plaintext or hash). Output
is a table with columns: `ID`, `PREFIX`, `LABEL`, `TRUST`,
`CREATED`.

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

Creates a new room. The caller becomes the owner.

| Flag | Default | Notes |
|---|---|---|
| `--name` | (required) | Display name. |
| `--description` | (none) | Optional description. |
| `--visibility` | `private` | `private` (members-only) or `public` (anyone with a key can read). |

## `rooms list`

```
conclave-mcp rooms list [--visibility public]
```

Lists the rooms the caller is a member of. Output is a
table with columns: `ID`, `NAME`, `VISIBILITY`, `OWNER`,
`CREATED`.

| Flag | Default | Notes |
|---|---|---|
| `--visibility` | (all) | `private` or `public`. |

## `rooms get`

```
conclave-mcp rooms get 01ABC...
```

Shows the details of a single room, including the member
list with each member's role. The caller must be a member.

## `rooms archive`

```
conclave-mcp rooms archive 01ABC...
```

Soft-archives a room. The room is hidden from `rooms list`
for non-owner members, but the messages remain. Owner only.

## `rooms invite`

```
conclave-mcp rooms invite 01ABC... 4 --role=writer
```

Invites an API key (the id from `keys list`) to a room. Owner
only.

| Flag | Default | Notes |
|---|---|---|
| `--role` | `writer` | `owner`, `writer`, or `reader`. |

The MCP equivalent is the `rooms.invite` tool. The HTTP
equivalent is `POST /v1/rooms/{id}/members` with body
`{key_id, role}`.

## `rooms remove`

```
conclave-mcp rooms remove 01ABC... --key-id 4
```

Removes an API key from a room. The key is no longer a
member; the key itself is not deleted. Owner only.

| Flag | Default | Notes |
|---|---|---|
| `--key-id` | (required) | The API key id. |

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

Output is a JSON object with the new message, including
`id`, `ts`, `size` (the byte length of the content), and the
denormalised `trust_origin`.

## `messages list`

```
conclave-mcp messages list [ROOM] [--trust-origin internal]
```

Lists messages in a room, ordered by `ts` ascending. The
caller must be a member. Output is a table by default; pass
`--format=json` for a JSON array.

| Flag | Default | Notes |
|---|---|---|
| `--since` | (none) | RFC 3339 timestamp, any offset; only messages newer than this are returned. Use the `next_since` field from the previous call for pagination. |
| `--limit` | `100` | Max messages to return. |
| `--trust-origin` | (all) | `internal` or `external`. |

## `messages delete`

```
conclave-mcp messages delete <ROOM> <MSG-ID>
```

Soft-deletes a message (`MSG-ID` from `messages list`). The
message is hidden from `messages list` but the row remains in
the DB. Owner only.

## `init`

```
conclave-mcp init [--room NAME]
```

Run inside a git repository with a key that may own the room
(normally the admin key). Finds or creates the room named after
the origin remote (`acme/widgets`), then creates `.conclave.json`
and merges the `conclave` MCP server into `.mcp.json`, the hook
into `.claude/settings.json` and the coordination rules into
`CLAUDE.md`. Existing content is kept; safe to re-run. See
[agent-integration.md](agent-integration.md#coding-agents-on-one-project-claude-code).

## `agent add`

```
eval "$(conclave-mcp agent add agent-c [--store] [--room NAME] [--role writer] [--trust-origin internal])"
```

Admin key, room owner. Creates a key labelled `agent-c`,
invites it to the repository's room and prints
`export CONCLAVE_API_KEY=...` on stdout (once; nothing is
stored). If the invite fails, the new key is revoked again.

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
failures exit 1, never 2. `CONCLAVE_RELEASE_ON_STOP=1` also
releases the claims when the turn ends (for `claude -p` runs,
which never fire SessionEnd).

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

Lists the audit log. Admin-scope required (in v0.1, all
keys have admin scope; this will change in v0.2).

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
conclave-mcp rooms invite "$INBOX_ID" 2 --role=writer
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
