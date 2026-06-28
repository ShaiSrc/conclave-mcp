<!--
MIRRORED FROM THE OBSIDIAN VAULT
================================

This file is a mirror of the ADR with the same name in
the maintainer's Obsidian vault:

  /workspace/notes/Conclave/ADRs/ADR-NNNN-...md

The vault is the source of truth. The mirror may lag.
If this file disagrees with the vault, the vault wins.

To re-sync: copy the vault file over this one. No
frontmatter changes are required.

-->

---
adr_id: ADR-0009
title: SSE transport partially overrides SPEC-0001 §3 non-goal
status: Accepted
date: 2026-06-27
deciders: Project Owner
tags: [scope, transport, realtime, override]
project: Conclave
related: [ADR-0001, ADR-0007, ADR-0008, SPEC-0001, SPEC-0003]
supersedes: [SPEC-0001 §3 "WebSocket-based realtime push"]
---

# ADR-0009 — SSE transport partially overrides SPEC-0001 §3 non-goal

## Status

Accepted — 2026-06-27.

## Context

SPEC-0001 §3 lists "WebSocket-based realtime push" as a
v0.x non-goal. The original reasoning, recorded in the
SPEC-0001 §3 commit message and the ADR-0001 narrative,
was:

- Realtime push requires long-lived connections, which
  are a different operational shape than
  request/response.
- The Conclave runtime is single-node + request/response;
  adding a long-lived connection per subscriber is
  operator-overhead for a feature that polling can
  cover.
- The Polling pattern (`GET /v1/rooms/{id}/messages?since=...`)
  is well-understood and works with every HTTP client
  and every reverse proxy.

The InterAgent v1.0 spec, however, lists `subscribe_room`
as one of the 11 MCP tools. ADR-0007 §6 originally
marked `subscribe_room` as "Deferred" and the
`transport: sse` item as "Out of scope" (ADR-0007 §2).

In the 2026-06-27 evening session the project owner
decided that **subscribe_room is a real user-visible
gap** and that the SSE transport is the cleanest way
to close it. The decision is:

- Adopt SSE as an **additive** transport at
  `GET /v1/rooms/{id}/events`.
- Polling remains the canonical fallback for clients
  that do not want long-lived connections.
- SPEC-0001 §3 is **partially overridden**: the
  WebSocket-specific reasoning is dropped; the broader
  "v0.x is request/response" reasoning is kept for the
  other transports.

## Decision

### 2.1. The transport

Server-Sent Events (SSE) is the new transport. The
endpoint is:

```
GET /v1/rooms/{id}/events?since=<iso8601>&trust_origin=<internal|external>&wait=<seconds>
```

- The client must authenticate with a bearer token in
  the `Authorization` header (or `X-Conclave-Token`).
- The client must be a member of the room.
- The server emits one `event: message.created` per new
  message in the room, with the same JSON shape as
  `GET /v1/rooms/{id}/messages` returns for one row.
- The server emits a heartbeat `event: ping` every
  30 seconds to keep the connection alive.
- The server closes the connection after `wait`
  seconds (default 60, max 300) so a client that
  goes idle does not hold a connection forever.
- The client can reconnect with the same `since`
  parameter to resume.

### 2.2. Why SSE, not streamable_http (MCP-2025)

The MCP-2025 spec defines a `streamable_http` envelope
that wraps multiple JSON-RPC requests in a single
streaming HTTP response. Conclave does not implement
this envelope for two reasons:

1. The envelope is a different protocol shape than the
   existing JSON-RPC-over-HTTP endpoint at `POST /v1/mcp`
   (commit 81f5095). Adding both would mean two
   parallel transports for the same JSON-RPC surface,
   which doubles the test surface and the support
   burden.
2. The two endpoints — JSON-RPC-over-HTTP for tool
   calls, SSE for room subscription — are a cleaner
   mapping: the request/response shape goes to HTTP,
   the streaming shape goes to SSE. The MCP `tools/call`
   method is already covered by HTTP; the `subscribe`
   intent is the only one that wants a stream.

ADR-0007 §6 records `transport: streamable_http` as
**Superseded** for the Conclave v0.x line.

### 2.3. Why SSE, not long-polling

