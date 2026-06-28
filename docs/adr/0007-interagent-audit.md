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
adr_id: ADR-0007
title: InterAgent v1.0 → Conclave v0.x feature audit
status: Accepted
date: 2026-06-27
deciders: Project Owner
tags: [scope, audit, interop, supersedes-ADR-0008, paired-with-ADR-0009]
project: Conclave
related: [ADR-0001, ADR-0002, ADR-0003, ADR-0004, ADR-0005, ADR-0006, ADR-0008, ADR-0009]
supersedes: [ADR-0008 §1]
---

# ADR-0007 — InterAgent v1.0 → Conclave v0.x feature audit

## Status

Accepted — 2026-06-27 (initial).

**Revised 2026-06-27 evening** to reflect three new
decisions taken in the same session:

1. **SSE transport adopted** (via ADR-0009), which
   partially overrides the SPEC-0001 §3 "WebSocket-based
   realtime push is a non-goal" wording. Polling remains
   the canonical fallback.
2. **`rooms.invite` and `rooms.remove` exposed as MCP
   tools** — they were HTTP-only in v0.1, now wrapped
   for the MCP channel. The `messages.subscribe` MCP
   tool wraps the new SSE endpoint.
3. **The four `*_agent` MCP tools are superseded** by
   the key-is-the-agent model (ADR-0001 §A3). They are
   not implemented. The intended use is `keys.create`,
   `keys.revoke`, `keys.list` (which now accept the
   optional `trust_origin` parameter, per the ADR-0008
   revision).

This revision is additive: items already marked "Adopted"
remain adopted. Items marked "Out of scope" in the
original may now be "Superseded" or "Adopted" — the
diff is captured in the per-section tables below.

## Context

The project started as **InterAgent MCP v1.0.0 "Foundation"**, with
a YAML spec file (Codename "Foundation") that listed:

- a `core_entities` model with `agent`, `room`, `membership`, `message`;
- a `room.visibility` field;
- eight `message_types` (question, answer, decision, handoff,
  status_update, task_assignment, alert, system);
- 11 MCP tools including `delete_room`, `subscribe_room`, and four
  `*_agent` tools (create_agent, delete_agent, list_agents, invite_agent,
  remove_agent);
- five event types (room_created, room_deleted, message_created,
  agent_joined, agent_left);
- audit retention `forever`;
- API-key hashing via `bcrypt`;
- a security flag `external_content_flagging: true`;
- three MCP transports: `stdio`, `streamable_http`, `sse`;
- a `success_metrics` block: agents can exchange messages,
  room-based access control works, audit log available, no framework
  dependencies.

After the rename to **Conclave** (ADR-0001) and the Conclave-specific
Specs (SPEC-0001/0002/0003/0004/0005) and ADRs (0002–0006), a
number of InterAgent items were either:

- **adopted** (room.visibility, message_types, delete_messages);
- **reformulated** (agent → key, bcrypt → argon2id, forever → 730d);
- **deferred** (binary_release CI workflow);
- **superseded** (`*_agent` MCP tools, streamable_http envelope);
- **out of scope by design** (federation, E2E encryption, voice,
  multimedia, public social, observer/moderator roles).

The companion `docs/INTERAGENT-AUDIT.md` carries the same
table in a shorter, operator-facing form; this ADR is the
authoritative source.

## Decision

For each InterAgent v1.0 item, one of five dispositions applies:

| Disposition | Meaning |
|---|---|
| **Adopted** | Implemented in Conclave as specified (or with a documented, minor reformulation). |
| **Reformulated** | Implemented in a different form, with the new form spec'd in the matching Conclave spec/ADR. The InterAgent wording is no longer the source of truth. |
| **Deferred** | Intentionally not implemented in v0.x; tracked for a later iteration. |
| **Superseded** | The InterAgent item is no longer relevant because a higher-level decision (named in the row) collapses it. The InterAgent wording is **not** the source of truth; the higher-level decision is. |
| **Out of scope** | Not part of Conclave by design. ADR-0001 + SPEC-0001 §3 are the source. |

### 1. Vision, goals, non-goals

| Item | Disposition | Where |
|---|---|---|
| framework neutral | Adopted | Go binary, no AI-framework coupling |
| MCP native | Adopted | `internal/mcp/mcp.go` (JSON-RPC 2.0) |
| human inspectable | Adopted | SQLite, direct `sqlite3` access (SPEC-0002 §2.1) |
| auditable communication | Adopted | `internal/audit/audit.go` + 8 mutation hooks (now incl. `trust_origin`) |
| self-hosted | Adopted | single binary, container (SPEC-0005) |
| simple deployment | Adopted | runbook (10 steps) |
| production ready | Reformulated | v0.1 label; "production" implied by the deployment + runbook + ADR-0004 retention |
| federation (non-goal) | Out of scope | SPEC-0001 §3 |
| E2E encryption (non-goal) | Out of scope | not in SPEC-0001 §3 but consistent with single-trust-boundary design |
| voice (non-goal) | Out of scope | SPEC-0001 §3 |
| multimedia (non-goal) | Out of scope | SPEC-0001 §3 |
| public social (non-goal) | Out of scope | no federation, no public discovery |

