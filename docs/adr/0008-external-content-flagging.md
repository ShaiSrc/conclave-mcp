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
adr_id: ADR-0008
title: trust_origin replaces external_content_flagging
status: Superseded
date: 2026-06-27
deciders: Project Owner
tags: [security, scope, trust, key-attribute, message-attribute]
project: Conclave
related: [ADR-0001, ADR-0007, ADR-0009, SPEC-0001, SPEC-0002]
superseded_by: ADR-0007 §3 (this ADR is the v0.x disposition; the revised ADR-0007 records the reformulation)
---

# ADR-0008 — `trust_origin` replaces `external_content_flagging`

## Status

**Superseded by the ADR-0007 revision (2026-06-27 evening).**

The original version of this ADR (2026-06-27 morning) declared
`external_content_flagging` to be **out of scope** for v0.x because
the InterAgent spec did not define what "external" meant and the
Conclave single-trust-boundary model had no obvious anchor for the
flag. The original text is preserved below as §0 ("Original
disposition, 2026-06-27 morning").

In the evening session the project owner supplied a definition:

- **internal**: an agent from a trusted network (own
  infrastructure, personal partner agents). Default for
  server-startup keys and for the `--trust-origin=internal`
  flag on `keys create`.
- **external**: an invited agent that we **do not** trust
  by default, even if the room owner invited them (e.g.
  a collaborator's third-party agent). Marked with
  `--trust-origin=external` on `keys create`.

This gives the flag a clear semantic anchor: the **API
key** is the trust origin, and the server denormalises
it onto every message the key writes.

The original "out of scope" disposition is therefore
reformulated, not just removed. The implementation
schema, behaviour, and edge cases are recorded in §1 of
this ADR. Migration 0004 ships the schema; SPEC-0002
§3.2 + §3.5 are the spec sources.

## §1. Reformulated decision (2026-06-27 evening)

### 1.1. Definition

`trust_origin` is a property of an API key, with two valid
values: `internal` (default) and `external`. The semantics
are:

- An **internal** key is one whose bearer the operator
  trusts as part of the same trust boundary. Internal
  messages are not flagged specially.
- An **external** key is one whose bearer is invited
  into a room but is not part of the operator's trust
  boundary. External messages are passed through
  (the server is a dumb pipe) but the `messages.trust_origin`
  column records the origin so the receiving client can
  render external content distinctly.

The classification is **at the key, not at the message**.
When a key is created, the operator decides. When the key
sends a message, the server copies the key's
`trust_origin` onto the message row. When the key is
revoked, future messages from that key are rejected
(revocation wins over origin).

### 1.2. Schema (Migration 0004)

```sql
ALTER TABLE api_keys
  ADD COLUMN trust_origin TEXT NOT NULL DEFAULT 'internal'
  CHECK (trust_origin IN ('internal','external'));

ALTER TABLE messages
  ADD COLUMN trust_origin TEXT NOT NULL DEFAULT 'internal'
  CHECK (trust_origin IN ('internal','external'));

CREATE INDEX idx_messages_room_trust_ts
  ON messages(room_id, trust_origin, ts)
  WHERE deleted_at IS NULL;
```

The `idx_messages_room_trust_ts` partial index is for the
common query "list external messages in a room since X"
which is the primary use case for the flag.

### 1.3. Behaviour

- `keys create --trust-origin=external` → 201 with the
  usual `id, key, label, trust_origin` response.
- `keys list` returns `trust_origin` for every key.
- `messages.send` reads the caller's `trust_origin` from
  `api_keys` and stores it on the new message row. The
  sender cannot override the value.
- `messages.list` returns `trust_origin` for every
  message. The optional `?trust_origin=external` filter
  narrows the result. The default is no filter.
- `messages.subscribe` (SSE) includes `trust_origin` in
  every event payload.
- The `audit.detail` JSON for `message.send` includes
  `trust_origin` so the audit log captures the
  classification at the time of send.

### 1.4. Why we do NOT block external messages by default

The owner of a room has **explicitly** invited the
external key. Blocking the resulting messages would
defeat the purpose of the invite. The classification is
a **client-side hint** ("render this differently") plus
an **audit-trail marker**, not a server-side access
control. If the operator wants hard blocking, they can
add a per-room ACL rule in v0.3+ (see
`docs/INTERAGENT-AUDIT.md` §13 Open Questions for v0.3).

### 1.5. Migration behaviour

Existing keys (from migration 0003 or earlier) are
backfilled with `trust_origin = 'internal'`. This is a
**safe default** because:

- All pre-migration keys were created by server startup
  or by a single-operator workflow; both are internal.
- Operators who want to mark specific keys as external
  must do so explicitly with `keys update --trust-origin=external`
  (added in this commit, owner-only).

Existing messages (from migration 0003 or earlier) are
backfilled by copying `api_keys.trust_origin` via a JOIN
on `messages.author_key_id`. This preserves the audit
history of which key wrote which message.

## §0. Original disposition (2026-06-27 morning, preserved for traceability)

The InterAgent v1.0 spec listed `external_content_flagging: true`
as a security feature. The wording does not define what
"external content" means or how a flag is set; it is a
placeholder for "messages from outside the trust boundary should
be marked so that downstream agents can handle them carefully".

In the Conclave single-trust-boundary model, the "external" /
"internal" distinction was not defined: every message that
reaches the server has already crossed at least one trust
boundary (the caller's HTTP client). The placeholder feature
was therefore rejected as ambiguous. ADR-0007 §9 originally
recorded this as "Out of scope."

The §1 reformulation supersedes this rejection: the
project owner's definition of "internal" vs "external" in
the evening session provides the semantic anchor the
original spec lacked. The implementation is now possible
without ambiguity.

## Consequences (of the §1 reformulation)

Positive
- `trust_origin` is a precise, testable concept. The
  server-side schema, the audit trail, the list filter,
  and the SSE event payload all expose it consistently.
- A receiving client (e.g. the operator's Hermes subagent)
  can render external messages with a distinct style,
  filter them by default, or route them to a separate
  review queue — all without server-side changes.
- The audit log records the trust origin at send time,
  so a future forensic analysis can answer "what did
  this external collaborator say in this room last
  Tuesday" without joining `api_keys` to `messages` (the
  denormalisation is on purpose).

Negative / Risks
- The flag is **client-side** in the sense that the
  server does not enforce any behaviour on the basis of
  `trust_origin`. An operator who expects "external"
  to mean "blocked" will be surprised. The docs and
  the runbook make this explicit; the schema column
  name and the `CHECK` constraint are the only
  enforcement.
- Denormalising `trust_origin` onto every message means
  that if the operator reclassifies a key (e.g.
  `keys update --trust-origin=external` after the fact),
  **past messages keep the old classification**. This is
  the right behaviour for an audit trail (the message
  was sent as internal, so it is recorded as internal),
  but it is non-obvious. Documented in the runbook.
- A future "v0.3 federated" architecture would need to
  reconcile the local `trust_origin` classification
  with whatever the federation partner uses. This ADR
  does not address that case.

## Alternatives considered (carried over from §0)

- **Add a boolean `messages.is_external` column.**
  Rejected in favour of an enum because the operator
  may later want more than two values (e.g. `partner`,
  `contractor`, `public-demo`).
- **Add a server-side content classifier** (URLs, code
  blocks, etc.). Rejected: scope creep, not the question
  the operator asked.
- **Enforce external-blocked by default** at the room
  level. Rejected for v0.x: the operator's stated model
  is "invited = allowed, classified = flagged", not
  "external = blocked". A v0.3 per-room ACL can add
  this.

## Follow-up

- ADR-0007 (companion) — records the same disposition in
  the InterAgent feature-audit table. The `*_agent`
  supersession and the SSE adoption are the two
  co-decisions from the same session.
- Migration 0004 — adds `api_keys.trust_origin` and
  `messages.trust_origin` with the partial index and
  the backfill logic.
- Code patches — `keys create/update --trust-origin`,
  `messages send/list` expose and filter, MCP tools
  expose, CLI flags.
- Docs — `docs/runbook.md` §3 (server startup) and
  §5 (operations) gain a paragraph on trust origin.
