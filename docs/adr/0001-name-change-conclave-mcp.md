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
adr_id: ADR-0001
title: Project Name Change — InterAgent MCP → Conclave MCP v0.x
status: Accepted
date: 2026-06-26
deciders: Project Owner
tags: [naming, branding, project-meta]
project: Conclave
related: [ADR-0005]
supersedes: []
---

# ADR-0001 — Project Name Change: InterAgent MCP → Conclave MCP v0.x

## Status
Accepted — 2026-06-26

## Context
The project was previously known as **InterAgent MCP**. A new umbrella name,
**Conclave**, has been chosen. Following discussion, the decision is:

- The umbrella name **"Conclave"** is used for the project as a whole.
- The **first public iteration** is named **Conclave MCP v0.x** (a single naming
  line — no separate v1/v2 split at the naming layer).
- All specifications (Specs) and Architecture Decision Records (ADRs) are
  persisted in CouchDB inside the Obsidian vault (`obsidiannotes` DB,
  replicated to the local vault folder by the Obsidian LiveSync plugin).
- No legacy `v1`/`v2` distinction is encoded in the product name. Versioning
  is applied only as a `v0.x` semver suffix on the single `Conclave MCP` line.

This ADR records the name change and the documentation-location decision so
that future specs, ADRs, code, and CI artefacts can refer to a single,
stable name.

## Decision
1. The product is called **Conclave MCP v0.x** for its first public iteration.
2. The previous name `InterAgent MCP` is **deprecated** for new artefacts.
   Legacy references may exist in earlier documentation and may be kept for
   historical traceability but must not be used in:
   - new specs or ADRs,
   - new code, package names, or repository paths,
   - new CI or release artefacts.
3. All Specs and ADRs are stored as `.md` files in the Obsidian vault
   folder (`/workspace/notes/Conclave/...`); CouchDB is a read-only
   mirror maintained by the LiveSync plugin. See ADR-0005 for the
   full storage model.
4. ADRs follow a lightweight RFC-7804 style: number, title, status, date,
   context, decision, consequences, and optional follow-up. A template is
   available in the `adr-management-couchdb` skill.

## Consequences
Positive
- One single, stable name for the project; no dual `v1`/`v2` to keep in sync.
- Clear separation of concerns: Specs describe *what*, ADRs describe
  *why-decided*, task descriptions describe *how-to-execute*.
- Specs and ADRs are searchable in CouchDB via Mango and viewable in Obsidian.

Negative / Risks
- Any external artefacts (repos, packages, URLs) still using `InterAgent` need
  a separate follow-up decision (covered by ADR-0002, to be written).
- LiveSync may briefly show duplicate documents if a `.md` is written to the
  vault folder while a corresponding CouchDB document is also created.

## Follow-up
- ADR-0002: Migration plan for external artefacts (Gitea repos, package names,
  README, URLs) — to be drafted.
- Spec-0001: Conclave MCP v0.x — scope, MVP features, constraints, non-goals.
