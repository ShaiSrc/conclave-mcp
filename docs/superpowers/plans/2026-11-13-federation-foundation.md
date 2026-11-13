# Federation foundation (v0.8.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two Conclave servers peer through pinned Ed25519 identities and exchange verified, signed requests.

**Architecture:** A new package `internal/federation` holds identity, signatures, peer policy, the fetch client and HTTP handlers. Each server gets a second listener that serves only the federation routes, plus admin endpoints under `/v1/federation` and a `conclave-mcp federation` CLI.

**Tech Stack:** Go 1.25, chi, modernc SQLite, `crypto/ed25519`, cobra, `golang.org/x/time/rate`.

## Global Constraints

- The spec, `docs/superpowers/specs/2026-11-13-federation-foundation-design.md`, is binding. Every name, value and status code in it is exact.
- Federation off (no `CONCLAVE_FEDERATION_DOMAIN`) changes nothing: no listener, no keys generated, `/v1/federation*` gives 404 `federation_disabled`.
- Run tests in Docker:
  - `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - gofmt: `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` must print nothing.
- Use TDD: write the failing test, see it fail, implement, see it pass.
- Commits use the repo's git identity and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Docs and code comments never mention a person; use "operator".
- Follow existing patterns:
  - error envelope via each package's `writeErr(w, r, status, code, msg)`;
  - admin checks via `auth.IsAdmin(ctx)`;
  - audit via `audit.Write(ctx, db, actorKeyID, action, targetType, targetID, roomID, result, detail)`;
  - CLI commands like `cmd/keys/keys.go` (`makeClientAndDo`, `internal/cli/client`);
  - config like `internal/config/config.go` (env lookup, flags, `validate`).

---

### Task 1: Identity, config, storage, federation listener

**Files:**
- Create: `migrations/0011_federation.sql`
- Create: `internal/federation/identity.go` and `identity_test.go`
- Create: `internal/federation/domain.go` and `domain_test.go`
- Modify: `internal/config/config.go` and its tests
- Modify: `internal/server/server.go`
- Modify: `cmd/serve/*.go` (pass the config through)

**Interfaces:**
- `federation.NormalizeDomain(s string) (string, error)`: lowercases the input, checks the spec's syntax (letters, digits, dots, hyphens, optional `:port` from 1 to 65535, at most 253 characters, no leading or trailing dot or hyphen in a label) and returns the canonical form.
- `type Identity struct { Domain string; KeyID string; Public ed25519.PublicKey; private ed25519.PrivateKey }`
  - `func LoadOrCreateIdentity(ctx, db *sql.DB, domain string) (*Identity, error)`: reads the newest unretired row in `federation_keys`, or generates and stores one. Domains are not stored with the key; the domain comes from config.
  - `func (id *Identity) Sign(msg []byte) []byte`
  - `func (id *Identity) Fingerprint() string`
- `func Fingerprint(pub ed25519.PublicKey) string` returns `SHA256:` followed by the base64url (no padding) SHA-256 of the public key. `KeyIDFor(pub)` returns `ed25519:` followed by the first 16 characters after `SHA256:`.
- `type Document struct { Protocol string; Domain string; Keys []DocKey }` and `type DocKey struct { KeyID, Alg, PublicKey, Fingerprint string }`, with JSON names as in the spec (`key_id`, `alg`, `public_key`, `fingerprint`).
  - `func (id *Identity) Document() Document`
- `func (s *Service) PublicRoutes(r chi.Router)` mounts `GET /.well-known/conclave`. Task 2 adds `/fed/v1/*` to the same method. `Service` is the package's main type; it holds the DB, identity, config and peers, and grows in later tasks.
- Config fields:
  - `FederationDomain string`, from `CONCLAVE_FEDERATION_DOMAIN` and `--federation-domain`;
  - `FederationHost string` (default `127.0.0.1`);
  - `FederationPort int` (default 7433);
  - `FederationMode string` (`allowlist` or `open`, default `allowlist`);
  - `FederationInsecureHTTP bool` (`CONCLAVE_FEDERATION_INSECURE_HTTP=1`).
  - `validate()`: the domain must pass `NormalizeDomain`, the mode must be valid, and the federation host and port must differ from the main ones.
- Server: when the domain is set, `New` builds a second `http.Server` on `FederationHost:FederationPort` whose router has only `PublicRoutes`, plus the request ID and recovery middleware. `Start` runs both servers and shuts both down. Log a warn line at start when insecure HTTP is on.

**Migration 0011:**
```sql
CREATE TABLE federation_keys (
  key_id TEXT PRIMARY KEY,
  public_key BLOB NOT NULL,
  private_key BLOB NOT NULL,
  created_at TEXT NOT NULL,
  retired_at TEXT
);
CREATE TABLE federation_peers (
  domain TEXT PRIMARY KEY,
  status TEXT NOT NULL CHECK (status IN ('allowed','blocked','seen')),
  fingerprint TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  added_at TEXT NOT NULL,
  last_seen_at TEXT
);
CREATE TABLE federation_nonces (
  origin TEXT NOT NULL,
  nonce TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  PRIMARY KEY (origin, nonce)
);
CREATE INDEX federation_nonces_expires ON federation_nonces(expires_at);
```
Use the timestamp format the other migrations use (see `0007_fixed_width_ts.sql`).

**Tests:**
- `NormalizeDomain`: a table of accepted and refused inputs (uppercase, a port, IDNA `xn--` allowed as ASCII, empty labels, `..`, a leading hyphen, ports 0 and 65536, a 254-character domain).
- An identity is created once and stays stable after a reload.
- The fingerprint and key ID have the right format.
- The document's JSON shape matches the spec exactly.
- Config validation for each new field.
- A server test: with the domain set, `GET /.well-known/conclave` on the federation listener answers 200 while `/v1/whoami` and `/ui/` there answer 404. Without the domain no second listener exists.

- [ ] Write the failing tests
- [ ] Implement
- [ ] Run the full suite and gofmt in Docker
- [ ] Commit `feat(federation): server identity, config and federation listener`

---

### Task 2: Signatures, peers, inbound verification, ping

**Files:**
- Create: `internal/federation/sig.go` and `sig_test.go`
- Create: `internal/federation/peers.go` and `peers_test.go`
- Create: `internal/federation/inbound.go` and `inbound_test.go`
- Create: `internal/federation/fetch.go` and `fetch_test.go`

**Interfaces:**
- `type SigParams struct { Origin, KeyID, Nonce string; TS int64; Sig []byte }`
  - `func ParseSigHeader(h string) (SigParams, error)`: the scheme is exactly `Conclave-Sig-v1`. The parameters are `k="v"` pairs separated by commas, each of the five exactly once, each value at most 512 bytes, with no unknown parameters and no escapes inside quotes.
  - `func SigningString(method, pathQuery, destination, origin string, ts int64, nonce string, body []byte) []byte`: lines joined by `\n`, starting with `conclave-sig-v1`.
  - `func (id *Identity) SignRequest(req *http.Request, destination string, body []byte) error` sets the header. `pathQuery` is `req.URL.RequestURI()`.
- `type Peer struct { Domain, Status, Fingerprint, Note string; AddedAt time.Time; LastSeenAt *time.Time }`
- `type Peers struct{ db *sql.DB }` with these methods:
  - `Get`;
  - `List`;
  - `Allow(domain, fingerprint, note)`: an upsert to `allowed`, which also clears `blocked`;
  - `Block(domain)`: an upsert;
  - `Remove(domain)`;
  - `RecordSeen(domain, fingerprint)`: inserts only if absent;
  - `Touch(domain)`.
- Fetch client:
  - `func NewFetcher(insecureHTTP bool, isAllowed func(domain string) bool) *Fetcher`
  - `func (f *Fetcher) Document(ctx, domain string) (Document, error)`:
    - fetches `GET <scheme>://<domain>/.well-known/conclave`;
    - 5 s timeout, no redirects (`CheckRedirect` returns `http.ErrUseLastResponse`, and a 3xx is an error);
    - body capped at 64 KiB;
    - the document's `domain` must equal the requested domain, and its protocol must be `conclave-federation/1`;
    - each key's fingerprint is recomputed from `public_key`, and a mismatch is an error.
  - The dialer `Control` hook refuses loopback, RFC 1918, RFC 4193 (fc00::/7), link-local (169.254/16, fe80::/10), CGNAT (100.64/10), multicast and unspecified addresses unless `isAllowed(domain)`.
  - The `Fetcher` also takes a `rate.Limiter` with 10 per minute and burst 10 for fetches of domains that are not allowed. Over the limit it returns `ErrFetchLimited`.
- Inbound middleware: `func (s *Service) Verify(next http.Handler) http.Handler` runs the spec's ten verification steps in order. Errors use the existing JSON envelope with these codes:
  - 401 `bad_signature` for a malformed header, a wrong key, a failed signature or a replay;
  - 401 `clock_skew` for a timestamp outside the window;
  - 403 `peer_not_allowed` for a blocked peer or a peer not on the allowlist;
  - 421 `wrong_destination`;
  - 413 `too_large`;
  - 429 `rate_limited` with `Retry-After`;
  - 502 `peer_unreachable` when an open-mode document fetch fails.

  On success it puts the verified origin into the context (`federation.OriginFrom(ctx) string`) and restores the body for the handler.
- On a key mismatch for a known peer, write the `federation.key_changed` audit entry (actor key id 0; target type `peer`; target id the domain; detail `{"presented":"<key id>","expected":"<fingerprint>"}`) and refuse.
- Open mode: on a successful first contact, call `RecordSeen` and write the `federation.peer.seen` audit entry.
- Refusals are logged at warn level, at most one line per origin per minute.
- Nonces are stored with expiry `ts + 300 s`. A goroutine started by the service deletes expired nonces every minute and stops with the context.
- `POST /fed/v1/ping` (behind `Verify`) answers `{"domain":"<our domain>"}`. Its body must be an empty JSON object or empty.
- Outbound:
  - `func (s *Service) Ping(ctx, domain string) (rtt time.Duration, err error)` signs and posts to the peer's `/fed/v1/ping`;
  - the client is the `Fetcher`'s client, with the same dialer rules;
  - the response domain must match.

**Tests:**
- Sign and verify round trip.
- Flipping each signed field (method, path, query, destination, origin, ts, nonce, body) fails.
- Header parse table: a missing, duplicate or unknown parameter, an oversized value, a wrong scheme, a bad base64.
- One test per verification step through `httptest`, each asserting the status and code.
- A replay of an identical request gives 401.
- Allowlist: an unknown origin gives 403 and no fetch happens. Open mode: an unknown origin is fetched, then verified, recorded as `seen` and audited. A key change for a pinned peer is refused and audited.
- Blocked wins in both modes.
- Fetcher:
  - a private-address server (httptest on 127.0.0.1) is refused for an unknown domain and accepted when `isAllowed` returns true;
  - a 302 is refused;
  - a 65 KiB body is refused;
  - a domain mismatch and a fingerprint mismatch are refused;
  - the fetch limiter returns `ErrFetchLimited`.
- Nonce pruning removes expired rows.
- The rate limit gives 429 after the burst.

- [ ] Write the failing tests
- [ ] Implement
- [ ] Run the full suite and gofmt in Docker
- [ ] Commit `feat(federation): signed requests, peer policy and ping`

---

### Task 3: Admin API and CLI

**Files:**
- Create: `internal/federation/admin.go` and `admin_test.go`
- Modify: `internal/server/server.go` (mount `AdminRoutes` inside `/v1`, after auth)
- Create: `cmd/federation/federation.go` and `federation_test.go`
- Modify: `cmd/conclave-mcp/main.go` (register the command)

**Interfaces:**
- `func (s *Service) AdminRoutes(r chi.Router)` mounts the spec's seven endpoints, all admin only (403 `forbidden` otherwise). When the server has federation off, mount a stub that answers 404 `federation_disabled` on `/v1/federation` and below.
- `GET /v1/federation` returns `{"domain","mode","fingerprint","key_id","peers":{"allowed":n,"blocked":n,"seen":n}}`.
- `GET /v1/federation/peers` returns `{"peers":[{"domain","status","fingerprint","note","added_at","last_seen_at"}]}`.
- `GET /v1/federation/lookup?domain=` returns `{"domain","fingerprint","key_id"}` from `Fetcher.Document`. An admin lookup counts as allowed for the dialer, because the operator chose the host, but the fetch limiter still applies.
- `POST /v1/federation/peers` takes `{"domain","fingerprint","note"}`. The fingerprint is required. The handler fetches the document; if no key has that fingerprint it answers 409 `fingerprint_mismatch`. It refuses our own domain with 400. On success it calls `Allow`, writes the `federation.peer.add` audit entry, and answers 201 with the peer.
- `POST /v1/federation/peers/{domain}/block` returns 200 with the peer and writes `federation.peer.block`.
- `DELETE /v1/federation/peers/{domain}` returns 204, writes `federation.peer.remove`, and returns 404 when the domain is unknown.
- `POST /v1/federation/peers/{domain}/ping` returns `{"domain","rtt_ms"}`, or 502 `peer_unreachable` with the error text.
- CLI `conclave-mcp federation`:
  - `status`;
  - `peers list` (table and `--format json`, like `keys list`);
  - `peers add <domain>`:
    - without `--fingerprint`, it calls lookup and prints `fingerprint of <domain>: SHA256:…` and then `compare it with the peer's operator, then run: conclave-mcp federation peers add <domain> --fingerprint SHA256:…`;
    - with the fingerprint, it adds the peer; `--note` is optional;
  - `peers block <domain>`;
  - `peers remove <domain>`;
  - `peers ping <domain>`.

**Tests:**
- Admin API: a non-admin key gets 403; with federation off, each route gives 404; adding with a wrong fingerprint gives 409 and adds nothing; adding our own domain gives 400; block, remove and their audit rows; ping to an httptest peer.
- CLI: each subcommand against an `httptest` server, like `cmd/keys` tests, including the two-step `add` output.

- [ ] Write the failing tests
- [ ] Implement
- [ ] Run the full suite and gofmt in Docker
- [ ] Commit `feat(federation): admin API and federation CLI`

---

### Task 4: e2e and docs

**Files:**
- Create: `tests/e2e/federation_test.go`
- Create: `docs/federation.md`
- Modify: `docs/architecture.md` (package layout, data model, configuration, deployment topology, HTTP API table)
- Modify: `docs/runbook.md` (reverse proxy for the federation listener)
- Modify: `docs/cli-reference.md`
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Added`)
- Modify: `packaging/reverse-proxy/README.md`, if it has config examples: add the federation location blocks

**e2e:**
- Start servers A, B and C in process on free ports, using the existing e2e helpers to start a server with config. Domains are `127.0.0.1:<fedport>`, with insecure HTTP on.
- A and B are in allowlist mode. A looks up B, adds B with B's fingerprint, and B does the same for A. Ping A→B and B→A both succeed.
- C pings A: C is not allowed on A, so it gets 403 `peer_not_allowed`.
- Open mode is not tested here: every e2e server listens on 127.0.0.1, and the fetch client refuses loopback for origins that are not allowed (as the spec says). The in-package tests in `internal/federation` cover open mode with a test dialer.
- A blocks B; B pings A and gets 403.

**docs/federation.md:** state that the reverse proxy must forward the `Host` header and the request URI unchanged (the signature covers both). Then: what federation does in 0.8, turning it on (env vars), the reverse proxy, exchanging fingerprints, the modes, the signature format (exact signing string), the verification order, error codes, audit actions, and the limits. It is the start of the protocol document that 0.14.0 freezes.

- [ ] Write the e2e test and see it pass
- [ ] Write the docs
- [ ] Run the full suite and gofmt in Docker
- [ ] Commit `test(federation): two-server e2e; docs: federation`

---

### Task 5: Web UI federation page

**Files:**
- Modify: `internal/ui/assets/app.js` (navigation entry, shown only when `GET /v1/federation` does not answer 404)
- Create: `internal/ui/assets/federation.js`
- Modify: `internal/ui/assets/app.css` if needed
- Modify: the UI e2e or asset tests that list pages
- Modify: `docs/web-ui.md`

**Behaviour:**
- The page shows our domain, mode, key ID and fingerprint in a monospace block with a copy button.
- The peers table has columns domain, status, fingerprint, note and last contact, with Block, Remove and Ping per row. Ping shows the RTT or the error.
- Adding a peer takes two steps:
  1. The operator enters a domain and clicks "Look up". The page shows the fetched fingerprint and the text "Compare this fingerprint with the one the peer's operator gives you. Add only if they match."
  2. The operator clicks "Add peer", which sends the fingerprint the lookup returned.
- Management actions (add, block, remove) use the same confirmation mechanism that the other management pages use at the time this task runs. If the passkey confirmation for management actions (branch `feat/confirmed-changes`) is on main by then, rebase onto it and use it.
- Use the existing helpers in `lib.js` (`api`, `h`, `action`, `rerender`). No new libraries.

**Tests:**
- The JS syntax check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- Any existing asset or route test extended for the new file.
- A browser smoke test against a locally built server with federation on.

- [ ] Implement
- [ ] JS check, full suite, gofmt
- [ ] Browser smoke test
- [ ] Commit `feat(ui): federation page`
