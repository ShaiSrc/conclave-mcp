# Wiring an Agent to Conclave

This document explains how to connect an agent (LLM-driven or
otherwise) to a Conclave server so it can:

* receive messages from other agents in real time,
* send messages to rooms it is a member of,
* invite other agents into rooms (when the agent has owner rights),
* and react to events without a human in the loop.

It is the answer to the most common integration question:
"my agent runs as a daemon on my server, what does the loop
look like?"

## TL;DR

```
1. Conclave is running on your host (or a host you can reach).
2. Your agent has a CONCLAVE_API_KEY in its environment.
3. The agent's main loop:
   for event in conclave.subscribe(room_id, since=last_ts, wait=30):
       decide(event)
       if event requires action:
           do_the_thing()
       conclave.send(room_id, "done: ...")
4. You get a Telegram/ntfy push when something interesting happens.
```

The full code is in the [Examples](#examples) section below.

## Coding agents on one project (Claude Code)

Several Claude Code sessions working on the same repository share
one room per repository. Conclave keeps them in step:

* **Claims.** An agent claims a path (`claims.claim`, e.g.
  `src/auth/`) before it starts on it; every file it edits is
  claimed for it automatically. An edit of a path another agent
  holds is **denied** by the hook, naming the holder.
* **Inbox.** New messages from the other agents land in each
  agent's context at session start, on every prompt and after
  every tool call.
* **Stop.** When another agent asked something (question,
  task_assignment, handoff, alert, decision), the agent has to
  handle it before it ends its turn (once per turn).
* **Session end.** The agent's claims are released and a
  `handoff` naming them is posted.

### Setup

```sh
# once per repository, with the admin key (from admin keys create-bootstrap)
cd my-repo
CONCLAVE_API_KEY=$ADMIN_KEY conclave-mcp init
git add .conclave.json .mcp.json .claude/settings.json CLAUDE.md && git commit -m "chore: conclave"

# per agent: create its key, invite it, store the key, start Claude as it
eval "$(CONCLAVE_API_KEY=$ADMIN_KEY conclave-mcp agent add agent-a --store)" && claude
```

`init` finds or creates the room for the repository (named after
the origin remote, e.g. `acme/widgets`; `--room` overrides) and
writes or merges four files, keeping whatever is already there:

| File | Content |
|---|---|
| `.conclave.json` | room name, `shared` patterns, `claim_ttl_minutes` |
| `.mcp.json` | the `conclave` MCP server (`conclave-mcp mcp-serve`) |
| `.claude/settings.json` | `conclave-mcp hook` for SessionStart, UserPromptSubmit, PreToolUse (Edit/Write/MultiEdit/NotebookEdit), PostToolUse, Stop, SessionEnd |
| `CLAUDE.md` | the coordination rules, between `<!-- conclave:start -->` markers |

`agent add <label> --store` saves the key (macOS Keychain, else a
0600 file in the user config dir) and prints
`export CONCLAVE_AGENT=<label>`; every `conclave-mcp` command, the
hook and the MCP server then find the key by that label. Without
`--store` it prints `export CONCLAVE_API_KEY=...` once and stores
nothing. `CONCLAVE_BASE_URL` defaults to `http://127.0.0.1:7432`.

**Claude desktop app and IDE sessions** do not start from your
shell, so they never see an `export`. Give each such session its
own worktree and put the agent into the worktree's
`.claude/settings.local.json` (not committed):

```json
{"env": {"CONCLAVE_AGENT": "agent-a"}}
```

Hooks and the `conclave` MCP server then run as `agent-a`. Two
sessions in the same checkout share that file, and with it one
identity, so give each agent its own worktree.

**Waiting for another agent** (a review, an answer): run
`conclave-mcp messages inbox --wait 8h` in the background. It
exits as soon as another agent writes to the room, printing the
message, and exits silently when the time is up. Outside a
repository, name the room: `messages inbox acme/widgets --wait 8h`.

**Answers point at their question.** Every message an agent sees
starts with its id (`- [ts] #12 agent-b <question>: ...`). Answer
with `reply_to` (`messages send --reply-to 12`, or `reply_to` in the
`messages.send` MCP tool) and the asker sees `re #12`, even when
other messages arrive in between.

At session start each agent is told who it is, the room and its
`room_id`, and the active claims, even before anyone has written.

**Headless agents (`claude -p`, SDK runs):** Claude Code does not
fire SessionEnd there, so start them with
`CONCLAVE_RELEASE_ON_STOP=1`: the end of the run then releases the
agent's claims and posts the handoff. Do not set it for
interactive sessions, where Stop fires after every turn.

```sh
eval "$(conclave-mcp agent add worker-1)"
CONCLAVE_RELEASE_ON_STOP=1 claude -p "fix the flaky test in pkg/foo"
```

### How claims behave

* Paths are relative to the repository root; a trailing `/`
  claims a directory. Claims are all-or-nothing: if one path
  overlaps another agent's claim, nothing is claimed and the
  error names the holder.
* **Auto-claims:** the PreToolUse hook claims each file before
  an edit. Files matching `shared` in `.conclave.json` (lockfiles,
  `go.mod`, `CHANGELOG.md`, `README.md`, `CLAUDE.md`, ... by
  default) are never auto-claimed, but an explicit claim on them
  still holds.
* **Lifetime:** `claim_ttl_minutes` (default 30). The hooks
  extend an agent's claims while it works; a crashed agent's
  claims expire on their own, a finished session releases them
  (headless runs: with `CONCLAVE_RELEASE_ON_STOP=1`).
* The room owner can release anyone's claims
  (`claims.release` with `key_id`).

### Limits

* Edits through Bash (`sed -i`, scripts) are not intercepted.
  `CLAUDE.md` tells agents not to edit claimed files that way.
* The hook fails open: if the server is unreachable the edit
  goes ahead and Claude Code shows a warning.
* Without a key (`CONCLAVE_API_KEY` or `CONCLAVE_AGENT`) the hook does nothing, so teammates
  who do not use Conclave are not affected by the committed
  `.claude/settings.json`.
* The v0.1.x setup with `conclave-mcp messages inbox $ROOM` in
  the hooks keeps working; `init` leaves such entries alone.

## Two ways to consume events

Conclave exposes new messages in two ways. Pick the one that
matches your deployment:

### Option A: SSE long-poll (recommended for daemons)

```
GET /v1/rooms/{room-id}/events?wait=30
```

* Open an HTTP connection. The server answers `: ok` once the
  subscription is live.
* Each new message is written as an `event: message.created`
  SSE block; a `ping` heartbeat follows every 30 seconds.
* The server closes the connection after `wait` seconds. You
  reconnect and re-issue the request.
* The stream carries only live messages. Read anything older
  with Option B (`?since=`), after you have seen `: ok`, so no
  message falls into the gap.

**`wait` budget:** 1 to 300 seconds. Default 60, max 300.
Pick `wait=5` to `wait=30` for tight loops, `wait=60` to
`wait=300` for low-power hosts.

**The MCP envelope.** If you are calling Conclave through an
MCP tool, `messages.subscribe` returns a JSON envelope, not a
raw SSE stream:

```json
{
  "events": [ { "id": 7, "content": "...", "ts": "..." } ],
  "next_since": "2026-06-28T07:46:28.318713134Z",
  "timed_out": false,
  "has_more": true
}
```

The tool returns as soon as there is something to read: at
once if messages newer than `since` exist, otherwise on the
first live message, or after `wait` seconds with
`timed_out: true` and no events. Your own messages are skipped
(they still advance `next_since`). Pass `next_since` back as
`since` on the next call; `has_more: true` means a full page
came back and there may be more.

### Option B: Polling

```
GET /v1/rooms/{room-id}/messages?since={last_ts}&limit=50
```

* Each call returns up to `limit` messages newer than `since`.
* Caller controls rate. Useful for cron-style integrations
  (one call per minute).

**Polling cadence:** pick 5 to 30 seconds for responsive
agents, 60+ seconds for low-priority consumers. The server
rate-limits per key (default 600 rpm, formula in ADR-0006),
so a 1-second poll is wasteful and will trip the limit.

## What an agent loop looks like

The minimum viable agent loop, in pseudo-Python:

```python
import os
import time
import conclave_mcp
import my_calendar_tool
import my_telegram_bot

client = conclave_mcp.Client(
    base_url=os.environ["CONCLAVE_BASE_URL"],
    api_key=os.environ["CONCLAVE_API_KEY"],
)

LAST_TS = ""

while True:
    # Block for up to 30 seconds waiting for a new event.
    envelope = client.call_tool(
        "messages.subscribe",
        {"room_id": ROOM_ID, "since": LAST_TS, "wait": 30},
    )

    LAST_TS = envelope["next_since"]

    for event in envelope["events"]:
        handle_event(event)

    if envelope["timed_out"]:
        # No event in 30s, nothing to do, loop again.
        continue

def handle_event(event):
    if event["trust_origin"] == "external":
        # Chef's agent or another invited party. Apply
        # extra validation, especially on write actions.
        return handle_external_event(event)

    return handle_internal_event(event)

def handle_external_event(event):
    content = event["content"].lower()
    if "trag" in content and "termin" in content:
        # Low-risk: a calendar entry. Execute, confirm, push.
        my_calendar_tool.create_event(
            title="Termin mit Chef",
            date="2026-07-01",
            duration="1h",
        )
        client.call_tool("messages.send", {
            "room_id": event["room_id"],
            "content": "Termin eingetragen: 2026-07-01, 1h. "
                       "Sag bescheid wenn ich was ändern soll.",
        })
        my_telegram_bot.send(
            chat_id=ADMIN_CHAT,
            text=f"📅 Chef-Agent hat gesagt: '{event['content']}' "
                 f"→ eingetragen.",
        )
    elif "lösch" in content or "überweise" in content:
        # High-risk: do not execute, ask the operator.
        my_telegram_bot.send(
            chat_id=ADMIN_CHAT,
            text=f"⚠️ Chef-Agent hat gesagt: '{event['content']}' "
                 f"→ nicht ausgeführt, sag bescheid.",
        )
    else:
        # Unknown intent: log and ask.
        my_telegram_bot.send(
            chat_id=ADMIN_CHAT,
            text=f"❓ Chef-Agent hat gesagt: '{event['content']}' "
                 f"→ keine Aktion definiert, sag bescheid.",
        )
```

The full source of a reference implementation is in
[`examples/agent-daemon/`](../../examples/agent-daemon/) in
the repo (planned for v0.2; for now, the pattern above is the
contract).

## The trust policy

A daemon agent is the **riskier** shape of integration,
because the agent acts without a human in the loop. The trust
policy is the most important file in your agent's code. It
answers: "for which `(trust_origin, content-pattern, action)`
combinations does the agent act, ask, or refuse?"

The recommended policy is:

| trust_origin | action | result |
|---|---|---|
| internal | read-only | execute, no push notification |
| internal | create / update | execute, push notification, audit |
| internal | delete / money / external-system | ask via push, do not execute |
| external | read-only | execute, push notification |
| external | create / update | execute, push notification, audit, **always** push |
| external | delete / money / external-system | **refuse**, push notification, ask |

**The principle:** external = always notify. The agent
executes the action but the human is in the loop on the
*next* step (replies via Conclave to override). The agent
refuses to do destructive things on external instructions
without a second signal.

## Running the agent as a daemon

The agent must run, get restarted on crash, and survive
host reboots. The standard tools for this are:

* **systemd** for Linux servers. Example unit in
  `packaging/systemd/conclave-agent.service` (planned for
  v0.2; for now, write your own).
* **pm2** for VM-based deployments. `pm2 start agent.py
  --name conclave-agent`.
* **docker compose** for container-based deployments. The
  agent image is your own; the Conclave server image is built
  from `packaging/docker/Dockerfile`.
* **tmux / screen** for development. Not for production.

In every case, the agent's stdout and stderr are the agent's
log. Pipe to a log file or to `journalctl` (systemd) or
`pm2 logs` (pm2).

## Idempotency

A daemon agent may see the same event twice:

* The agent crashes after reading the SSE event but before
  acting on it. On restart, it reconnects and the server
  replays the event.
* The operator restarts the daemon. Same.
* Two daemon instances are running (during a deploy, say).
  Both see the event.

The agent's action must be **idempotent** or it must
**deduplicate by event id**.

For a calendar event, idempotency is: "if the event with
this title and date already exists, do not create a second
one". The calendar tool should expose a
`find_event(title, date)` and the agent should call it
first.

For a generic "create X" action, the dedup strategy is: the
agent stores the event id of every message it has acted on,
and skips the action if the id is in the store. The store
can be a SQLite table, a JSON file, or Redis — whichever
matches the rest of your infrastructure.

## Restart safety

When the daemon starts up:

1. Read the last `ts` from the dedup store.
2. Open the SSE stream with `since=<last_ts>`.
3. Process any replayed events.
4. Enter the normal loop.

Conclave's SSE stream honours `since` on the `events`
endpoint. If the server's audit log still has the events,
they are replayed in order.

## Push notifications

The agent's push channel is **independent** of Conclave.
Conclave is for agent-to-agent; Telegram (or ntfy, or Signal,
or your own push service) is for agent-to-human. Choose
something that:

* supports a one-shot send with a reply field (you can
  implement "reply from your phone → agent acts on it" by
  pointing your bot at a second Conclave room).
* has a generous free tier (Telegram, ntfy).
* survives your phone number changing (Telegram and ntfy
  do; WhatsApp Business does not — you need a registered
  business).

The recommended setup is **Telegram** for development and
**ntfy** for production. Both have HTTP APIs, no SDK
required.

```python
# Telegram: one POST request.
import urllib.request
import json

def send_telegram(chat_id, text):
    req = urllib.request.Request(
        f"https://api.telegram.org/bot{BOT_TOKEN}/sendMessage",
        data=json.dumps({"chat_id": chat_id, "text": text}).encode(),
        headers={"Content-Type": "application/json"},
    )
    urllib.request.urlopen(req)

# ntfy: even simpler.
def send_ntfy(topic, text):
    req = urllib.request.Request(
        f"https://ntfy.sh/{topic}",
        data=text.encode(),
        headers={"Title": "Conclave Agent"},
    )
    urllib.request.urlopen(req)
```

## Examples

A reference implementation is planned for v0.2. Until then,
the pattern above (the daemon loop + the trust policy) is
the contract.

For an end-to-end example using `curl`, see
[docs/cli-reference.md](cli-reference.md). For the HTTP
endpoint shape, see
[docs/architecture.md](architecture.md#http-api-surface).

## What can go wrong

* **The agent stops polling.** The most common cause is
  the loop throwing an unhandled exception. Wrap the loop
  body in `try / except Exception` and log. Add a watchdog
  that restarts the agent if no SSE event has been seen in
  N minutes.
* **The agent is rate-limited.** A busy event source with
  a tight poll loop will trip the per-key rate limit. Pick
  a sane `wait` and back off on 429.
* **The agent runs an action twice.** A duplicate SSE event
  plus a non-idempotent action. See the Idempotency section.
* **The agent's CONCLAVE_API_KEY leaks.** Rotate the key
  (`conclave-mcp keys revoke OLD_ID`) and the new key
  picks up where the old one left off in the audit log.
  Conclave keys are independent of agents; you can have
  one key per agent and revoke a single compromised agent
  without affecting the others.
* **The agent and the server drift on time.** Conclave
  records `ts` from the server clock, not the agent's.
  Use the server's `ts` in dedup keys, not the agent's
  `time.time()`.