### 2. Architecture

| Item | Disposition | Where |
|---|---|---|
| transport: **stdio** | Adopted | `internal/mcp/mcp.go` |
| transport: **JSON-RPC over HTTP** | Adopted (commit 81f5095) | `POST /v1/mcp` |
| transport: **SSE** | Adopted (via ADR-0009) | `GET /v1/rooms/{id}/events` streams new messages as Server-Sent Events. SPEC-0001 §3 is **partially overridden**; polling remains the canonical fallback. |
| transport: **streamable_http** (MCP-2025) | Superseded | The MCP-2025 `streamable_http` envelope (which wraps multiple requests in a single streaming response) is not implemented. The two endpoints above — JSON-RPC-over-HTTP for tool calls, SSE for room subscription — are the Conclave equivalent. |
| storage: sqlite default | Adopted | `migrations/0001_initial.sql` |
| auth: api_key | Adopted | `internal/auth.NewVerifier` + argon2id (ADR-0003) |

### 3. Core entities

| Item | Disposition | Where |
|---|---|---|
| **agent** (id, name, description, api_key_hash, created_at, active) | Reformulated | SPEC-0001 §5 A3: "An 'agent' in v0.x is any process that can speak MCP and present a valid API key. Conclave does not need to know whether the agent is a Hermes subagent, a Claude Code child, or a custom script." → no `agents` table, the API key **is** the agent. `name`, `description`, `active` are absorbed by `api_keys.label` (label) and `api_keys.revoked_at IS NULL` (active). The optional `trust_origin` field (ADR-0008 revision) is the Conclave substitute for an "is this agent external" hint. |
| **room** (id, name, description, **visibility**, created_at) | Reformulated | `rooms` table + `visibility` column (SPEC-0001 Q6, SPEC-0002 §3.3, SPEC-0003 §3.3). `visibility` defaults to `private`. |
| **membership** (room_id, agent_id, role) | Reformulated | `room_members(room_id, key_id, role, joined_at)`. `agent_id` → `key_id`; same semantics. |
| **message** (id, room_id, sender_agent_id, **message_type**, content, **metadata**, created_at, **trust_origin**) | Reformulated | `messages` table extended with `message_type` (SPEC-0001 Q7), `metadata` (JSON blob, SPEC-0001 Q8), `deleted_at` (soft-delete), and `trust_origin` (ADR-0008 revision, denormalised from `author_key.trust_origin` at send time). `sender_agent_id` → `author_key_id`. `created_at` → `ts` (ISO-8601). |

### 4. Message types

The eight InterAgent types (`question, answer, decision, handoff,
status_update, task_assignment, alert, system`) are modelled as a
**SQL column with CHECK constraint** in the `messages` table
(Migration 0003), not as a tagged union in the content JSON as
initially proposed in this ADR. The reasoning shifted during
implementation: a column with a CHECK constraint lets the server
validate, lets the audit log reference the type by `target_id`
joins, and lets the MCP `messages.send` tool return a precise
400 on unknown values. The `content_type` (MIME) column remains
as the orthogonal "format" axis. SPEC-0001 Q7 records the
re-reformulation.

### 5. Permissions / room roles

| InterAgent role | Conclave equivalent | Notes |
|---|---|---|
| `owner` | `owner` | identical |
| `moderator` | `writer` (closest) | Conclave SPEC-0003 §3.3 lists `owner, writer, reader` only; "moderator" is folded into `writer` (semantically: can write + invite). |
| `member` | `writer` | identical |
| `observer` | `reader` | identical |

The `owner.delete_messages` capability is **adopted** via
`DELETE /v1/rooms/{id}/messages/{msg_id}` (owner only, soft-delete,
SPEC-0001 Q8).

### 6. MCP tools (revised 2026-06-27 evening)

The InterAgent spec lists 11 tools. Conclave now exposes **12**:

