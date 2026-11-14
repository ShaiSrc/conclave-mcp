# Release candidate (v0.14.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Freeze the federation protocol as version 1 (document, vectors, route list, extension rules), prove three servers work together, fuzz every parser that reads another server's bytes, add an online backup and a server info view, review the whole surface for security and API stability, and write the upgrade guide.

**Architecture:** Protocol code lives in `internal/federation` and `internal/fedrooms`; the interop suite in `tests/e2e`; fuzz targets next to the code they fuzz; backup and server info in `cmd/conclave-mcp`, `internal/server`, `internal/mcp` and `internal/ui/assets`. No migration.

**Tech Stack:** Go 1.25, chi, modernc SQLite, vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-14-release-candidate-design.md`. Earlier specs in the same folder still hold.
- No new migration. Never edit released migrations 0001–0016.
- Test in Docker:
  - suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - race detector on touched packages; `gofmt -l .` empty; `go vet ./...`
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- TDD: failing test first.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comment or doc mentions a person; say "operator". Never print keys.

---

### Task 1: Protocol version 1 (document, vectors, routes, extension rules)

- **Extension rules in code:**
  - `applyEvent` (`internal/federation/replica.go`): an unknown event type is skipped (returns nil, so `applied_seq` moves past it) instead of `refused`. Same on the pull path.
  - Check every `/fed/v1` body decoder and the server document decoder: none may reject unknown fields. Add a test per endpoint with an extra field.
- **Vectors:** `docs/protocol-v1-vectors.json` with a fixed 32-byte Ed25519 seed (hex), its public key (base64url), fingerprint and key ID. Also:
  - one request: method, path and query, destination, origin, ts, nonce, body, the exact signing string, the `Authorization` header, the signature;
  - one endorsement string and its signature;
  - one author signature (`conclave-msg-v1`) with its fields, signing string and signature.

  `internal/federation/vectors_test.go` (`TestProtocolVectors`) recomputes each value with the package's own functions (`SigningString`, `formatSigHeader`, `EndorsementString`, `MessageSig.SigningString`, `Fingerprint`, `KeyIDFor`) and compares.
- **Protocol document:** `docs/protocol.md`, "Conclave federation protocol, version 1". It is normative: MUST, SHOULD and MAY in their RFC 2119 sense. Write it from the code, which is the source of truth, and from the existing docs (`federation.md`, `federated-rooms.md`, `direct-messages.md`, `follow.md`, `bots.md`). Sections:
  - conventions;
  - transport and the federation listener;
  - server document;
  - keys, rotation and endorsements;
  - request signatures;
  - verification order;
  - addresses and domains (normalization rules, handles);
  - errors (shape, codes and statuses);
  - endpoints: one subsection per route, with method, path, request body, answer and errors. Include a table "Endpoints" listing every `METHOD /path` exactly as routed;
  - room events (types and payloads), author signatures, delivery and catch-up, limits;
  - extension and versioning rules (spec wording);
  - test vectors (pointer to the JSON).

  Where code and docs disagree, the code wins: note the doc fix in the commit.
- **Operator docs:** the operator docs link to `protocol.md` for the wire format. In `federation.md`, replace the "Status: 0.8. This is the start of the protocol document…" line with a pointer to `protocol.md`, which wins on conflicts.
- **Routes:** `internal/federation/routes_test.go` (`TestProtocolRoutes`) walks the federation listener's router (`chi.Walk`) and compares its `METHOD path` set with the rows of the "Endpoints" table in `docs/protocol.md`, parsed from the file. Public HTML pages and `/.well-known/conclave` are in the table too.
- [ ] TDD, then commit `feat(federation): protocol version 1 (document, vectors, routes, unknown events skipped)`

---

### Task 2: Fuzz targets

- Go native fuzzing (`func FuzzX(f *testing.F)`), seeds from real values (valid headers, documents, bodies from existing tests). Targets and properties:
  - `FuzzParseSigHeader`: no panic; a parsed header formats back to a header that parses to the same params.
  - `FuzzVerifyRequest`: random `Authorization`, `Conclave-*` headers and body against a test service with one allowed peer. Never 2xx, never a panic.
  - `FuzzDocument`: decode plus the checks the fetcher and re-pinning apply, endorsements included. No panic.
  - `FuzzNormalizeDomain`: no panic; a successful output normalizes to itself.
  - `FuzzAddress`: address parsing (`name@domain`, through `bots.NormalizeHandle` / `NormalizeRemoteName` and `NormalizeDomain`). No panic; the output round-trips.
  - `FuzzInbound`: for each `/fed/v1` route that takes a body, the fuzzed body is signed by the test peer's key so it passes verification. Never 5xx, never a panic. One target with the route index as a fuzz argument is fine.
  - `FuzzApplyEvents`: random event batches pushed to a replica. Never 5xx, never a panic.
  - `FuzzAuthorSig`: random `MessageSig` fields and signature. Verification never panics and never succeeds for a random signature.
  - `FuzzPublicPage`: random profile name, description, skills and post text. The HTML parsed with `golang.org/x/net/html` (only if it is already a dependency; otherwise `html/template`'s escaping is checked with a tokenizer written in the test) has no element or attribute the template does not write.
  - `FuzzFeed`: the same, and the Atom output parses with `encoding/xml`.
- Crashers go to `testdata/fuzz/<Target>/` as regression seeds with the fix in the same commit.
- CI: a `fuzz` job in `.github/workflows/ci.yml` (needs `lint`) runs each target `-fuzztime=15s` (a loop over `go test -list '^Fuzz'` per package).
- [ ] Commit `test: fuzz targets for every federation parser` (plus `fix:` commits for what they find)

---

### Task 3: Interop suite with three servers

- `tests/e2e/interop_test.go`, `TestInterop_ThreeServers`, built on `newFedEnv` and the moderation helpers. Servers A, B and C each peer the other two (allowlist). Sub-steps run in order as `t.Run`:
  1. lookup and directory across servers;
  2. a room on A with bots of B and C. Messages from all three appear in the same order on every server. B's author signature is verified on C (`verified` true in C's replica). Claims work across servers, a member leaves, and a deletion is retracted on every server;
  3. a direct conversation between bots of B and C, both ways;
  4. bots of B (open policy) and C (approve, then accept) follow a bot of A; A posts once, each timeline has it once;
  5. C is stopped (`close`), A's room gets messages, C is started again on the same data dir and catches up;
  6. B rotates its key. A and C keep working with B without operator steps (B writes in A's room, C verifies it, a DM from B to C);
  7. A blocks a bot of C, then blocks and purges C. B's replica of A's room retracts the messages of C's bots.
- Wait with polling helpers (no fixed sleeps), as the existing e2e tests do.
- `go test -count=3 -run TestInterop ./tests/e2e/` passes.
- Bugs found get a unit test and a `fix:` commit.
- [ ] Commit `test(e2e): interop suite with three servers`

---

### Task 4: Backup and server info

- **Backup:** `conclave-mcp backup <file>` (`cmd/conclave-mcp`, same data dir and DB resolution as `serve`):
  - writes the copy with `VACUUM INTO ?` over a read connection;
  - refuses an existing file (exit code as other usage errors);
  - sets mode 0600 on the result;
  - prints the path and size.

  No HTTP endpoint, no MCP tool. Test: a copy taken while another connection writes opens and holds the rows; an existing file is refused; the mode is 0600.
- **Server info:**
  - `GET /v1/server` (admin keys only, 403 otherwise) answers `{version, schema_version, started_at, federation: {enabled, domain, mode, protocol}}`. `version` is the build's version string (pass it into `server.Config` / Deps from `main.go`); `schema_version` is from `meta`.
  - CLI `conclave-mcp server` prints it (`--json` as other commands).
  - MCP `server.info` (admin keys, read-only) answers the same. Tool count +1 (README badge and docs).
- **Web UI:** a "Server" nav entry and page (`internal/ui/assets/server.js`, routed in `app.js`) showing the info and the backup command (`conclave-mcp backup /path/to/conclave-backup.db`) with a link to the upgrade guide on GitHub (`https://github.com/ShaiSrc/conclave-mcp/blob/main/docs/upgrading.md`).
- **Docs:** `docs/cli-reference.md` (`backup`, `server`), `docs/web-ui.md` (Server page), `docs/runbook.md` (online backup uses `backup`; the runbook stays ≤ 200 lines).
- [ ] TDD, then commit `feat: online backup and server info`

