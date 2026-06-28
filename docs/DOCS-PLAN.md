# Conclave MCP — Documentation Plan (2026-06-28)

This document explains the documentation structure of the
Conclave MCP repository. It is itself part of the repo and
is the entry point for anyone asking "where do I find X?".

## TL;DR

* **README.md** — what it is, quickstart, links to everything else.
* **CHANGELOG.md** — version history, "what changed in v0.1.1".
* **CONTRIBUTING.md** — how to contribute (branch, spec, code, PR).
* **docs/cli-reference.md** — every CLI command with examples.
* **docs/agent-integration.md** — how to wire an agent to Conclave.
* **docs/architecture.md** — high-level data flow + package map.
* **docs/trust-model.md** — internal vs external, who classifies, consequences.
* **docs/adr/** — Architecture Decision Records (mirrored from the
  Obsidian vault; the vault is the source of truth, see
  `docs/adr/README.md`).
* **docs/runbook.md** — operator runbook (install, upgrade, backup, monitor).
* **packaging/* README.md** — deployment sub-topics.
  deployment sub-topics.

## Where are the Specs?

Specs and ADRs live in the maintainer's Obsidian vault
(`/workspace/notes/Conclave/`). The vault syncs to CouchDB via
Obsidian LiveSync; it is **not** part of the git repo.

Top-level ADRs (those that define architecture other contributors
need to see) are mirrored into `docs/adr/`. The full Spec
collection (SPEC-0001..SPEC-0009 + minor) lives in the vault
and is referenced by file path or commit hash from the repo
docs when needed.

If you are a contributor and need access to a spec that is not
mirrored in the repo, open an issue and ask.

## Who is each doc for?

| Doc | Audience |
|---|---|
| README.md | Anyone evaluating the project. Quickstart in 5 min. |
| CHANGELOG.md | Anyone upgrading or curious about version differences. |
| CONTRIBUTING.md | New contributors. Read before opening a PR. |
| docs/cli-reference.md | Operators and integrators using the CLI. |
| docs/agent-integration.md | Agent authors wiring their agent to Conclave. |
| docs/architecture.md | Anyone wanting to understand the codebase. |
| docs/trust-model.md | Operators thinking about trust classification policy. |
| docs/adr/* | Contributors asking "why was X done this way?". |
| docs/runbook.md | Operators running a production deployment. |
| packaging/* | Operators deploying via Docker or behind a reverse proxy. |