Long-polling (`GET /v1/rooms/{id}/messages?wait=true&since=...`
that blocks 30s) was the alternative considered. SSE wins
for two reasons:

- The reverse-proxy configurations in `packaging/reverse-proxy/`
  (nginx, Caddy) already have documented `proxy_buffering
  off` and SSE-related settings. A long-poll endpoint
  would need its own proxy configuration.
- The client-side ergonomics are simpler with SSE: a
  well-behaved EventSource implementation handles
  reconnection, last-event-id tracking, and the
  heartbeat for free. Long-polling requires the client
  to implement all of that.

The trade-off is that SSE needs the
`proxy_buffering off` setting in nginx and the
`flush_interval -1` setting in Caddy. The
`docs/runbook.md` §4 ("Reverse proxies") already
documents this in the context of long-poll and SSE
compatibility, so no new configuration is needed.

### 2.4. What is NOT changed

- WebSocket is still not implemented. The InterAgent
  spec did not require it; SPEC-0001 §3's
  WebSocket-specific reasoning remains in force.
- The MCP-over-stdio transport is unchanged.
- The `POST /v1/mcp` JSON-RPC-over-HTTP transport
  (commit 81f5095) is unchanged.
- The `messages.list?since=...` polling pattern is
  unchanged. **Polling is the canonical fallback for
  clients that cannot or will not use SSE.**

## Consequences

Positive
- The InterAgent `subscribe_room` MCP tool is now
  implemented as `messages.subscribe`, closing one of
  the user-visible InterAgent gaps.
- The receiving client (operator's Hermes subagent) can
  react to new messages in a room within ~1s of arrival,
  without polling. This is the right ergonomics for an
  interactive agent.
- The SSE endpoint also exposes `trust_origin`
  (ADR-0008 revision), so an external-only EventSource
  filter (`?trust_origin=external`) is one query param
  away.
- Polling is still the canonical fallback; clients that
  already poll keep working.

Negative / Risks
- Each SSE connection holds a goroutine on the server
  for the duration of the connection. For v0.x scale
  (single trust boundary, a few operators, dozens of
  subscribers) this is fine. A v0.3+ scale-up would
  need a pub/sub backplane (Redis, NATS, in-process
  ring buffer).
- The reverse-proxy buffering setting
  (`proxy_buffering off` in nginx) is a known
  operational footgun. The runbook already warns about
  it in §4; no new documentation is needed, but a
  misconfigured proxy silently breaks the SSE endpoint
  by buffering forever.
- An idle SSE client that does not disconnect on
  `wait` seconds expiry will be force-closed by the
  server. This is the right behaviour, but a client
  that does not handle the close gracefully will see
  the connection drop and must reconnect.

## Alternatives considered

- **Long-polling** — discussed in §2.3.
- **WebSocket** — too heavy; the InterAgent spec did
  not require it; the operator did not ask for it.
- **A separate `subscribe` MCP tool that wraps a
  client-side loop** — would not be a real push, just
  a polling loop with the MCP framing. Not adopted.
- **No SSE; just better polling** — the operator
  asked for `subscribe_room` to be real, and SSE is
  the standard way to deliver it without changing the
  rest of the architecture.

## Follow-up

- ADR-0007 (companion) — `subscribe_room` disposition
  changes from Deferred to Adopted (via this ADR).
- ADR-0008 (companion) — the `trust_origin` payload
  field on SSE events comes from the same revision.
- SPEC-0001 §3 — add a note that "SSE-based room
  subscription is not in scope of this non-goal; see
  ADR-0009 for the exception." Without removing the
  broader "v0.x is request/response" wording.
- SPEC-0003 §2.2 — add the `GET /v1/rooms/{id}/events`
  endpoint documentation.
- SPEC-0004 §5.2 — add the `messages subscribe` CLI
  subcommand.
- Code patches — `internal/server/events.go` (new
  package), `messages.subscribe` MCP tool,
  `cmd/messages` subscribe subcommand.
- Tests — `tests/e2e/surface_test.go` gains
  `TestE2E_Surface_SSE_Subscribe` and
  `TestE2E_Surface_SSE_Heartbeat`.
- Docs — `docs/runbook.md` §4 cross-references
  ADR-0009 for the SSE-specific proxy settings.
