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
adr_id: ADR-0006
title: Rate-limit middleware must read from config, not hardcode
status: Accepted
date: 2026-06-27
project: Conclave
related_specs: [SPEC-0003, SPEC-0007]
related_adrs: [ADR-0001, ADR-0005]
---

# ADR-0006 — Rate-limit wiring

## Context

The Phase-20/30 bridge commit `1b53c0a` wired the real HTTP
control plane into the `conclave-mcp serve` subcommand. During
that work, the rate limiter was mounted with a hardcoded
`RateLimitMiddleware(60, 30)` in `internal/server/server.go`.

That hardcoding contradicts two documents:

- **SPEC-0003 §6**: "Per-key requests per minute: 600". The
  default rate limit is 600 rpm, overridable via
  `RATE_LIMIT_PER_MIN`.
- **SPEC-0003 §6** (rate-limit headers): `X-RateLimit-Limit` and
  `Retry-After` must reflect the actual limit in effect.

The `config` package already plumbs `RateLimitPerMin` from the
environment (`internal/config/config.go` line 30, default 600,
env `RATE_LIMIT_PER_MIN`, validated `> 0`). The `serve` subcommand
already reads it into the boot log (`cmd/serve/serve.go` line 76).
The middleware itself already accepts `(perMinute, burst int)`
and falls back to 600/30 if called with non-positive values
(`internal/server/ratelimit.go` lines 30-36). The only thing
missing is the wiring from `config.Config.RateLimitPerMin` into
the middleware call.

If the operator sets `RATE_LIMIT_PER_MIN=60` (a legitimate
operational choice, e.g. for a public test instance), the server
silently continues to enforce 600 rpm. The env var is a lie.

## Decision

**Spec-compliant: pass `cfg.RateLimitPerMin` and a burst derived
from it (`max(30, RateLimitPerMin/20)`) to
`RateLimitMiddleware`.** This is option (a) in SPEC-0007 §7.

Burst formula: `BurstFromRate(rpm int) int { return max(30, rpm/20) }`.

Why this formula:

- At the spec default 600 rpm → burst 30. Same numbers as the
  Phase-20/30 bridge placeholder, so no observable behaviour
  change for operators who never set the env var.
- Scales linearly with the rate, so the burst stays a small
  fraction of the per-minute budget. A naive `burst = rpm`
  would let a key drain its full minute in a single second.
- Floor of 30: tests and CI fixtures that pin a low rpm
  (e.g. 60) still get a usable burst.

The helper lives in `internal/server/ratelimit.go` as the
exported function `BurstFromRate(rpm int) int`. It is unit-tested
with a small table.

The middleware call changes from
`r.Use(RateLimitMiddleware(60, 30))` to
`r.Use(RateLimitMiddleware(deps.RateLimitPerMin, BurstFromRate(deps.RateLimitPerMin)))`.

`server.Deps` grows a `RateLimitPerMin int` field. Zero values
are treated as "use the spec default" (600), matching the
existing pattern for `MaxMessageBytes` and `MaxConcurrentVerify`.

`cmd/serve/serve.go` passes `cfg.RateLimitPerMin` into `Deps`.

The `X-RateLimit-Limit` and `X-RateLimit-Reset` headers (set by
the existing `RateLimitMiddleware`) now reflect the configured
limit, not a constant. The `Retry-After` value on 429 responses
is unchanged in shape (seconds, integer, `>= 1`).

## Consequences

### Positive

- `RATE_LIMIT_PER_MIN` is honoured. Operators who lower it get
  the rate cap they asked for.
- The load test `TestLoad_RateLimitEnforced` (SPEC-0007 §6.2)
  can assert the spec value rather than a hidden constant.
- `Deps` continues to be the single composition root for the
  server; the new field follows the existing zero-means-default
  convention.

### Negative

- A change that previously could not regress the rate limit
  (because the constant never moved) now can. The unit test on
  `BurstFromRate` is the safety net.
- Bursts at low `RATE_LIMIT_PER_MIN` (e.g. 60) are 30, which is
  half the per-minute budget. If an operator pins a very low
  rate, they may see throttling sooner than expected. The
  spec-mandated default of 600 makes this an edge case.

### Neutral

- No new env var, no new config field, no new public API.
- The `X-RateLimit-*` headers are already set by the
  middleware; no change in shape.

## Alternatives considered

- **(b) Amend SPEC-0003 §6 to lower the default to 60 rpm with
  a burst of 30.** Rejected. 60 rpm is aggressive for a
  chat-shaped workload and contradicts the reference deployment
  guidance in SPEC-0005 §9. Changing the spec to match a bug
  is the wrong direction.
- **(c) Add a new `RATE_LIMIT_BURST` env var.** Rejected for
  v0.1. The burst is a derived quantity; surfacing it invites
  operators to set it independently of the rate, which leads
  to nonsensical combinations (e.g. 600 rpm, burst 1000). A
  future spec may add it if real operational feedback demands
  control over the burst shape.
- **(d) Keep the hardcode and document it.** Rejected. The
  `RATE_LIMIT_PER_MIN` env var already exists in `config` and
  validates `> 0`. Documenting that it does nothing would be
  the most user-hostile outcome.

## Verification

- `go test -count=1 -run TestBurstFromRate ./internal/server/...`
  passes. (New unit test.)
- `go test -count=1 ./...` exits 0.
- A manual `RATE_LIMIT_PER_MIN=60 ./conclave-mcp serve` and
  61 rapid requests within 60s shows the 61st as 429 with
  `Retry-After: 1`.

## Follow-up

- None for v0.1. The SPEC-0007 §6.2 load test
  `TestLoad_RateLimitEnforced` is the regression guard.