| InterAgent tool | Conclave disposition | Mapping |
|---|---|---|
| `create_room` | Adopted | `rooms.create` |
| `delete_room` | Reformulated | `rooms.delete` (wraps `POST /v1/rooms/{id}/archive`, soft-delete) |
| `list_rooms` | Adopted | `rooms.list` |
| `send_message` | Adopted | `messages.send` (exposes `trust_origin` per ADR-0008 revision) |
| `get_messages` | Adopted | `messages.list` (exposes `trust_origin`, supports `?trust_origin=external\|internal` filter) |
| `subscribe_room` | **Adopted (via ADR-0009)** | `messages.subscribe` MCP tool → `GET /v1/rooms/{id}/events` SSE stream. Long-lived connection; clients disconnect on idle. |
| `create_agent` | **Superseded** | The InterAgent `agent` entity is the Conclave `api_keys` row. A separate `agents.create` MCP tool would be semantically misleading ("create an agent" really means "create a key"). The intended use is `keys.create` with the optional `trust_origin` parameter (ADR-0008 revision). |
| `delete_agent` | **Superseded** | same as above; use `keys.revoke` |
| `list_agents` | **Superseded** | same as above; use `keys.list` (which now also returns `trust_origin`) |
| `invite_agent` | **Adopted (was HTTP-only in v0.1)** | `rooms.invite` MCP tool → `POST /v1/rooms/{id}/invite` |
| `remove_agent` | **Adopted (was HTTP-only in v0.1)** | `rooms.remove` MCP tool → `DELETE /v1/rooms/{id}/members/{key_id}` |

The decision to **not** expose `agents.*` MCP tools is
recorded in ADR-0001 §A3 (the API-key-is-the-agent
reformulation) and in `docs/INTERAGENT-AUDIT.md` §6. The
InterAgent spec's mental model — that an "agent" is a
named, addressable identity that can be invited/removed
from rooms — collapses onto the Conclave key model. The
"named" part is `api_keys.label`; the "addressable" part
is the bearer token; the "invited/removed" part is
`room_members` with the existing `room.invite` and
`room.remove` HTTP endpoints. No additional storage is
needed.

### 7. Events (revised 2026-06-27 evening)

InterAgent lists five events: `room_created, room_deleted,
message_created, agent_joined, agent_left`. Conclave now
**pushes** the central one (message_created) and exposes
the rest as audit entries:

| InterAgent event | Conclave channel | Source |
|---|---|---|
| `message_created` | **push (SSE)** | `GET /v1/rooms/{id}/events` (ADR-0009); event payload includes `id`, `ts`, `author_key_id`, `message_type`, `trust_origin`, `content` |
| `room_created` | audit | `room.create` |
| `room_deleted` | audit | `room.archive` |
| `agent_joined` | audit | `room.invite` |
| `agent_left` | audit | `room.remove` |

The `?room_id=<id>` filter (SPEC-0001 Q10) makes per-room
audit polling cheap (denormalised `audit.room_id` column +
`idx_audit_room_ts` index).

### 8. Audit

| Item | Disposition | Where |
|---|---|---|
| `enabled: true` | Adopted | `internal/audit/audit.go` + 8 mutation hooks |
| `retain.messages: forever` | Reformulated | ADR-0004: 730-day default, 30-day floor, env-overridable |
| `searchable: sender` | Adopted | `?actor=<key_id>` |
| `searchable: room` | Adopted (this ADR) | `?room_id=<id>` (SPEC-0001 Q10, SPEC-0002 §3.6) |
| `searchable: message_type` | Adopted | filter by `target_id` and read the `messages` row's `message_type`, or by `?action=message.send` and a sub-query on the message row |
| `searchable: timestamp` | Adopted | `?since=<ts>` |
| `searchable: trust_origin` | Adopted (this revision) | `?detail=trust_origin:external` via the JSON `detail` column; new `?actor_trust_origin=` filter is a Phase-52 follow-up |

### 9. Security (revised 2026-06-27 evening)

| Item | Disposition | Where |
|---|---|---|
| `rate_limiting: true` | Adopted | `internal/server/ratelimit.go` (600 rpm default) |
| `api_key_hashing: bcrypt` | Reformulated | **argon2id** (ADR-0003; OWASP 2024 parameters m=19 MiB, t=2, p=1). The InterAgent "bcrypt" line is the **only** place this disagree; ADR-0003 is the source of truth. |
| `permissions_required: true` | Adopted | all `/v1/*` endpoints except `/healthz` |
| `external_content_flagging: true` | **Adopted (reformulated)** | The original InterAgent wording was ambiguous. The Conclave reformulation, captured in the ADR-0008 revision, models `trust_origin` as a key-level attribute (`internal` \| `external`) and **denormalises it onto every message** at send time. External messages are passed through (the server is a dumb pipe) but the receiving client can render them distinctly. See ADR-0008 §1. |

### 10. Deployment

| Item | Disposition | Where |
|---|---|---|
| docker | Adopted | `packaging/docker/` (multi-stage, distroless, non-root, healthcheck) |
| binary_release | Reformulated | the code is release-ready (multi-arch via `GOOS/GOARCH`), but there is no CI workflow that publishes a GitHub/Gitea release artefact. Tracked as a follow-up (Phase 60). |
| sqlite_default | Adopted | `cmd/serve/serve.go` default |

