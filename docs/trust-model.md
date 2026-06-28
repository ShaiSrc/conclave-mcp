# Trust Model

This document explains Conclave's trust classification
system: what `internal` and `external` mean, who sets them,
how they are propagated, and what consequences flow from the
classification. The full architecture decision is in
[`docs/adr/0008-external-content-flagging.md`](adr/0008-external-content-flagging.md)
(ADR-0008, revised 2026-06-28) and the spec line is SPEC-0001
Q11.

## The shape of the problem

Conclave is for **one-trust-boundary deployments**: a small
group of agents that need to talk to each other, where some
agents are owned by you and some are owned by people you do
not fully trust. The classic example is "you and a partner
org each have an agent; you want them to coordinate on a
shared task".

The naive way to handle this is to give every agent the same
key. That is wrong because:

* **Audit ambiguity**: if both agents use the same key, you
  cannot tell who said what in the log.
* **Trust ambiguity**: a reader cannot tell whether a
  message came from your own agent (which you trust) or from
  the partner's agent (which you only sort-of trust).
* **Revocation granularity**: if you suspect a compromise,
  you have to revoke the shared key and update both agents.

The right way is to give each agent its own key, and to
classify the keys so readers can reason about trust.

## The classification

Every API key has a `trust_origin` field. The closed set is
two values:

* **`internal`**: the key belongs to you or a trusted partner
  whose infrastructure you operate or audit. Examples:
  your own agents, an agent that runs on hardware you own, an
  agent of a partner org that has signed an agreement and
  whose codebase you have reviewed.
* **`external`**: the key belongs to a third party that you
  have invited into a specific room. Examples: a partner org
  that you do not have a full code-review relationship with,
  a contractor's agent, an external service's bot.

**The classification is operator-set.** It is set at key
creation and is not exposed in any "update" endpoint in
v0.1. To change a key's classification, revoke the old key
and create a new one with the new classification. (A future
admin command may relax this; see ADR-0007 §Open questions.)

## What the classification does

Three things:

1. **It propagates to messages.** When an agent sends a
   message, the server copies the author's `trust_origin`
   onto the message. The message's `trust_origin` is
   **immutable** for the message's lifetime, even if the
   key's classification changes later.
2. **It is queryable.** A reader can filter messages by
   `trust_origin` (`messages list --trust-origin=external`
   on the CLI, `trust_origin` filter on the MCP tool).
3. **It is a hint for the reader.** Conclave does **not**
   enforce a policy based on `trust_origin`; it does not
   block external messages or quarantine them. The
   classification is information for the receiving agent's
   trust policy. See [docs/agent-integration.md](agent-integration.md#the-trust-policy).

## What the classification does NOT do

* **It does not authenticate the message.** The argon2id
  hash does that. `trust_origin` is metadata.
* **It does not enforce trust.** Conclave does not say "you
  cannot send to room X if your `trust_origin` is external".
  The room membership and the writer role are the
  enforcement.
* **It does not change the wire format.** Internal and
  external messages look the same on the wire; the only
  difference is the `trust_origin` field.

## The lifecycle

```
+----------------+       +-----------------+       +----------------+
| operator       |       | server          |       | reader         |
| (human)        |       | (conclave-mcp)  |       | (other agent)  |
+----------------+       +-----------------+       +----------------+
        |                        |                        |
        | 1. keys create         |                        |
        |    --trust-origin=X    |                        |
        |----------------------->|                        |
        |                        |                        |
        | 2. plaintext on STDOUT |                        |
        |<-----------------------|                        |
        |                        |                        |
        | 3. (hand the plaintext |                        |
        |  to the agent out of   |                        |
        |  band)                 |                        |
        |                        |                        |
        |                        | 4. messages send       |
        |                        |    (author: agent A)   |
        |                        |<-----------------------|
        |                        |                        |
        |                        | 5. message persisted   |
        |                        |    with                |
        |                        |    trust_origin =      |
        |                        |    api_keys.trust_orig |
        |                        |                        |
        |                        | 6. message listed /    |
        |                        |    streamed to reader  |
        |                        |----------------------->|
        |                        |                        |
        |                        | 7. reader's trust      |
        |                        |    policy decides      |
        |                        |    what to do          |
```

The crucial bit is step 5: the server copies the author's
`trust_origin` onto the message at write time, and the
message's `trust_origin` does not change even if the key's
classification is later updated.

## What the classification looks like in the wire format

A message from an internal key:

```json
{
  "id": 7,
  "room_id": "01ABC...",
  "author_key_id": 2,
  "ts": "2026-06-28T07:46:28.318713134Z",
  "content_type": "text/plain",
  "content": "hello",
  "trust_origin": "internal"
}
```

A message from an external key, in the same room:

```json
{
  "id": 8,
  "room_id": "01ABC...",
  "author_key_id": 4,
  "ts": "2026-06-28T07:46:30.123456789Z",
  "content_type": "text/plain",
  "content": "hi from chef's agent",
  "trust_origin": "external"
}
```

A reader's trust policy can match on `trust_origin` and
apply different validation. The recommended policy is in
[docs/agent-integration.md](agent-integration.md#the-trust-policy).

## Threats the classification does NOT address

* **A compromised `internal` key.** A compromised internal
  key has the same access as your own agent. The
  classification does not protect against a stolen
  plaintext. **Mitigation:** rotate keys, use short-lived
  keys, monitor the audit log, run the agent on a host with
  good physical security.
* **A malicious operator with shell access.** The operator
  owns the binary, the DB, and the audit log. They can
  rewrite the `trust_origin` of any message after the fact.
  **Mitigation:** run the DB on a host that the operator
  does not own (e.g. a managed service).
* **A network attacker.** Conclave speaks plain HTTP
  locally. If you expose it to the network without TLS, an
  attacker on the wire can read and forge messages.
  **Mitigation:** terminate TLS at a reverse proxy or
  sidecar. See [packaging/reverse-proxy/README.md](../packaging/reverse-proxy/README.md).
* **A malicious internal agent.** Your own agent has writer
  access to every room it is a member of. If you let your
  own agent do something destructive, that is on you.
  **Mitigation:** trust policies in the agent, not in
  Conclave.
* **Replay of old messages.** Conclave does not protect
  against an attacker re-sending a captured SSE event.
  **Mitigation:** the receiving agent's idempotency store
  (see [docs/agent-integration.md](agent-integration.md#idempotency))
  handles this.

## Default classifications

* `keys create` (the HTTP path) defaults to `internal`.
* `admin keys create-bootstrap` defaults to `external`.

These defaults are chosen for the most common case: a fresh
install is more often a single-user setup (so the first key
should be `internal`) and a bootstrap is more often the
first key of a shared install (so the bootstrap should be
`external` to force the operator to think about the trust
classification explicitly).

Both defaults can be overridden with `--trust-origin`.

## ADR

The full decision is in
[`docs/adr/0008-external-content-flagging.md`](adr/0008-external-content-flagging.md).
The original InterAgent v1.0 spec proposed
`external_content` as a per-message flag, ADR-0008 revised
this to `trust_origin` as a key attribute that is
denormalised onto messages. The key change is that the
classification is set once at key creation, not per message,
which avoids the case where an attacker can flip the flag at
send time.
