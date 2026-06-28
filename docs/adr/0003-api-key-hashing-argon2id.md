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
adr_id: ADR-0003
title: API-key hashing algorithm — argon2id with OWASP 2024 parameters
status: Accepted
date: 2026-06-26
deciders: Project Owner
tags: [security, authentication, api-keys, crypto]
project: Conclave
related: [SPEC-0001, SPEC-0003]
supersedes: []
---

# ADR-0003 — API-key hashing: argon2id

## Status
Accepted — 2026-06-26

## Context
Conclave MCP v0.x authenticates agents by API key (see SPEC-0001 §Scope and
SPEC-0003 §Auth). Keys are 256-bit random tokens generated server-side,
shown to the user exactly once, and then must be stored such that a
database compromise does not yield usable credentials.

The choice of password-hashing algorithm is a once-and-for-all decision:
every Conclave deployment will live with it for the lifetime of its data,
because re-hashing all keys is a privileged operation that operators
rarely schedule.

## Decision
1. **Algorithm: argon2id** — the hybrid variant, resistant to both GPU
   and side-channel attacks. SHA-256-with-salt and bcrypt are explicitly
   rejected. scrypt is a viable backup if argon2id becomes unavailable,
   but is not the default.
2. **Parameters (OWASP 2024 recommendation):**
   - `m = 19 MiB` (memory cost)
   - `t = 2` (iterations)
   - `p = 1` (parallelism)
   - salt length: 16 bytes, generated per key with a CSPRNG
3. **Library pinning.** The project uses these libraries in their latest
   stable major:
   - Rust: `argon2 = "0.5"`
   - Python: `argon2-cffi = "23.1"`
   - Go: `golang.org/x/crypto/argon2`
   Custom implementations are forbidden.
4. **Storage format.** A single column `key_hash` of type `TEXT`, holding
   the standard argon2 PHC string:
   `$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>`
   This makes the parameters self-describing and allows future tuning
   without schema migration.
5. **Verification timing.** Verification must run in constant time per
   key. Libraries above are constant-time by default; we do not add
   shortcuts.

## Consequences
Positive
- Parameters are above the 2024 hardware floor; brute-force is uneconomic.
- Self-describing PHC string means we can raise `t` or `m` later without
  breaking existing hashes. A future migration step re-hashes on next
  successful login.
- A single, well-known library per language reduces supply-chain risk vs.
  rolling our own.

Negative / Risks
- ~50–100 ms per key derivation on modern x86. Acceptable for the
  expected auth rate (single-digit per second per agent) but observable
  in benchmarks; load tests must account for it.
- `m = 19 MiB` per concurrent verification. At 32 concurrent verifications
  this is ~600 MiB of RAM. Mitigation: cap concurrent verifications at
  the HTTP server level (default: `MAX_CONCURRENT_VERIFY = 32`).
- The PHC string contains the salt. This is by design (the salt is not a
  secret) and matches OWASP guidance.

## Follow-up
- A periodic security review (annual) checks whether OWASP has updated
  the recommended parameters; ADR is amended if so.
- A `conclave-mcp keys rehash` admin command re-hashes existing keys to
  current parameters on next successful login.
