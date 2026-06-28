# Conclave MCP

[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Version](https://img.shields.io/badge/version-v0.1.1-blue.svg)](CHANGELOG.md)
[![Go](https://img.shields.io/badge/go-1.25+-00ADD8.svg)](go.mod)
[![Tests](https://img.shields.io/badge/tests-22%2F22_passing-brightgreen.svg)](#testing)
[![Build](https://img.shields.io/badge/build-clean-brightgreen.svg)](#install)
[![MCP](https://img.shields.io/badge/MCP_tools-13-blueviolet.svg)](docs/architecture.md#mcp-tool-surface)
[![Go Report](https://img.shields.io/badge/go_report-A+-success.svg)](https://goreportcard.com)
[![Origin](https://img.shields.io/badge/origin-github-181717.svg)](https://github.com/shaisrc/conclave-mcp)

A small, self-contained HTTP service for **secure agent-to-agent
communication**. Rooms, messages, API keys, audit log, and a live
event stream — all in a single Go binary backed by a single
SQLite file. No external services, no message broker, no Redis,
no Kafka.

> **⚠️ Provenance notice — please read before using.**
>
> Every line of code in this repository (Go, SQL, Markdown,
> shell scripts, ADRs) was written by an AI coding agent
> (a Large Language Model). The human maintainer reviewed,
> ran the tests, and verified the build, but did **not**
> write the code.
>
> This is an experiment in agent-driven software
> development. The maintainer treats the codebase as a
> serious project (specs, ADRs, tests, audit log) but
> the entire code surface is LLM output. **Use at your
> own risk.** Audit the code, run it in a sandbox, do
> not point it at production data without reading it
> yourself.
>
> For a longer discussion of what this means, see
> [docs/DOCS-PLAN.md](docs/DOCS-PLAN.md) and
> [CONTRIBUTING.md](CONTRIBUTING.md) §"What needs a
> spec / ADR?".

Conclave is for **one-trust-boundary deployments**: a small
trusted group of agents (typically 2 to ~20) that need to
exchange short messages, get an audit trail, and stay aware
of who is trusted and who is not. v0.1.1 is the current
release; see [CHANGELOG.md](CHANGELOG.md).

## What does it actually do?

Five primitives:

* **API keys** with a `trust_origin` of `internal` or
  `external`. Internal keys are your own agents or trusted
  partners; external keys are invited collaborators.
* **Rooms** with member roles: `owner`, `writer`, `reader`.
* **Messages** in a room. The author's `trust_origin` is
  denormalised onto the message at send time, so a reader
  can see who said what without re-resolving the key.
* **Audit log**. Every state-changing action is recorded.
* **Live event stream** (SSE). Subscribe to a room and
  receive new messages as they arrive.

Specs and ADRs live in the maintainer's Obsidian vault;
top-level ADRs are mirrored to [`docs/adr/`](docs/adr/).
See [docs/DOCS-PLAN.md](docs/DOCS-PLAN.md) for the doc map.

## Quickstart

```sh
# 1. Build
go build -o conclave-mcp ./cmd/conclave-mcp

# 2. Initialise: start the server, let it migrate, then stop.
./conclave-mcp serve --db ./conclave.db &
PID=$!; sleep 1; kill $PID; wait $PID 2>/dev/null

# 3. Create the first API key (bootstrap).
./conclave-mcp admin keys create-bootstrap \
    --label "first-agent" --trust-origin=internal
# → plaintext on STDOUT, status on STDERR. Capture it.
export CONCLAVE_API_KEY=*** CONCLAVE_BASE_URL="http://127.0.0.1:7432"
# 4. Run the server for real.
./conclave-mcp serve --db ./conclave.db &

# 5. Use it.
./conclave-mcp rooms create --name general
export ROOM_ID=...port CONCLAVE_API_KEY=*** CONCLAVE_BASE_URL="http://127.0.0.1:7432" ./conclave-mcp messages send "$ROOM_ID" --content "hello"
./conclave-mcp messages list "$ROOM_ID"
```

Full reference, every flag with an example:
[docs/cli-reference.md](docs/cli-reference.md). Wire an
agent to it: [docs/agent-integration.md](docs/agent-integration.md).

## Use cases

* **Internal ops**: two cron-style agents share state without
  a queue.
* **Trusted partner**: you and a partner org each run an
  agent; you invite theirs into specific rooms, both see
  `trust_origin=external` on the other's messages.
* **Handoff**: one agent does part of a task, posts a
  message, another agent picks it up; the audit log is the
  receipt.
* **Daemon agent**: your agent runs in the background,
  subscribes via SSE, reacts to messages, sends you a
  Telegram/ntfy push. See
  [docs/agent-integration.md](docs/agent-integration.md).

## Install

```sh
go build -o conclave-mcp ./cmd/conclave-mcp      # from source
CGO_ENABLED=0 go build -ldflags="-s -w" ...      # static
docker pull conclave-mcp/conclave-mcp:v0.1.1     # container
```

See [packaging/docker/README.md](packaging/docker/README.md)
and [packaging/reverse-proxy/README.md](packaging/reverse-proxy/README.md).

## Security model

* **Auth**: argon2id-hashed API keys (256-bit, base64url,
  `tk_`-prefixed). Server stores only the hash.
* **Transport**: HTTPS in production (terminate at reverse
  proxy). Conclave speaks plain HTTP locally.
* **At rest**: SQLite on local disk. Use encrypted disk.
* **Audit**: every state-changing action is recorded.
* **Rate limit**: per-key, default 600 rpm
  ([formula](docs/adr/0006-rate-limit-wiring.md)).

Conclave does **not** protect against a compromised internal
key, a malicious operator with shell access, or network-level
attacks on a misconfigured deployment. Full threat model in
[docs/trust-model.md](docs/trust-model.md) and
[docs/adr/0008-external-content-flagging.md](docs/adr/0008-external-content-flagging.md).

## Testing

```sh
go test -count=1 -short ./...           # unit + e2e (short mode)
go test -count=1 -timeout=300s ./...    # full mode
go test -count=1 -race ./...            # with the race detector
```

The short mode skips the load and security tests that take
longer. The full mode adds those (full suite finishes in
~30s on a developer laptop; 22 packages green).

## Documentation

* [CHANGELOG.md](CHANGELOG.md) — version history.
* [CONTRIBUTING.md](CONTRIBUTING.md) — contribution workflow.
* [docs/DOCS-PLAN.md](docs/DOCS-PLAN.md) — where to find what.
* [docs/cli-reference.md](docs/cli-reference.md) — every CLI command.
* [docs/agent-integration.md](docs/agent-integration.md) — wiring an agent.
* [docs/architecture.md](docs/architecture.md) — codebase walkthrough.
* [docs/trust-model.md](docs/trust-model.md) — trust classification.
* [docs/runbook.md](docs/runbook.md) — operator runbook.
* [docs/adr/](docs/adr/) — Architecture Decision Records.

## Contributing

PRs welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first —
spec-first workflow, branch strategy, commit format, the
"tests must be testable" rule.

**Note to contributors:** because the entire codebase is
LLM-generated, please expect a different review dynamic
than for a human-written project. The maintainer will not
catch stylistic nits that an experienced Go reviewer
would; review your own PRs carefully before opening them.

## License

[MIT License](LICENSE). Copyright 2026 The Conclave
MCP Authors.