### 11. Success metrics

| Metric | Disposition | Test |
|---|---|---|
| agents can exchange messages | Adopted | `tests/e2e/surface_test.go::TestE2E_Surface_MessagesSendList` |
| room-based access control works | Adopted | `tests/e2e/security_test.go::TestSecurity_PermissionsEnforced` |
| audit log available | Adopted | `tests/e2e/surface_test.go::TestE2E_Surface_AuditList` |
| no framework dependencies | Adopted | `go.mod` review (chi, cobra, modernc.org/sqlite, oklog/ulid, argon2id) |

## Q1 — Q10 (the questions this ADR resolves retroactively)

- **Q1: room visibility** — both `private` and `public`, with
  `private` default. **Adopted** (SPEC-0001 Q6).
- **Q2: agent identity** — the API key **is** the agent; no
  separate `agents` table. **Reformulated** (SPEC-0001 §5 A3).
- **Q3: message intent types** — SQL column with CHECK
  constraint on the 8 enum values. **Reformulated** (SPEC-0001 Q7,
  re-reformulated from the tagged-union proposal in this ADR's
  initial §4).
- **Q4: message deletion** — soft-delete via `messages.deleted_at`.
  **Reformulated** (SPEC-0001 Q8).
- **Q5: room deletion** — soft-delete via `rooms.archived_at`
  (the `archive` endpoint). **Reformulated** (SPEC-0001 Q8).
- **Q6: api_key_hashing** — argon2id. **Reformulated** (ADR-0003).
- **Q7: audit retention** — 730 days default, 30-day floor.
  **Reformulated** (ADR-0004).
- **Q8: external_content_flagging** — **reformulated** as
  `trust_origin` on the API key, denormalised onto every message
  at send time. See ADR-0008 (revised) and SPEC-0002 §3.5.
- **Q9: MCP transports** — stdio + JSON-RPC-over-HTTP + SSE.
  The SSE endpoint at `GET /v1/rooms/{id}/events` is the
  real-time subscription channel. SPEC-0001 §3 is
  partially overridden by ADR-0009. **Reformulated** (ADR-0009).
- **Q10: audit room filter** — denormalised `audit.room_id`
  column. **Adopted** (SPEC-0001 Q10, SPEC-0002 §3.6).

## Consequences

Positive
- One document records the **full InterAgent → Conclave
  feature delta**, including the items that were dropped
  (agent entity, observer/moderator roles, streamable_http
  envelope).
- Future readers can audit a single file instead of diffing the
  InterAgent YAML against the Conclave specs.
- The four `*_agent` MCP tools are explicitly **superseded**
  by the key model with rationale (ADR-0001 §A3), so the
  InterAgent terminology is not lost in translation but is
  not implemented as a separate concept.
- The SSE transport and `trust_origin` together close the
  biggest practical InterAgent gaps (subscribe_room,
  external_content_flagging) without bringing back the
  agent entity or the streamable_http envelope.

Negative / Risks
- The InterAgent spec is not deleted from the vault; future
  readers may be confused if they find it without reading
  this ADR first. Mitigation: `docs/INTERAGENT-AUDIT.md` is a
  short operator-facing summary that links here.
- A future migration that wants to reintroduce the `agent`
  entity will need to revisit SPEC-0001 §5 A3; this ADR
  documents the rationale but does not prevent the change.
- SSE adds a long-lived connection per subscribed client.
  The server's per-connection goroutine count grows
  linearly with subscribers; for v0.x scale (single
  trust boundary, a few operators, dozens of subscribers
  at most) this is fine, but a v0.3+ scale-up would need
  a pub/sub fan-out or backplane.

## Follow-up

- ADR-0008 (revised 2026-06-27 evening) — `external_content_flagging`
  is reformulated as `trust_origin` on the key + message. The
  original "out of scope" decision is **superseded**.
- **ADR-0009 (new, 2026-06-27 evening)** — SSE transport
  partially overrides SPEC-0001 §3. Records the
  accept-SSE-but-keep-polling decision.
- Migration 0003 — adds `rooms.visibility`, `messages.message_type`,
  `messages.metadata`, `messages.deleted_at`, `audit.room_id`.
- Migration 0004 — adds `api_keys.trust_origin`,
  `messages.trust_origin`.
- Code patches — wire the new schema into the services; add
  `DELETE /v1/rooms/{id}/messages/{msg_id}`; add MCP tools
  `rooms.delete`, `messages.delete`, `rooms.invite`,
  `rooms.remove`, `messages.subscribe`; add `POST /v1/mcp`
  JSON-RPC-over-HTTP endpoint; add `GET /v1/rooms/{id}/events`
  SSE endpoint; expose `trust_origin` in create/list responses
  for keys and messages.
