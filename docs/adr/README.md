# Architecture Decision Records (ADRs)

This directory contains a **mirror** of the Architecture
Decision Records for Conclave MCP. The authoritative copies
live in the maintainer's Obsidian vault
(`/workspace/notes/Conclave/ADRs/`) and are synced to
CouchDB via Obsidian LiveSync.

## What is mirrored and what is not

The following ADRs are mirrored here because they describe
architecture that an external contributor needs to see:

* [ADR-0001](0001-name-change-conclave-mcp.md) — project
  rename from InterAgent MCP to Conclave MCP.
* [ADR-0003](0003-api-key-hashing-argon2id.md) — argon2id
  parameters for API key hashing.
* [ADR-0006](0006-rate-limit-wiring.md) — rate-limit
  formula `max(30, rpm/20)`.
* [ADR-0007](0007-interagent-audit.md) — feature parity
  audit against the legacy InterAgent v1.0 spec, and the
  decision to use `trust_origin` instead of the legacy
  `*_agent` toolset.
* [ADR-0008](0008-external-content-flagging.md) —
  `trust_origin` as the replacement for
  `external_content_flagging`. Read this before proposing
  changes to the trust model.
* [ADR-0009](0009-sse-transport.md) — the decision to add
  SSE as an additional transport, partially overriding
  SPEC-0001 §3. Read this before proposing changes to the
  message delivery model.

The following ADRs live only in the vault because they are
internal-process or not relevant to external contributors:

* **ADR-0002** (artefact migration InterAgent → Conclave):
  describes the maintainer's private migration of files
  from one vault folder to another. Not interesting to
  external readers.
* **ADR-0004** (audit retention 730 days): already
  documented in [docs/runbook.md](../runbook.md). The
  underlying decision is operational, not architectural.
* **ADR-0005** (vault is source of truth): describes the
  maintainer's private documentation workflow. You are
  reading the consequence of this ADR right now (this very
  README).

If you need access to a non-mirrored ADR, open an issue and
ask.

## Source of truth and drift

The vault is the source of truth. The mirror is updated by
the maintainer on each ADR change. The mirror **may lag**
the vault by hours to days; if you need the latest version
of an ADR, read the vault copy.

If the mirror disagrees with the vault:

* The vault is correct.
* The mirror is stale.
* Open an issue and the maintainer will re-sync.

If the mirror disagrees with code:

* The code is correct.
* The ADR is stale (a new ADR supersedes it, or a
  follow-up commit implemented the decision differently).
* The follow-up commit's message will reference the new
  ADR.

## How to write a new ADR

If you are proposing a change that affects architecture,
auth, the data model, the rate-limit formula, the trust
model, or the storage engine, you need an ADR.

The ADR lives in the vault. The maintainer will mirror it
to this directory once it is accepted. The format:

```markdown
---
adr_id: ADR-NNNN
title: <short, sentence-case, ends without a period>
status: Accepted | Superseded | Proposed
date: YYYY-MM-DD
project: Conclave
related_adrs: [ADR-XXXX, ...]
related_specs: [SPEC-XXXX, ...]
supersedes: ADR-XXXX (optional)
---

# ADR-NNNN: <title>

## Context
<What is the situation that forces a decision?>

## Options
<What were the options? What are the trade-offs?>

## Decision
<What was decided?>

## Consequences
<What follows from the decision? What becomes easier?
What becomes harder? What is now impossible?>

## Open questions
<What is still unclear? What is the next decision?>
```

The full template is in
[`0001-name-change-conclave-mcp.md`](0001-name-change-conclave-mcp.md)
and the rest. Look at ADR-0007 and ADR-0009 for examples of
non-trivial ADRs with multiple options and consequences.