---

### Task 5: API review

- A review of `/v1` routes, MCP tools (`internal/mcp`) and CLI commands for anything that would be a breaking change after 1.0: inconsistent names, shapes, error codes, defaults, missing pagination on lists that grow.
- Cheap fixes land with tests, an entry under "Changed" in the CHANGELOG and a note for the upgrade guide. The rest is listed in `docs/upgrading.md` under the compatibility promise as kept as is.
- [ ] Commit `refactor(api): consistency fixes before 1.0`

---

### Task 6: Security review

- Independent reviewers by area:
  1. signatures, keys, rotation, peers;
  2. rooms, replicas, delivery, claims;
  3. DMs, follows, feeds;
  4. fetcher (SSRF), public pages, Atom;
  5. admin API, web UI, MCP tools as they expose remote content.
- Each finding is fixed with a test, or recorded as accepted with a reason.
- Decide the 0.13 accepted limits: messages past the per-host re-pin budget, and peers that missed two rotations.
- `docs/security.md`: trust boundaries, what each one relies on, accepted limits, and how to report.
- `SECURITY.md`: GitHub private vulnerability reporting; fixes go to the latest minor release.
- [ ] Commits `fix(security): …` and `docs: security model`

---

### Task 7: Upgrade guide and release prep

- `docs/upgrading.md`:
  - before upgrading;
  - the steps;
  - notes per version from 0.4 to 0.14, taken from `CHANGELOG.md`: new settings, changed behavior, migrations;
  - turning federation on;
  - peers on different versions;
  - rollback;
  - the compatibility promise from the spec, plus the API review's kept items.
- `docs/runbook.md`: its upgrade section links to the guide.
- `docs/roadmap.md` is unchanged.
- Release prep:
  - CHANGELOG `[0.14.0] — <date>`;
  - README version badge, the "v0.14.0 is the current release" line, the MCP tools badge and the tests badge.
- Browser smoke test of the Server page and the federation page with two live servers.
- [ ] Commit `docs: upgrade guide; release v0.14.0`
