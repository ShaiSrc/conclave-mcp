# Web admin UI, part 1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator manages Conclave from a browser on the server's machine (rooms, members and rights, keys, audit, the agents' messages live) and logs in with a passkey; no password exists.

**Architecture:** A new package `internal/ui` holds everything server side: the migration 0009 tables behind a small `store` (`sessions.go`), the WebAuthn ceremonies and the registration gate on `github.com/go-webauthn/webauthn` (`auth.go`), the cookie path into `/v1` (`v1.go`: `Auth` wraps the bearer middleware and gives the request the claims of the admin key labelled `operator`), and the embedded pages behind one guard for Host, Origin and the security headers (`ui.go`). `internal/server` mounts it only on a loopback listen address. The frontend is four plain ES modules (`lib.js`, `app.js`, `rooms.js`, `keys.js`) that call the existing `/v1` endpoints with `X-Conclave-UI: 1`, so no permission rule is written twice. The CLI gets `ui setup` (online) and `admin ui-reset` (offline, like `create-bootstrap`).

**Tech Stack:** Go 1.25, chi, modernc SQLite, cobra, `github.com/go-webauthn/webauthn` v0.17.4; test-only `github.com/descope/virtualwebauthn` v1.0.5; vanilla JS (ES modules, no framework, no build step), embedded with `embed.FS`. Spec: `docs/superpowers/specs/2026-11-12-web-admin-ui-design.md`.

## Global Constraints

- Work on the branch `feat/web-ui`. Go is not installed locally. Every go command runs in Docker, from the repository root, exactly:
  `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  (narrow it with package paths and `-run`), and `go vet ./...` the same way.
- The suite (`go test ./...`, `go vet ./...`) is green at the end of every task, and `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` prints nothing. Before every commit run `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -w <the Go files you changed>`.
- JavaScript has no test runner. From Task 5 on, every task that touches `internal/ui/assets/*.js` runs the syntax check
  `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
  → `ok`, and `TestAssets_NoMarkupFromData` (Task 4) guards the files. The browser smoke test is in Task 10.
- Commit with the repository's existing git config. Every commit message ends with the trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` (second `-m`).
- Nothing personal in the repository: no names, machine paths, install-specific labels, other project names, no "the user said". Use "operator", "admin key", neutral example labels (`agent-a`, `partner`).
- YAGNI. Exactly two new dependencies: `github.com/go-webauthn/webauthn` v0.17.4 (the newest that builds with Go 1.25; v0.18 needs Go 1.26) and, imported only from `_test.go` files, `github.com/descope/virtualwebauthn` v1.0.5 (its `go.mod` says `go 1.25.0`; verified to build and run with Go 1.25 against webauthn v0.17.4). Task 2 adds both with `go get` on package paths, which keeps `go.mod` in its present style (every requirement `// indirect`); do not run `go mod tidy`.
- Exact strings (from the spec, do not paraphrase):
  - cookie `conclave_ui`: 32 random bytes base64url, `HttpOnly`, `SameSite=Strict`, `Path=/`, no `Secure`, no `Max-Age`; the database stores only its hex SHA-256
  - header `X-Conclave-UI: 1`; admin key label `operator` (`is_admin = 1`, `trust_origin = internal`); `meta` key `ui_user_handle`
  - audit actions `ui.setup_token`, `ui.passkey.add`, `ui.passkey.remove`, `ui.login`, `ui.logout`, `ui.reset`
  - timeouts: setup token 10 min, ceremony 5 min, session idle 1 h, session absolute 12 h
  - RP ID `localhost`, RP name `Conclave`, origin `http://localhost:<port>`; accepted `Host` values `localhost:<port>` and `127.0.0.1:<port>`; setup URL `http://localhost:<port>/ui/#setup=<token>`
  - headers on every `/ui` response: `Content-Security-Policy: default-src 'self'; base-uri 'none'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`
  - external fence lines `--- external content from <label> ---` and `--- end of external content ---` (as the CLI)
- Errors use the `/v1` envelope `{"error": {"code", "message", "request_id"}}`. The UI's status codes: missing `X-Conclave-UI` with the cookie `401 unauthenticated`; wrong `Host` or `Origin` `403 forbidden` (`host not allowed` / `origin not allowed`); dead session `401 unauthenticated`; registration refused `403 forbidden`; non-admin on `POST /v1/ui/setup-tokens` `403 forbidden` `admin key required`; setup token once a passkey exists `409 conflict`; last passkey `409 conflict`.
- The scripts put server data into the page only through `h()` (text nodes, attribute values) and `textContent`; never `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval`. No inline script, style or `on…=` attribute.
- The UI exists only when `ui.Loopback(cfg.Host)`; otherwise `/ui` is 404 and the cookie counts for nothing.
- Verified library API (read from the module source in the Docker module cache, v0.17.4): `webauthn.New(*webauthn.Config)`; `Config{RPID, RPDisplayName, RPOrigins, AttestationPreference, AuthenticatorSelection, Timeouts}`; `webauthn.User` = `WebAuthnID() []byte`, `WebAuthnName() string`, `WebAuthnDisplayName() string`, `WebAuthnCredentials() []webauthn.Credential`; `(*WebAuthn).BeginRegistration(user, ...RegistrationOption) (*protocol.CredentialCreation, *SessionData, error)` with `webauthn.WithExclusions`; `FinishRegistration(user, SessionData, *http.Request) (*Credential, error)`; `BeginDiscoverableLogin(...LoginOption) (*protocol.CredentialAssertion, *SessionData, error)` with `webauthn.WithUserVerification(protocol.VerificationRequired)`; `FinishPasskeyLogin(handler func(rawID, userHandle []byte) (User, error), SessionData, *http.Request) (User, *Credential, error)`. The browser gets `creation.Response` / `assertion.Response` (the `publicKey` member), which `PublicKeyCredential.parseCreationOptionsFromJSON` / `parseRequestOptionsFromJSON` read. `Credential.Flags` must be stored: a login whose backup-eligible flag differs from the stored one fails with "Backup Eligible flag inconsistency" (checked: a synced passkey has BE=1).
- Decisions this plan adds to the spec (reviewers: these are intended):
  - `ui_credentials` gets a `flags INTEGER NOT NULL DEFAULT 0` column (the authenticator flags byte; see the previous point).
  - `/v1/mcp` keeps bearer auth only: its proxy forwards the `Authorization` header to its own calls, so a cookie could not reach anything. The UI does not use it.
  - The ceremony cookie is `conclave_ui_ceremony` (`Path=/ui/auth/`, 5 minutes); a passkey's name comes from the `name` field of `register/begin` (default `passkey`).
  - `POST /v1/ui/setup-tokens` answers `409` once a passkey exists (the token could not be used).
  - `admin ui-reset` also renames a revoked `operator` key to `operator-revoked-<id>`: the label is `UNIQUE`, so otherwise the next setup could not create the operator key again.
  - Every non-GET `/ui` request needs the UI `Origin` too (not only `/v1`), bodies under `/ui` are capped at 64 KiB, and the CSP adds `base-uri 'none'`.
  - A login whose sign counter went backwards (`CloneWarning`) is refused with `403`.
  - `keys.js` shows no Revoke button for the key labelled `operator`: revoking it locks the UI out (the API still allows it; `docs/web-ui.md` says how to recover).

## File map

| File | Task | Responsibility |
|---|---|---|
| `migrations/0009_web_ui.sql` | 1 | `ui_credentials` (+ `flags`), `ui_sessions`, `ui_setup_tokens` |
| `internal/ui/ui.go` | 1, 2, 3, 4 | names and timeouts; `Service`, `New`; `Loopback`, `uiHost`; embedded assets, `Routes`, `guard` |
| `internal/ui/sessions.go` (+ `sessions_test.go`) | 1 | `store`: tokens, sessions (sliding idle expiry), setup tokens, purge |
| `internal/ui/auth.go` (+ `auth_test.go`, `helpers_test.go`) | 2 | ceremonies, registration gate, operator key, passkey list/remove, state, logout |
| `internal/ui/v1.go` (+ `v1_test.go`) | 3 | `Auth` (cookie on `/v1`), `V1Routes` (`POST /v1/ui/setup-tokens`) |
| `internal/server/server.go`, `internal/server/ui_test.go` | 3 | mount the UI on a loopback address |
| `internal/ui/ui_test.go`, `internal/ui/assets/index.html`, `app.css` | 4 | pages, headers, Host/Origin checks, asset guard |
| `internal/ui/assets/lib.js`, `app.js` | 5, 6, 7 | helpers; router, header, login, setup, passkeys |
| `internal/ui/assets/rooms.js` | 6 | rooms list, room page (messages live, requests, claims, members) |
| `internal/ui/assets/keys.js` | 7 | keys (create, create agent, revoke), audit |
| `cmd/ui/ui.go` (+ test), `cmd/admin/admin.go` (+ test), `cmd/conclave-mcp/main.go` | 8 | `ui setup`, `admin ui-reset` |
| `docs/web-ui.md` (new), `README.md`, `docs/cli-reference.md`, `docs/architecture.md`, `docs/DOCS-PLAN.md`, `CHANGELOG.md` | 9 | docs |
| `tests/e2e/ui_test.go` | 10 | the real binary end to end; browser smoke test (not committed) |

---

### Task 1: Migration 0009 and the session and setup-token store

**Files:**
- Create: `migrations/0009_web_ui.sql`, `internal/ui/ui.go`, `internal/ui/sessions.go`, `internal/ui/sessions_test.go`

**Interfaces:**
- Produces: tables `ui_credentials(id TEXT PK, public_key BLOB, sign_count INTEGER, transports TEXT, flags INTEGER, name TEXT, created_at TEXT, last_used_at TEXT NULL)`, `ui_sessions(token_hash PK, created_at, last_seen_at, expires_at)`, `ui_setup_tokens(token_hash PK, expires_at, used_at NULL)`; timestamps are `storage.FormatTS` strings, compared as strings in SQL.
- Produces in package `ui`: `const CookieName = "conclave_ui"`, `HeaderName = "X-Conclave-UI"`, `OperatorLabel = "operator"`, `SetupTokenTTL = 10 * time.Minute`, `CeremonyTTL = 5 * time.Minute`, `IdleTimeout = time.Hour`, `MaxSession = 12 * time.Hour`.
- Produces: `newToken() (token, hash string, err error)` (32 random bytes, base64url; hash = `hashToken(token)`), `hashToken(token string) string` (hex SHA-256).
- Produces: `type store struct { db *sql.DB; now func() time.Time }` with `createSession(ctx) (string, error)` (purges first), `session(ctx, token string) (bool, error)` (live = younger than `MaxSession` and used within `IdleTimeout`; a live one gets `last_seen_at = now`), `deleteSession(ctx, token) error`, `purge(ctx) error`, `createSetupToken(ctx) (token string, expires time.Time, err error)`, `setupTokenValid(ctx, token) (bool, error)`, `useSetupToken(ctx, token) (bool, error)` (marks used; false if not valid).
- Produces (test helpers in `sessions_test.go`, used by later test files of the package): `testDB(t) *sql.DB` (migrated), `type clock struct{ t time.Time }` with `now()` and `advance(d)`, `newTestStore(t) (*store, *clock)`.

- [ ] **Step 1: Write the failing tests**

`internal/ui/sessions_test.go`:

```go
package ui

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

// testDB is a fresh, fully migrated database.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	return db
}

// clock is a settable time source for store.now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore(t *testing.T) (*store, *clock) {
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	return &store{db: testDB(t), now: c.now}, c
}

func TestSessions_IdleAndAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, err := s.createSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM ui_sessions`).Scan(&stored); err != nil || stored != hashToken(tok) || stored == tok {
		t.Fatalf("stored %q (%v): want the SHA-256 of the token, not the token", stored, err)
	}
	for i := 0; i < 12; i++ { // used every 59 minutes: never idle, 11 h 48 min in total
		c.advance(59 * time.Minute)
		if ok, err := s.session(ctx, tok); err != nil || !ok {
			t.Fatalf("after %d uses: session = %v, %v; want live", i+1, ok, err)
		}
	}
	c.advance(59 * time.Minute) // 12 h 47 min after login, 59 minutes idle
	if ok, _ := s.session(ctx, tok); ok {
		t.Error("session still live 12 h after login")
	}

	idle, _ := s.createSession(ctx)
	c.advance(61 * time.Minute)
	if ok, _ := s.session(ctx, idle); ok {
		t.Error("session still live after 61 idle minutes")
	}
	if ok, _ := s.session(ctx, "not-a-token"); ok {
		t.Error("unknown token accepted")
	}
}

func TestSessions_DeleteAndPurge(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, _ := s.createSession(ctx)
	if err := s.deleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.session(ctx, tok); ok {
		t.Error("session live after delete")
	}
	_, _ = s.createSession(ctx)
	c.advance(2 * time.Hour)
	_, _ = s.createSession(ctx) // purges the idle one
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ui_sessions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("sessions after purge: %d (%v), want 1", n, err)
	}
}

func TestSetupTokens_SingleUseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, expires, err := s.createSetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := c.t.Add(10 * time.Minute); !expires.Equal(want) {
		t.Errorf("expires %v, want %v", expires, want)
	}
	if ok, _ := s.setupTokenValid(ctx, tok); !ok {
		t.Fatal("fresh token not valid")
	}
	if ok, _ := s.useSetupToken(ctx, tok); !ok {
		t.Fatal("first use refused")
	}
	if ok, _ := s.useSetupToken(ctx, tok); ok {
		t.Error("second use accepted")
	}
	if ok, _ := s.setupTokenValid(ctx, tok); ok {
		t.Error("used token still valid")
	}

	late, _, _ := s.createSetupToken(ctx)
	c.advance(10*time.Minute + time.Second)
	if ok, _ := s.setupTokenValid(ctx, late); ok {
		t.Error("token valid after 10 minutes")
	}
	if ok, _ := s.useSetupToken(ctx, late); ok {
		t.Error("expired token used")
	}
	if ok, _ := s.setupTokenValid(ctx, "unknown"); ok {
		t.Error("unknown token valid")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/`
Expected: FAIL, build failed: `undefined: store`, `undefined: hashToken`.

- [ ] **Step 3: Write the migration**

`migrations/0009_web_ui.sql`:

```sql
-- Migration 0009: web admin UI (passkey login).
--
-- ui_credentials holds the operator's passkeys: public keys only, no
-- secret. flags is the authenticator-data flags byte from registration;
-- the WebAuthn library refuses a login whose backup-eligible flag differs
-- from the stored one. ui_sessions and ui_setup_tokens store only the
-- SHA-256 of their tokens, so a read of the database yields nothing
-- usable.

CREATE TABLE ui_credentials (
    id            TEXT PRIMARY KEY,      -- base64url credential id
    public_key    BLOB NOT NULL,
    sign_count    INTEGER NOT NULL DEFAULT 0,
    transports    TEXT NOT NULL DEFAULT '',
    flags         INTEGER NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    last_used_at  TEXT
);

CREATE TABLE ui_sessions (
    token_hash    TEXT PRIMARY KEY,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

CREATE TABLE ui_setup_tokens (
    token_hash    TEXT PRIMARY KEY,
    expires_at    TEXT NOT NULL,
    used_at       TEXT
);
```

- [ ] **Step 4: Write the package**

`internal/ui/ui.go`:

```go
// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import "time"

// Names the browser, the server and the CLI agree on.
const (
	CookieName    = "conclave_ui"   // the session cookie
	HeaderName    = "X-Conclave-UI" // /v1 takes the cookie only with this header set to 1
	OperatorLabel = "operator"      // the admin key UI sessions act as
)

// Timeouts.
const (
	SetupTokenTTL = 10 * time.Minute // a setup token from ui setup
	CeremonyTTL   = 5 * time.Minute  // between begin and finish of a passkey ceremony
	IdleTimeout   = time.Hour        // a session unused this long ends
	MaxSession    = 12 * time.Hour   // a session ends this long after login
)
```

`internal/ui/sessions.go`:

```go
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// newToken returns 32 random bytes as base64url and the form stored in
// the database (hashToken).
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// hashToken is the stored form of a token: hex SHA-256. Tokens are 256
// random bits, so a plain hash is enough (no salt, no argon2id).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// store reads and writes the ui_* tables (migration 0009).
type store struct {
	db  *sql.DB
	now func() time.Time // tests move the clock
}

// createSession starts a session and returns its token (the cookie
// value). It also purges what has expired.
func (s *store) createSession(ctx context.Context) (string, error) {
	if err := s.purge(ctx); err != nil {
		return "", err
	}
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, storage.FormatTS(now), storage.FormatTS(now), storage.FormatTS(now.Add(MaxSession)))
	return token, err
}

// session reports whether token belongs to a live session (younger than
// MaxSession, used within IdleTimeout) and, if so, marks it used now.
func (s *store) session(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE ui_sessions SET last_seen_at = ?
		 WHERE token_hash = ? AND expires_at > ? AND last_seen_at > ?`,
		storage.FormatTS(now), hashToken(token), storage.FormatTS(now), storage.FormatTS(now.Add(-IdleTimeout)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// deleteSession ends the session of token (logout).
func (s *store) deleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// purge deletes expired sessions and setup tokens.
func (s *store) purge(ctx context.Context) error {
	now := s.now()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM ui_sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		storage.FormatTS(now), storage.FormatTS(now.Add(-IdleTimeout))); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_setup_tokens WHERE expires_at <= ?`, storage.FormatTS(now))
	return err
}

// createSetupToken returns a new setup token valid SetupTokenTTL.
func (s *store) createSetupToken(ctx context.Context) (string, time.Time, error) {
	if err := s.purge(ctx); err != nil {
		return "", time.Time{}, err
	}
	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(SetupTokenTTL)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ui_setup_tokens (token_hash, expires_at) VALUES (?, ?)`, hash, storage.FormatTS(expires))
	return token, expires, err
}

// setupTokenValid reports whether token is unused and unexpired.
func (s *store) setupTokenValid(ctx context.Context, token string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM ui_setup_tokens WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		hashToken(token), storage.FormatTS(s.now())).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// useSetupToken marks token used; false if it was not valid.
func (s *store) useSetupToken(ctx context.Context, token string) (bool, error) {
	now := storage.FormatTS(s.now())
	res, err := s.db.ExecContext(ctx,
		`UPDATE ui_setup_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, hashToken(token), now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ ./internal/storage/`
Expected: `ok` for both.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 6: Commit**

```bash
git add migrations/0009_web_ui.sql internal/ui
git commit -m "feat(ui): migration 0009 and the session and setup-token store" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Passkey ceremonies, operator key, registration gate

**Files:**
- Modify: `go.mod`, `go.sum` (two dependencies), `internal/ui/ui.go` (whole file below)
- Create: `internal/ui/auth.go`, `internal/ui/helpers_test.go`, `internal/ui/auth_test.go`

**Interfaces:**
- Consumes: Task 1 (`store`, `newToken`, `hashToken`, the constants, `testDB`, `clock`).
- Produces: `type Service struct { store; wa *webauthn.WebAuthn; origin string; mu sync.Mutex; ceremonies map[string]ceremony }`, `New(db *sql.DB, port int) (*Service, error)` (origin `http://localhost:<port>`), `(*Service).Routes(r chi.Router)`.
- Produces endpoints (Task 4 puts them behind `guard`; Task 5 calls them):

| Endpoint | Body | Answer |
|---|---|---|
| `POST /ui/auth/register/begin` | `{"setup_token": "...", "name": "..."}` (both optional) | `200` creation options (the `publicKey` member) + cookie `conclave_ui_ceremony`; `403 forbidden` when not logged in and a passkey exists, or the setup token is invalid, used or expired; `400` bad JSON or name over 64 characters |
| `POST /ui/auth/register/finish` | `credential.toJSON()` | `201 {"id", "name"}`; with a setup token it also creates the `operator` key and sets `conclave_ui`; `400` no ceremony / older than 5 min / passkey not accepted; `401` session ended; `403` token used meanwhile; `409` label `operator` unusable, or another passkey was registered meanwhile |
| `POST /ui/auth/login/begin` | none | `200` request options + ceremony cookie |
| `POST /ui/auth/login/finish` | `credential.toJSON()` | `204` + `conclave_ui`; `400` no ceremony; `401` passkey not accepted; `403` clone warning or operator key revoked |
| `POST /ui/auth/logout` | none | `204`, deletes the session, clears the cookie |
| `GET /ui/auth/state` | | `{"setup_needed": bool, "logged_in": bool}` |
| `GET /ui/auth/credentials` | | `{"credentials": [{"id", "name", "created_at", "last_used_at"?}]}`; `401` without a session |
| `DELETE /ui/auth/credentials/{id}` | | `204`; `409` the last one; `404`; `401` |

- Produces (auth.go, used by Task 3): `operatorKeyID(ctx, db *sql.DB) (int64, error)` (the active admin key labelled `operator`, `sql.ErrNoRows` if none), `ensureOperatorKey(ctx, db, now time.Time) (int64, error)`, `errOperatorUnusable`, `credID(id []byte) string`, `(*store).credentialCount(ctx) (int, error)`, `(*Service).loggedIn(r) (bool, error)`, `writeJSON(w, status, body)`, `writeErr(w, r, status, code, msg)`.
- Produces (test helpers in `helpers_test.go`): `testHost = "localhost:7432"`, `testOrigin = "http://localhost:7432"`, `newTestUI(t, extra ...func(*Service, chi.Router)) (*Service, *clock, string)`, `type browser` with `newBrowser(t, base)`, fields `host`, `origin`, `cookies`, methods `do(method, path string, body any) (int, []byte)` and `send(method, path string, body any, hdr map[string]string) *http.Response` (`""` deletes a header), `type passkey` with `newPasskey(origin)`, `beginRegistration`, `finishRegistration`, `register(b, setupToken) (int, []byte)`, `login(b) (int, []byte)`, `loggedIn(t, s, base) (*browser, *passkey)`, `auditActions(t, db) []string`, `hasAll(got []string, want ...string) bool`.

- [ ] **Step 1: Add the dependencies**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go get github.com/go-webauthn/webauthn/webauthn@v0.17.4 github.com/go-webauthn/webauthn/protocol@v0.17.4 github.com/descope/virtualwebauthn@v1.0.5`
Expected: `go: added github.com/descope/virtualwebauthn v1.0.5`, `go: added github.com/go-webauthn/webauthn v0.17.4` and their dependencies (`fxamacker/cbor/v2`, `go-viper/mapstructure/v2`, `go-webauthn/x`, `golang-jwt/jwt/v5`, `google/go-tpm`, `philhofer/fwd`, `tinylib/msgp`, `x448/float16`), all `// indirect` in `go.mod`. Use the package paths as written: `go get` on the bare module path does not record the checksums the build needs.

- [ ] **Step 2: Write the failing tests**

`internal/ui/helpers_test.go`:

```go
package ui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/go-chi/chi/v5"
)

// The tests run the UI as if the server listened on port 7432.
const (
	testHost   = "localhost:7432"
	testOrigin = "http://localhost:7432"
)

// newTestUI serves the UI's routes on a test server and returns the
// service, its clock and the server URL. extra mounts more routes.
func newTestUI(t *testing.T, extra ...func(s *Service, r chi.Router)) (*Service, *clock, string) {
	t.Helper()
	s, err := New(testDB(t), 7432)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	s.now = c.now
	r := chi.NewRouter()
	s.Routes(r)
	for _, f := range extra {
		f(s, r)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return s, c, srv.URL
}

// browser is a test client that acts like the UI page: it sends the UI's
// Host, its Origin on writes and the X-Conclave-UI header, and keeps cookies.
type browser struct {
	t       *testing.T
	base    string
	host    string
	origin  string
	cookies map[string]*http.Cookie
}

func newBrowser(t *testing.T, base string) *browser {
	return &browser{t: t, base: base, host: testHost, origin: testOrigin, cookies: map[string]*http.Cookie{}}
}

// noRedirect returns redirects as responses.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request; body is sent as is when it is a string, else as JSON.
func (b *browser) do(method, path string, body any) (int, []byte) {
	b.t.Helper()
	resp := b.send(method, path, body, nil)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// send is do with extra headers ("" deletes one) and the whole response.
func (b *browser) send(method, path string, body any, hdr map[string]string) *http.Response {
	b.t.Helper()
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		j, err := json.Marshal(v)
		if err != nil {
			b.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Host = b.host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderName, "1")
	if method != http.MethodGet {
		req.Header.Set("Origin", b.origin)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	for _, c := range b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return resp
}

// passkey is a software authenticator with one credential, like a
// synced platform passkey (backup eligible and backed up).
type passkey struct {
	rp   virtualwebauthn.RelyingParty
	auth virtualwebauthn.Authenticator
	cred virtualwebauthn.Credential
}

func newPasskey(origin string) *passkey {
	return &passkey{
		rp:   virtualwebauthn.RelyingParty{ID: "localhost", Name: "Conclave", Origin: origin},
		auth: virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true}),
		cred: virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
	}
}

// beginRegistration returns the creation options, or nil and the error response.
func (p *passkey) beginRegistration(b *browser, setupToken string) (*virtualwebauthn.AttestationOptions, int, []byte) {
	b.t.Helper()
	status, body := b.do("POST", "/ui/auth/register/begin", map[string]string{"setup_token": setupToken, "name": "test key"})
	if status != http.StatusOK {
		return nil, status, body
	}
	opts, err := virtualwebauthn.ParseAttestationOptions(string(body))
	if err != nil {
		b.t.Fatalf("creation options: %v\n%s", err, body)
	}
	return opts, status, body
}

func (p *passkey) finishRegistration(b *browser, opts *virtualwebauthn.AttestationOptions) (int, []byte) {
	b.t.Helper()
	p.auth.Options.UserHandle = []byte(opts.UserID) // a discoverable credential keeps the user handle
	return b.do("POST", "/ui/auth/register/finish", virtualwebauthn.CreateAttestationResponse(p.rp, p.auth, p.cred, *opts))
}

// register runs a registration ceremony and returns the last response.
func (p *passkey) register(b *browser, setupToken string) (int, []byte) {
	b.t.Helper()
	opts, status, body := p.beginRegistration(b, setupToken)
	if opts == nil {
		return status, body
	}
	return p.finishRegistration(b, opts)
}

// login runs a discoverable login and returns the last response.
func (p *passkey) login(b *browser) (int, []byte) {
	b.t.Helper()
	status, body := b.do("POST", "/ui/auth/login/begin", nil)
	if status != http.StatusOK {
		return status, body
	}
	opts, err := virtualwebauthn.ParseAssertionOptions(string(body))
	if err != nil {
		b.t.Fatalf("request options: %v\n%s", err, body)
	}
	return b.do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(p.rp, p.auth, p.cred, *opts))
}

// loggedIn sets the UI up with a setup token and returns the logged-in
// browser and its passkey.
func loggedIn(t *testing.T, s *Service, base string) (*browser, *passkey) {
	t.Helper()
	tok, _, err := s.createSetupToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, pk := newBrowser(t, base), newPasskey(testOrigin)
	if status, body := pk.register(b, tok); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	return b, pk
}

// auditActions lists the audit log's actions, oldest first.
func auditActions(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action FROM audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// hasAll reports whether got contains every one of want.
func hasAll(got []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}
```

`internal/ui/auth_test.go`:

```go
package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
)

type uiState struct {
	SetupNeeded bool `json:"setup_needed"`
	LoggedIn    bool `json:"logged_in"`
}

func state(t *testing.T, b *browser) uiState {
	t.Helper()
	var st uiState
	status, body := b.do("GET", "/ui/auth/state", nil)
	if status != http.StatusOK || json.Unmarshal(body, &st) != nil {
		t.Fatalf("state: %d %s", status, body)
	}
	return st
}

// Setup token -> first passkey (creates the operator key, logs in) ->
// login from a second browser -> logout.
func TestCeremonies_SetupLoginLogout(t *testing.T) {
	s, _, base := newTestUI(t)
	b := newBrowser(t, base)
	if st := state(t, b); !st.SetupNeeded || st.LoggedIn {
		t.Fatalf("fresh state %+v, want setup_needed and not logged_in", st)
	}
	tok, _, err := s.createSetupToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pk := newPasskey(testOrigin)
	if status, body := pk.register(b, tok); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	c := b.cookies[CookieName]
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Fatalf("session cookie %+v: want HttpOnly, SameSite=Strict, Path=/, not Secure", c)
	}
	if st := state(t, b); st.SetupNeeded || !st.LoggedIn {
		t.Errorf("state after setup %+v, want logged in", st)
	}
	var trust string
	var admin int
	if err := s.db.QueryRow(`SELECT trust_origin, is_admin FROM api_keys WHERE label = 'operator' AND revoked_at IS NULL`).Scan(&trust, &admin); err != nil || trust != "internal" || admin != 1 {
		t.Errorf("operator key: trust %q admin %d (%v), want an internal admin key", trust, admin, err)
	}

	b2 := newBrowser(t, base)
	if status, body := pk.login(b2); status != http.StatusNoContent || b2.cookies[CookieName] == nil {
		t.Fatalf("login: %d %s, cookie %v", status, body, b2.cookies[CookieName])
	}
	old := *b2.cookies[CookieName]
	if status, _ := b2.do("POST", "/ui/auth/logout", nil); status != http.StatusNoContent || b2.cookies[CookieName] != nil {
		t.Fatalf("logout: %d, cookie %v left", status, b2.cookies[CookieName])
	}
	b2.cookies[CookieName] = &old // the old cookie is dead on the server too
	if st := state(t, b2); st.LoggedIn {
		t.Error("session still live after logout")
	}
	if got, want := auditActions(t, s.db), []string{"ui.passkey.add", "ui.login", "ui.logout"}; !slices.Equal(got, want) {
		t.Errorf("audit %v, want %v", got, want)
	}
}

func TestRegisterGate(t *testing.T) {
	s, clk, base := newTestUI(t)
	ctx := context.Background()
	if status, _ := newPasskey(testOrigin).register(newBrowser(t, base), "made-up"); status != http.StatusForbidden {
		t.Errorf("unknown setup token: %d, want 403", status)
	}
	expired, _, _ := s.createSetupToken(ctx)
	clk.advance(11 * time.Minute)
	if status, _ := newPasskey(testOrigin).register(newBrowser(t, base), expired); status != http.StatusForbidden {
		t.Errorf("expired setup token: %d, want 403", status)
	}

	// Two browsers start with one token: only the first finish uses it.
	tok, _, _ := s.createSetupToken(ctx)
	b1, b2 := newBrowser(t, base), newBrowser(t, base)
	p1, p2 := newPasskey(testOrigin), newPasskey(testOrigin)
	o1, st1, body1 := p1.beginRegistration(b1, tok)
	o2, st2, body2 := p2.beginRegistration(b2, tok)
	if o1 == nil || o2 == nil {
		t.Fatalf("begin: %d %s / %d %s", st1, body1, st2, body2)
	}
	if status, body := p1.finishRegistration(b1, o1); status != http.StatusCreated {
		t.Fatalf("first finish: %d %s", status, body)
	}
	if status, _ := p2.finishRegistration(b2, o2); status != http.StatusForbidden {
		t.Errorf("second finish with the same token: %d, want 403", status)
	}

	// A passkey exists: a new token no longer registers one.
	tok2, _, _ := s.createSetupToken(ctx)
	if status, body := newPasskey(testOrigin).register(newBrowser(t, base), tok2); status != http.StatusForbidden || !strings.Contains(string(body), "log in to add another") {
		t.Errorf("token after setup: %d %s, want 403", status, body)
	}
	// The logged-in operator adds one without a token.
	if status, body := newPasskey(testOrigin).register(b1, ""); status != http.StatusCreated {
		t.Errorf("second passkey from a session: %d %s", status, body)
	}
	if n, _ := s.credentialCount(ctx); n != 2 {
		t.Errorf("%d passkeys, want 2", n)
	}
}

func TestPasskeys_ListAndRemoveNotTheLast(t *testing.T) {
	s, _, base := newTestUI(t)
	b, pk := loggedIn(t, s, base)
	list := func() []string {
		var l struct {
			Credentials []struct {
				ID string `json:"id"`
			} `json:"credentials"`
		}
		status, body := b.do("GET", "/ui/auth/credentials", nil)
		if status != http.StatusOK || json.Unmarshal(body, &l) != nil {
			t.Fatalf("list: %d %s", status, body)
		}
		var ids []string
		for _, c := range l.Credentials {
			ids = append(ids, c.ID)
		}
		return ids
	}
	first := list()
	if len(first) != 1 {
		t.Fatalf("passkeys %v, want 1", first)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusConflict {
		t.Errorf("remove the last passkey: %d, want 409", status)
	}
	if status, body := newPasskey(testOrigin).register(b, ""); status != http.StatusCreated {
		t.Fatalf("second passkey: %d %s", status, body)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNoContent {
		t.Errorf("remove the first of two: %d, want 204", status)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNotFound {
		t.Errorf("remove it again: %d, want 404", status)
	}
	if ids := list(); len(ids) != 1 || ids[0] == first[0] {
		t.Errorf("passkeys after removal %v", ids)
	}
	if status, _ := pk.login(newBrowser(t, base)); status != http.StatusUnauthorized {
		t.Errorf("login with the removed passkey: %d, want 401", status)
	}
	if status, _ := newBrowser(t, base).do("GET", "/ui/auth/credentials", nil); status != http.StatusUnauthorized {
		t.Errorf("list without a session: %d, want 401", status)
	}
	if !hasAll(auditActions(t, s.db), "ui.passkey.remove") {
		t.Error("no ui.passkey.remove in the audit log")
	}
}

func TestLogin_WrongOriginAndLateFinish(t *testing.T) {
	s, clk, base := newTestUI(t)
	_, pk := loggedIn(t, s, base)
	evil := *pk
	evil.rp.Origin = "http://evil.example"
	if status, _ := evil.login(newBrowser(t, base)); status != http.StatusUnauthorized {
		t.Errorf("assertion for another origin: %d, want 401", status)
	}
	b := newBrowser(t, base)
	status, body := b.do("POST", "/ui/auth/login/begin", nil)
	if status != http.StatusOK {
		t.Fatalf("begin: %d %s", status, body)
	}
	opts, err := virtualwebauthn.ParseAssertionOptions(string(body))
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(CeremonyTTL + time.Second)
	if status, _ := b.do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(pk.rp, pk.auth, pk.cred, *opts)); status != http.StatusBadRequest {
		t.Errorf("finish after 5 minutes: %d, want 400", status)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/`
Expected: FAIL, build failed: `undefined: New`, `undefined: Service`.

- [ ] **Step 4: Extend `ui.go`**

Replace `internal/ui/ui.go` with:

```go
// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Names the browser, the server and the CLI agree on.
const (
	CookieName    = "conclave_ui"   // the session cookie
	HeaderName    = "X-Conclave-UI" // /v1 takes the cookie only with this header set to 1
	OperatorLabel = "operator"      // the admin key UI sessions act as
)

// Timeouts.
const (
	SetupTokenTTL = 10 * time.Minute // a setup token from ui setup
	CeremonyTTL   = 5 * time.Minute  // between begin and finish of a passkey ceremony
	IdleTimeout   = time.Hour        // a session unused this long ends
	MaxSession    = 12 * time.Hour   // a session ends this long after login
)

// Service serves /ui: the passkey ceremonies and sessions.
type Service struct {
	store
	wa     *webauthn.WebAuthn
	origin string // http://localhost:<port>, the only origin the UI runs on

	mu         sync.Mutex
	ceremonies map[string]ceremony // by hashToken(ceremony cookie)
}

// New returns the UI of a server listening on a loopback address at
// port. The relying party is localhost: an IP address cannot be one.
func New(db *sql.DB, port int) (*Service, error) {
	origin := fmt.Sprintf("http://localhost:%d", port)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  "localhost",
		RPDisplayName:         "Conclave",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: CeremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{
		store:      store{db: db, now: time.Now},
		wa:         wa,
		origin:     origin,
		ceremonies: map[string]ceremony{},
	}, nil
}

// Routes mounts the /ui endpoints on r.
func (s *Service) Routes(r chi.Router) {
	r.Post("/ui/auth/register/begin", s.registerBegin)
	r.Post("/ui/auth/register/finish", s.registerFinish)
	r.Post("/ui/auth/login/begin", s.loginBegin)
	r.Post("/ui/auth/login/finish", s.loginFinish)
	r.Post("/ui/auth/logout", s.logout)
	r.Get("/ui/auth/state", s.state)
	r.Get("/ui/auth/credentials", s.listCredentials)
	r.Delete("/ui/auth/credentials/{id}", s.removeCredential)
}
```

- [ ] **Step 5: Write `auth.go`**

`internal/ui/auth.go`:

```go
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// ceremonyCookie links a ceremony's begin and finish requests.
const ceremonyCookie = "conclave_ui_ceremony"

// operator is the one WebAuthn user: every passkey is the operator's.
type operator struct {
	handle []byte
	creds  []webauthn.Credential
}

func (o operator) WebAuthnID() []byte                         { return o.handle }
func (o operator) WebAuthnName() string                       { return OperatorLabel }
func (o operator) WebAuthnDisplayName() string                { return "Conclave operator" }
func (o operator) WebAuthnCredentials() []webauthn.Credential { return o.creds }

// ceremony is what a begin step keeps in memory for its finish step.
type ceremony struct {
	register   bool
	data       webauthn.SessionData
	setupToken string // registration with a setup token; "" when logged in
	name       string // passkey name (registration)
	expires    time.Time
}

// errOperatorUnusable: the label is taken by a key the UI cannot act as.
var errOperatorUnusable = errors.New(`a key labelled "operator" exists but is revoked or not an admin key`)

// operatorKeyID returns the id of the active admin key labelled operator.
func operatorKeyID(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM api_keys WHERE label = ? AND is_admin = 1 AND revoked_at IS NULL`, OperatorLabel).Scan(&id)
	return id, err
}

// ensureOperatorKey returns the operator key, creating it on first use:
// an admin key whose secret is random and never shown or stored in
// plain text, so only UI sessions can act as it.
func ensureOperatorKey(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	id, err := operatorKeyID(ctx, db)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	plaintext, phc, err := auth.GenerateToken()
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES (?, ?, ?, ?, 'internal', 1)`,
		OperatorLabel, phc, plaintext[3:11], now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, errOperatorUnusable
		}
		return 0, err
	}
	return res.LastInsertId()
}

// credID is the stored form of a credential id.
func credID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// userHandle is the operator's WebAuthn user handle: 32 random bytes,
// created once and kept in meta (ui_user_handle).
func (s *store) userHandle(ctx context.Context) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('ui_user_handle', ?) ON CONFLICT(key) DO NOTHING`,
		base64.RawURLEncoding.EncodeToString(b)); err != nil {
		return nil, err
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'ui_user_handle'`).Scan(&v); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(v)
}

// credentials returns the stored passkeys in the library's form.
func (s *store) credentials(ctx context.Context) ([]webauthn.Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_key, sign_count, transports, flags FROM ui_credentials ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var (
			id, transports string
			pk             []byte
			count          int64
			flags          int
		)
		if err := rows.Scan(&id, &pk, &count, &transports, &flags); err != nil {
			return nil, err
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil {
			return nil, err
		}
		c := webauthn.Credential{
			ID:            raw,
			PublicKey:     pk,
			Flags:         webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(flags)),
			Authenticator: webauthn.Authenticator{SignCount: uint32(count)},
		}
		for _, t := range strings.Split(transports, ",") {
			if t != "" {
				c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// credentialCount is the number of registered passkeys.
func (s *store) credentialCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_credentials`).Scan(&n)
	return n, err
}

// addCredential stores a new passkey. With first set it inserts only
// while no passkey exists (the setup-token path) and reports false
// otherwise.
func (s *store) addCredential(ctx context.Context, c *webauthn.Credential, name string, first bool) (bool, error) {
	transports := make([]string, len(c.Transport))
	for i, t := range c.Transport {
		transports[i] = string(t)
	}
	q := `INSERT INTO ui_credentials (id, public_key, sign_count, transports, flags, name, created_at) SELECT ?, ?, ?, ?, ?, ?, ?`
	if first {
		q += ` WHERE NOT EXISTS (SELECT 1 FROM ui_credentials)`
	}
	res, err := s.db.ExecContext(ctx, q, credID(c.ID), c.PublicKey, c.Authenticator.SignCount,
		strings.Join(transports, ","), int(c.Flags.ProtocolValue()), name, storage.FormatTS(s.now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// operator loads the WebAuthn user with its passkeys.
func (s *Service) operator(ctx context.Context) (operator, error) {
	h, err := s.userHandle(ctx)
	if err != nil {
		return operator{}, err
	}
	creds, err := s.credentials(ctx)
	return operator{handle: h, creds: creds}, err
}

// loggedIn reports whether r carries a live session cookie.
func (s *Service) loggedIn(r *http.Request) (bool, error) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false, nil
	}
	return s.session(r.Context(), c.Value)
}

// requireSession answers 401 unless r is logged in.
func (s *Service) requireSession(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return false
	}
	if !ok {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "log in first")
	}
	return ok
}

// startSession logs the operator in: a new session and its cookie.
func (s *Service) startSession(w http.ResponseWriter, r *http.Request) error {
	token, err := s.createSession(r.Context())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

// startCeremony keeps c for the finish step under a fresh cookie.
func (s *Service) startCeremony(w http.ResponseWriter, c ceremony) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	now := s.now()
	c.expires = now.Add(CeremonyTTL)
	s.mu.Lock()
	// ponytail: a sweep per begin, fine for one operator; a flood of
	// begins on the loopback address only grows the map for 5 minutes.
	for k, v := range s.ceremonies {
		if !now.Before(v.expires) {
			delete(s.ceremonies, k)
		}
	}
	s.ceremonies[hash] = c
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: token, Path: "/ui/auth/",
		MaxAge: int(CeremonyTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

// takeCeremony returns the request's ceremony of the given kind and
// forgets it: every ceremony finishes at most once.
func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, register bool) (ceremony, bool) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Path: "/ui/auth/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	ck, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	hash := hashToken(ck.Value)
	s.mu.Lock()
	c, ok := s.ceremonies[hash]
	delete(s.ceremonies, hash)
	s.mu.Unlock()
	return c, ok && c.register == register && s.now().Before(c.expires)
}

// registerGate decides who may register a passkey: a logged-in
// operator, or anyone with a valid setup token while no passkey exists.
// It returns the setup token to use up ("" when logged in).
func (s *Service) registerGate(w http.ResponseWriter, r *http.Request, setupToken string, passkeys int) (string, bool) {
	in, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return "", false
	}
	if in {
		return "", true
	}
	if passkeys > 0 {
		writeErr(w, r, http.StatusForbidden, "forbidden", "a passkey is registered: log in to add another")
		return "", false
	}
	valid, err := s.setupTokenValid(r.Context(), setupToken)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "setup token lookup")
		return "", false
	}
	if !valid {
		writeErr(w, r, http.StatusForbidden, "forbidden", "setup token invalid, used or expired: run conclave-mcp ui setup again")
		return "", false
	}
	return setupToken, true
}

func (s *Service) registerBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupToken string `json:"setup_token"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "passkey"
	}
	if len([]rune(name)) > 64 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "name must be at most 64 characters")
		return
	}
	op, err := s.operator(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return
	}
	token, ok := s.registerGate(w, r, req.SetupToken, len(op.creds))
	if !ok {
		return
	}
	creation, data, err := s.wa.BeginRegistration(op,
		webauthn.WithExclusions(webauthn.Credentials(op.creds).CredentialDescriptors()))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin registration")
		return
	}
	if err := s.startCeremony(w, ceremony{register: true, data: *data, setupToken: token, name: name}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "start ceremony")
		return
	}
	writeJSON(w, http.StatusOK, creation.Response)
}

func (s *Service) registerFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, true)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no passkey registration in progress, or it took longer than 5 minutes")
		return
	}
	if c.setupToken == "" && !s.requireSession(w, r) { // the session may have ended meanwhile
		return
	}
	op, err := s.operator(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return
	}
	cred, err := s.wa.FinishRegistration(op, c.data, r)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "passkey not accepted: "+err.Error())
		return
	}
	var keyID int64
	if c.setupToken != "" {
		if used, err := s.useSetupToken(ctx, c.setupToken); err != nil || !used {
			writeErr(w, r, http.StatusForbidden, "forbidden", "setup token invalid, used or expired: run conclave-mcp ui setup again")
			return
		}
		keyID, err = ensureOperatorKey(ctx, s.db, s.now())
		if errors.Is(err, errOperatorUnusable) {
			writeErr(w, r, http.StatusConflict, "conflict", err.Error())
			return
		}
	} else {
		keyID, err = operatorKeyID(ctx, s.db)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "operator key")
		return
	}
	added, err := s.addCredential(ctx, cred, c.name, c.setupToken != "")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "store passkey")
		return
	}
	if !added {
		writeErr(w, r, http.StatusConflict, "conflict", "another passkey was registered meanwhile: log in with it")
		return
	}
	via := "session"
	if c.setupToken != "" {
		if err := s.startSession(w, r); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "start session")
			return
		}
		via = "setup_token"
	}
	detail, _ := json.Marshal(map[string]string{"name": c.name, "via": via})
	_ = audit.Write(ctx, s.db, keyID, "ui.passkey.add", "passkey", credID(cred.ID), "", "ok", string(detail))
	writeJSON(w, http.StatusCreated, map[string]string{"id": credID(cred.ID), "name": c.name})
}

func (s *Service) loginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin login")
		return
	}
	if err := s.startCeremony(w, ceremony{data: *data}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "start ceremony")
		return
	}
	writeJSON(w, http.StatusOK, assertion.Response)
}

func (s *Service) loginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, false)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no login in progress, or it took longer than 5 minutes")
		return
	}
	op, err := s.operator(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return
	}
	_, cred, err := s.wa.FinishPasskeyLogin(func(_, handle []byte) (webauthn.User, error) {
		if !bytes.Equal(handle, op.handle) {
			return nil, errors.New("unknown user handle")
		}
		return op, nil
	}, c.data, r)
	if err != nil {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "passkey not accepted: "+err.Error())
		return
	}
	if cred.Authenticator.CloneWarning {
		writeErr(w, r, http.StatusForbidden, "forbidden", "the passkey's signature counter went backwards: possibly a cloned authenticator")
		return
	}
	keyID, err := operatorKeyID(ctx, s.db)
	if err != nil {
		writeErr(w, r, http.StatusForbidden, "forbidden", "the operator key is revoked or missing: run admin ui-reset with the service stopped, then ui setup")
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ui_credentials SET sign_count = ?, flags = ?, last_used_at = ? WHERE id = ?`,
		cred.Authenticator.SignCount, int(cred.Flags.ProtocolValue()), storage.FormatTS(s.now()), credID(cred.ID)); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "update passkey")
		return
	}
	if err := s.startSession(w, r); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "start session")
		return
	}
	_ = audit.Write(ctx, s.db, keyID, "ui.login", "passkey", credID(cred.ID), "", "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if c, err := r.Cookie(CookieName); err == nil {
		if live, _ := s.session(ctx, c.Value); live {
			if err := s.deleteSession(ctx, c.Value); err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "end session")
				return
			}
			keyID, _ := operatorKeyID(ctx, s.db)
			_ = audit.Write(ctx, s.db, keyID, "ui.logout", "session", "", "", "ok", "")
		}
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// state tells the start page what to show: {setup_needed, logged_in}.
func (s *Service) state(w http.ResponseWriter, r *http.Request) {
	n, err := s.credentialCount(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "count passkeys")
		return
	}
	in, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setup_needed": n == 0, "logged_in": in})
}

func (s *Service) listCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, created_at, last_used_at FROM ui_credentials ORDER BY created_at`)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list passkeys")
		return
	}
	defer rows.Close()
	type view struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		LastUsedAt string `json:"last_used_at,omitempty"`
	}
	out := []view{}
	for rows.Next() {
		var (
			v    view
			last sql.NullString
		)
		if err := rows.Scan(&v.ID, &v.Name, &v.CreatedAt, &last); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		v.LastUsedAt = last.String
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// removeCredential deletes a passkey, never the last one.
func (s *Service) removeCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ui_credentials WHERE id = ? AND (SELECT COUNT(*) FROM ui_credentials) > 1`, id)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "remove passkey")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM ui_credentials WHERE id = ?`, id).Scan(&one) == nil {
			writeErr(w, r, http.StatusConflict, "conflict", "the last passkey cannot be removed")
			return
		}
		writeErr(w, r, http.StatusNotFound, "not_found", "passkey not found")
		return
	}
	keyID, _ := operatorKeyID(ctx, s.db)
	_ = audit.Write(ctx, s.db, keyID, "ui.passkey.remove", "passkey", id, "", "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr writes the /v1 error envelope.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": r.Header.Get("X-Request-Id"),
	}})
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ -v -run 'Ceremonies|RegisterGate|Passkeys|Login'`
Expected: `PASS` for `TestCeremonies_SetupLoginLogout`, `TestRegisterGate`, `TestPasskeys_ListAndRemoveNotTheLast`, `TestLogin_WrongOriginAndLateFinish`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/ui
git commit -m "feat(ui): passkey ceremonies, operator key and registration gate" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: UI sessions on `/v1`, the setup-token endpoint, server wiring

**Files:**
- Modify: `internal/ui/ui.go` (whole file below), `internal/server/server.go` (import; `New` ~line 70; `buildRouter` ~lines 123–200)
- Create: `internal/ui/v1.go`, `internal/ui/v1_test.go`, `internal/server/ui_test.go`

**Interfaces:**
- Consumes: Task 2 (`Service`, `operatorKeyID`, `credentialCount`, `createSetupToken`, `session`, `writeErr`, `writeJSON`, test helpers).
- Produces: `Loopback(host string) bool` (`localhost` or a loopback IP; `""` and `0.0.0.0` are not), `Service.hosts [2]string` (`localhost:<port>`, `127.0.0.1:<port>`), `(*Service).uiHost(host string) bool`.
- Produces: `(*Service).Auth(bearer func(http.Handler) http.Handler) func(http.Handler) http.Handler`: a request with an `Authorization` header, or without the `conclave_ui` cookie, goes to `bearer` unchanged; a cookie request needs `X-Conclave-UI: 1` (else `401`), a UI Host (else `403`), the UI Origin unless GET (else `403`), a live session (else `401`) and an active `operator` key (else `401`), and then carries `auth.Claims{KeyID: <operator id>, Scopes: []string{auth.ScopeAdmin}}`.
- Produces: `(*Service).V1Routes(r chi.Router)`: `POST /v1/ui/setup-tokens` (admin) → `201 {"token", "expires_at"}` (`expires_at` in `storage.FormatTS` form), audit `ui.setup_token`; `403` non-admin; `409` once a passkey exists.
- Produces: `server.buildRouter(deps Deps, uiSvc *ui.Service)`; the UI is mounted on a loopback `cfg.Host` only.

- [ ] **Step 1: Write the failing tests**

`internal/ui/v1_test.go`:

```go
package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// withV1 mounts a stand-in /v1: bearer "admin" is an admin key (id 1),
// bearer "agent" a plain key (id 2); /v1/whoami echoes the claims and
// POST /v1/rooms answers 201.
func withV1(s *Service, r chi.Router) {
	bearer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var c auth.Claims
			switch r.Header.Get("Authorization") {
			case "Bearer admin":
				c = auth.Claims{KeyID: 1, Scopes: []string{auth.ScopeAdmin}}
			case "Bearer agent":
				c = auth.Claims{KeyID: 2}
			default:
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "missing or invalid Authorization header")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey{}, c)))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.Auth(bearer))
		s.V1Routes(r)
		r.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
			c, _ := auth.FromContext(r.Context())
			writeJSON(w, http.StatusOK, map[string]any{"id": c.KeyID, "admin": auth.IsAdmin(r.Context())})
		})
		r.Post("/rooms", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	})
}

func TestCookieAuth_HeaderHostOriginSession(t *testing.T) {
	s, clk, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	opID, err := operatorKeyID(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	var me struct {
		ID    int64 `json:"id"`
		Admin bool  `json:"admin"`
	}
	status, body := b.do("GET", "/v1/whoami", nil)
	if status != http.StatusOK || json.Unmarshal(body, &me) != nil || me.ID != opID || !me.Admin {
		t.Fatalf("whoami with the cookie: %d %s, want the operator key %d as admin", status, body, opID)
	}
	for _, c := range []struct {
		name   string
		method string
		hdr    map[string]string
		host   string
		want   int
	}{
		{"cookie without the header", "GET", map[string]string{HeaderName: ""}, testHost, 401},
		{"wrong Host", "GET", nil, "attacker.example:7432", 403},
		{"other port", "GET", nil, "localhost:9999", 403},
		{"127.0.0.1 is a UI host", "GET", nil, "127.0.0.1:7432", 200},
		{"POST from another origin", "POST", map[string]string{"Origin": "http://attacker.example"}, testHost, 403},
		{"POST without Origin", "POST", map[string]string{"Origin": ""}, testHost, 403},
		{"POST from the UI", "POST", nil, testHost, 201},
	} {
		b.host = c.host
		path := "/v1/whoami"
		if c.method == "POST" {
			path = "/v1/rooms"
		}
		resp := b.send(c.method, path, nil, c.hdr)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	b.host = testHost

	// A bearer key still wins over the cookie.
	resp := b.send("GET", "/v1/whoami", nil, map[string]string{"Authorization": "Bearer agent"})
	_ = json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if me.ID != 2 || me.Admin {
		t.Errorf("bearer with a cookie: acted as %d (admin %v), want the agent key", me.ID, me.Admin)
	}

	clk.advance(IdleTimeout + time.Minute)
	if status, _ := b.do("GET", "/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("idle session: %d, want 401", status)
	}
}

func TestCookieAuth_RevokedOperatorKey(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = '2026-11-12T10:00:00Z' WHERE label = 'operator'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := b.do("GET", "/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("revoked operator key: %d, want 401", status)
	}
}

func TestSetupTokenEndpoint(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b := newBrowser(t, base)
	post := func(key string) (int, []byte) {
		resp := b.send("POST", "/v1/ui/setup-tokens", nil, map[string]string{"Authorization": "Bearer " + key})
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	if status, _ := post("agent"); status != http.StatusForbidden {
		t.Errorf("agent key: %d, want 403", status)
	}
	status, body := post("admin")
	var tok struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &tok) != nil || len(tok.Token) != 43 {
		t.Fatalf("admin key: %d %s", status, body)
	}
	if exp, err := time.Parse(time.RFC3339Nano, tok.ExpiresAt); err != nil || !exp.Equal(s.now().Add(SetupTokenTTL)) {
		t.Errorf("expires_at %q (%v), want now + 10 minutes", tok.ExpiresAt, err)
	}
	if !hasAll(auditActions(t, s.db), "ui.setup_token") {
		t.Error("no ui.setup_token in the audit log")
	}
	if status, body := newPasskey(testOrigin).register(newBrowser(t, base), tok.Token); status != http.StatusCreated {
		t.Fatalf("register with the endpoint's token: %d %s", status, body)
	}
	if status, _ := post("admin"); status != http.StatusConflict {
		t.Errorf("after setup: %d, want 409", status)
	}
}

func TestLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true, "127.0.0.2": true,
		"": false, "0.0.0.0": false, "192.168.1.10": false, "example.com": false,
	} {
		if got := Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}
```

`internal/server/ui_test.go`:

```go
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/internal/ui"
	"github.com/hermes/conclave-mcp/migrations"
)

// uiDB is a migrated database with an operator key and a UI session
// whose cookie value is "session-token".
func uiDB(t *testing.T) *sql.DB {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sum := sha256.Sum256([]byte("session-token"))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES ('operator', 'x', 'opprefix', '2026-11-12T10:00:00Z', 'internal', 1)`, nil},
		{`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?)`,
			[]any{hex.EncodeToString(sum[:]), storage.FormatTS(now), storage.FormatTS(now), storage.FormatTS(now.Add(time.Hour))}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func uiRequest(method, path, host string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: ui.CookieName, Value: "session-token"})
	req.Header.Set(ui.HeaderName, "1")
	return req
}

// On a loopback address a UI session reaches /v1 as the operator key.
func TestServer_UISessionOnV1(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest("GET", "/v1/whoami", "localhost:7432"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"label":"operator"`) {
		t.Fatalf("whoami with the UI cookie: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest("GET", "/ui/auth/state", "localhost:7432"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logged_in":true`) {
		t.Errorf("/ui/auth/state: %d %s", rec.Code, rec.Body)
	}
}

// On any other address there is no UI and the cookie counts for nothing.
func TestServer_NoUIOffLoopback(t *testing.T) {
	s, err := New(Config{Host: "0.0.0.0", Port: 7432}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/v1/whoami": http.StatusUnauthorized, "/ui/auth/state": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, uiRequest("GET", path, "localhost:7432"))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ ./internal/server/`
Expected: FAIL. `internal/ui`: build failed, `undefined: Loopback`, `s.Auth undefined`; `internal/server`: `TestServer_UISessionOnV1` fails with `401`.

- [ ] **Step 3: Add `Loopback`, `uiHost` and the hosts to `ui.go`**

Replace `internal/ui/ui.go` with:

```go
// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import (
	"database/sql"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Names the browser, the server and the CLI agree on.
const (
	CookieName    = "conclave_ui"   // the session cookie
	HeaderName    = "X-Conclave-UI" // /v1 takes the cookie only with this header set to 1
	OperatorLabel = "operator"      // the admin key UI sessions act as
)

// Timeouts.
const (
	SetupTokenTTL = 10 * time.Minute // a setup token from ui setup
	CeremonyTTL   = 5 * time.Minute  // between begin and finish of a passkey ceremony
	IdleTimeout   = time.Hour        // a session unused this long ends
	MaxSession    = 12 * time.Hour   // a session ends this long after login
)

// Service serves /ui: the passkey ceremonies and sessions.
type Service struct {
	store
	wa     *webauthn.WebAuthn
	origin string    // http://localhost:<port>, the only origin the UI runs on
	hosts  [2]string // the Host headers /ui and cookie sessions accept: localhost:<port>, 127.0.0.1:<port>

	mu         sync.Mutex
	ceremonies map[string]ceremony // by hashToken(ceremony cookie)
}

// New returns the UI of a server listening on a loopback address at
// port. The relying party is localhost: an IP address cannot be one.
func New(db *sql.DB, port int) (*Service, error) {
	origin := fmt.Sprintf("http://localhost:%d", port)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  "localhost",
		RPDisplayName:         "Conclave",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: CeremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{
		store:      store{db: db, now: time.Now},
		wa:         wa,
		origin:     origin,
		hosts:      [2]string{fmt.Sprintf("localhost:%d", port), fmt.Sprintf("127.0.0.1:%d", port)},
		ceremonies: map[string]ceremony{},
	}, nil
}

// Loopback reports whether the listen host is a loopback address. The
// UI and cookie sessions exist only then: passkeys need a secure
// context, and plain HTTP is one only on localhost.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// uiHost reports whether host (a request's Host header) is the UI's.
func (s *Service) uiHost(host string) bool { return host == s.hosts[0] || host == s.hosts[1] }

// Routes mounts the /ui endpoints on r.
func (s *Service) Routes(r chi.Router) {
	r.Post("/ui/auth/register/begin", s.registerBegin)
	r.Post("/ui/auth/register/finish", s.registerFinish)
	r.Post("/ui/auth/login/begin", s.loginBegin)
	r.Post("/ui/auth/login/finish", s.loginFinish)
	r.Post("/ui/auth/logout", s.logout)
	r.Get("/ui/auth/state", s.state)
	r.Get("/ui/auth/credentials", s.listCredentials)
	r.Delete("/ui/auth/credentials/{id}", s.removeCredential)
}
```

- [ ] **Step 4: Write `v1.go`**

`internal/ui/v1.go`:

```go
package ui

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Auth wraps the bearer middleware of /v1. A request without an
// Authorization header that carries the conclave_ui cookie acts as the
// operator key if it also has X-Conclave-UI: 1 (else 401), a UI Host
// (else 403), the UI Origin unless it is a GET (else 403) and a live
// session (else 401). Every other request goes to bearer unchanged.
func (s *Service) Auth(bearer func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		viaBearer := bearer(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil || r.Header.Get("Authorization") != "" {
				viaBearer.ServeHTTP(w, r)
				return
			}
			if r.Header.Get(HeaderName) != "1" {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "a UI session needs the X-Conclave-UI: 1 header")
				return
			}
			if !s.uiHost(r.Host) {
				writeErr(w, r, http.StatusForbidden, "forbidden", "host not allowed")
				return
			}
			if r.Method != http.MethodGet && r.Header.Get("Origin") != s.origin {
				writeErr(w, r, http.StatusForbidden, "forbidden", "origin not allowed")
				return
			}
			live, err := s.session(r.Context(), c.Value)
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
				return
			}
			if !live {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "session expired: log in again")
				return
			}
			id, err := operatorKeyID(r.Context(), s.db)
			if err != nil {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "the operator key is revoked or missing")
				return
			}
			ctx := context.WithValue(r.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: id, Scopes: []string{auth.ScopeAdmin}})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// V1Routes mounts the UI's /v1 endpoint on the /v1 router.
func (s *Service) V1Routes(r chi.Router) {
	r.Post("/ui/setup-tokens", s.newSetupToken)
}

// newSetupToken answers POST /v1/ui/setup-tokens (admin keys): a token
// that registers the first passkey, valid SetupTokenTTL. 409 once a
// passkey exists: more are added from a logged-in session.
func (s *Service) newSetupToken(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
		return
	}
	ctx := r.Context()
	n, err := s.credentialCount(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "count passkeys")
		return
	}
	if n > 0 {
		writeErr(w, r, http.StatusConflict, "conflict",
			"a passkey is registered: add more on the Passkeys page, or run admin ui-reset (service stopped) to start over")
		return
	}
	token, expires, err := s.createSetupToken(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "create setup token")
		return
	}
	c, _ := auth.FromContext(ctx)
	exp := storage.FormatTS(expires)
	detail, _ := json.Marshal(map[string]string{"expires_at": exp})
	_ = audit.Write(ctx, s.db, c.KeyID, "ui.setup_token", "ui", "setup_token", "", "ok", string(detail))
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "expires_at": exp})
}
```

- [ ] **Step 5: Wire the server**

In `internal/server/server.go` add the import (after `internal/rooms`):

```go
	"github.com/hermes/conclave-mcp/internal/ui"
```

In `New` replace

```go
	s := &Server{cfg: cfg, logger: cfg.Logger}

	s.router = s.buildRouter(deps)
```

with

```go
	s := &Server{cfg: cfg, logger: cfg.Logger}

	// The web UI and its cookie sessions exist only on a loopback
	// listen address (docs/web-ui.md).
	var uiSvc *ui.Service
	if ui.Loopback(cfg.Host) {
		var err error
		if uiSvc, err = ui.New(deps.DB, cfg.Port); err != nil {
			return nil, fmt.Errorf("web ui: %w", err)
		}
	}
	s.router = s.buildRouter(deps, uiSvc)
```

Replace `func (s *Server) buildRouter(deps Deps) chi.Router {` with `func (s *Server) buildRouter(deps Deps, uiSvc *ui.Service) chi.Router {`. After

```go
	keysSvc := keys.NewService(deps.DB)
	verifier := auth.NewVerifier(keysSvc, deps.MaxConcurrentVerify)
```

add

```go
	// authMW is the bearer verifier; with the web UI it also accepts a
	// UI session cookie and acts as the operator key (internal/ui).
	authMW := verifier.Middleware()
	if uiSvc != nil {
		authMW = uiSvc.Auth(authMW)
		uiSvc.Routes(r)
	}
```

In the `/v1` route replace `r.Use(verifier.Middleware())` with `r.Use(authMW)`, and after `claimsSvc.Routes(r)` add

```go
		if uiSvc != nil {
			uiSvc.V1Routes(r)
		}
```

Replace `r.With(verifier.Middleware()).Get("/v1/rooms/{id}/events", evH.Handle)` with `r.With(authMW).Get("/v1/rooms/{id}/events", evH.Handle)`. Leave `r.With(verifier.Middleware()).Post("/v1/mcp", s.mcpProxy())` as it is (bearer only, see Global Constraints).

- [ ] **Step 6: Run the tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ ./internal/server/`
Expected: `ok` for both.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok` (the existing server tests use `127.0.0.1` with port 0, which now mounts the UI; nothing they call changes).
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 7: Commit**

```bash
git add internal/ui internal/server
git commit -m "feat(ui): UI sessions on /v1 and the setup-token endpoint" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Serve the pages: embed, guard, security headers, redirect

**Files:**
- Modify: `internal/ui/ui.go` (whole file below: embedded assets, `Routes` behind `guard`)
- Create: `internal/ui/assets/index.html`, `internal/ui/assets/app.css`, `internal/ui/ui_test.go`

**Interfaces:**
- Consumes: Task 3 (`hosts`, `uiHost`, `origin`), Task 2 (the handlers, `writeErr`).
- Produces: `var assets embed.FS` (`//go:embed assets`); `Routes` now mounts, all behind `guard`: `GET /ui` → `302 /ui/`, `GET /ui/*` → the files of `internal/ui/assets/`, and the Task 2 endpoints. `guard` sets the four headers on every `/ui` response, answers `403 host not allowed` unless the Host is a UI host, redirects GET/HEAD on `127.0.0.1:<port>` to `http://localhost:<port>` + the same path and query (`302`), answers `403 origin not allowed` to any other method without `Origin: http://localhost:<port>`, and caps bodies at 64 KiB.
- Produces the page skeleton Task 5 relies on: `<nav id="nav">`, `<main id="app">`, `app.css`, `<script type="module" src="app.js">`; CSS classes `error`, `muted`, `meta`, `message`, `ext`, `external`, `fence`, `rights`, `secret`, `danger`.

- [ ] **Step 1: Write the failing tests**

`internal/ui/ui_test.go`:

```go
package ui

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestPages_ServedWithSecurityHeaders(t *testing.T) {
	_, _, base := newTestUI(t)
	b := newBrowser(t, base)
	for path, ctype := range map[string]string{"/ui/": "text/html", "/ui/app.css": "text/css"} {
		resp := b.send("GET", path, nil, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		for k, want := range map[string]string{
			"Content-Security-Policy": "default-src 'self'; base-uri 'none'; frame-ancestors 'none'",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := resp.Header.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", path, k, got, want)
			}
		}
		if path == "/ui/" && !strings.Contains(string(body), `<script type="module" src="app.js"></script>`) {
			t.Errorf("index.html does not load app.js:\n%s", body)
		}
	}
	resp := b.send("GET", "/ui", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("/ui: %d to %q, want 302 to /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPages_HostAndOriginChecks(t *testing.T) {
	_, _, base := newTestUI(t)
	b := newBrowser(t, base)
	b.host = "127.0.0.1:7432"
	resp := b.send("GET", "/ui/?x=1", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://localhost:7432/ui/?x=1" {
		t.Errorf("127.0.0.1: %d to %q, want 302 to http://localhost:7432/ui/?x=1", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, host := range []string{"attacker.example:7432", "localhost:9999", "localhost"} {
		b.host = host
		if status, _ := b.do("GET", "/ui/", nil); status != http.StatusForbidden {
			t.Errorf("Host %s: %d, want 403", host, status)
		}
	}
	b.host = testHost
	for name, hdr := range map[string]map[string]string{
		"another origin": {"Origin": "http://attacker.example"},
		"no origin":      {"Origin": ""},
		"127.0.0.1":      {"Origin": "http://127.0.0.1:7432"},
	} {
		resp := b.send("POST", "/ui/auth/login/begin", nil, hdr)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST from %s: %d, want 403", name, resp.StatusCode)
		}
	}
	if status, _ := b.do("POST", "/ui/auth/login/begin", nil); status != http.StatusOK {
		t.Errorf("POST from the UI origin: %d, want 200", status)
	}
}

// Agent-written text must never become markup: the scripts only use
// textContent, and the page has no inline script, style or handler.
func TestAssets_NoMarkupFromData(t *testing.T) {
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function`)
	inline := regexp.MustCompile(`<script>|<script [^>]*>[^<]|<style|\sstyle=|\son[a-z]+=`)
	err := fs.WalkDir(assets, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".js") && banned.Match(b) {
			t.Errorf("%s: %q", path, banned.Find(b))
		}
		if strings.HasSuffix(path, ".html") && inline.Match(b) {
			t.Errorf("%s: inline %q", path, inline.Find(b))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/`
Expected: FAIL, build failed: `undefined: assets`.

- [ ] **Step 3: Write the page and the style sheet**

`internal/ui/assets/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Conclave</title>
<link rel="stylesheet" href="app.css">
<script type="module" src="app.js"></script>
</head>
<body>
<header>
<strong>Conclave</strong>
<nav id="nav"></nav>
</header>
<main id="app"><p>Loading…</p></main>
</body>
</html>
```

`internal/ui/assets/app.css`:

```css
:root {
  color-scheme: light dark;
  --fg: #1d1d1f;
  --bg: #ffffff;
  --muted: #6e6e73;
  --line: #d2d2d7;
  --accent: #0a66c2;
  --danger: #b3261e;
  --ext-bg: #fff4e5;
  --ext-line: #c77700;
}
@media (prefers-color-scheme: dark) {
  :root {
    --fg: #f5f5f7;
    --bg: #1c1c1e;
    --muted: #a1a1a6;
    --line: #3a3a3c;
    --accent: #4da3ff;
    --danger: #ff6b60;
    --ext-bg: #332200;
    --ext-line: #e8a33d;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  color: var(--fg);
  background: var(--bg);
}
header {
  display: flex;
  flex-wrap: wrap;
  gap: 1rem;
  align-items: center;
  padding: 0.6rem 1rem;
  border-bottom: 1px solid var(--line);
}
nav { display: flex; flex-wrap: wrap; gap: 0.8rem; align-items: center; flex: 1; }
nav .rooms { display: flex; flex-wrap: wrap; gap: 0.6rem; padding-left: 0.8rem; border-left: 1px solid var(--line); }
nav button { margin-left: auto; }
main { padding: 1rem; max-width: 72rem; }
a { color: var(--accent); }
h1 { font-size: 1.4rem; }
h2 { font-size: 1.1rem; margin-top: 1.6rem; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 0.3rem 0.5rem; border-bottom: 1px solid var(--line); vertical-align: top; }
th { color: var(--muted); font-weight: 600; }
button { font: inherit; padding: 0.2rem 0.7rem; cursor: pointer; }
button.danger { color: var(--danger); }
input, select { font: inherit; }
pre { white-space: pre-wrap; overflow-wrap: anywhere; margin: 0.3rem 0; }
code { overflow-wrap: anywhere; }
.error { color: var(--danger); }
.muted, .meta { color: var(--muted); }
.meta { font-size: 0.85rem; display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; }
.message { padding: 0.5rem 0; border-bottom: 1px solid var(--line); }
.message details { margin: 0.3rem 0 0 1.5rem; }
.ext { font-weight: 700; color: var(--ext-line); }
.external { background: var(--ext-bg); border-left: 4px solid var(--ext-line); padding: 0.3rem 0.6rem; }
.fence { font-family: ui-monospace, monospace; font-size: 0.8rem; color: var(--ext-line); }
.rights { display: inline-flex; flex-wrap: wrap; gap: 0.2rem 0.6rem; }
.secret { padding: 0.6rem; border: 1px solid var(--line); }
form { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; }
```

- [ ] **Step 4: Embed and guard in `ui.go`**

Replace `internal/ui/ui.go` with:

```go
// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Names the browser, the server and the CLI agree on.
const (
	CookieName    = "conclave_ui"   // the session cookie
	HeaderName    = "X-Conclave-UI" // /v1 takes the cookie only with this header set to 1
	OperatorLabel = "operator"      // the admin key UI sessions act as
)

// Timeouts.
const (
	SetupTokenTTL = 10 * time.Minute // a setup token from ui setup
	CeremonyTTL   = 5 * time.Minute  // between begin and finish of a passkey ceremony
	IdleTimeout   = time.Hour        // a session unused this long ends
	MaxSession    = 12 * time.Hour   // a session ends this long after login
)

// Service serves /ui: the passkey ceremonies and sessions.
type Service struct {
	store
	wa     *webauthn.WebAuthn
	origin string    // http://localhost:<port>, the only origin the UI runs on
	hosts  [2]string // the Host headers /ui and cookie sessions accept: localhost:<port>, 127.0.0.1:<port>

	mu         sync.Mutex
	ceremonies map[string]ceremony // by hashToken(ceremony cookie)
}

// New returns the UI of a server listening on a loopback address at
// port. The relying party is localhost: an IP address cannot be one.
func New(db *sql.DB, port int) (*Service, error) {
	origin := fmt.Sprintf("http://localhost:%d", port)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  "localhost",
		RPDisplayName:         "Conclave",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: CeremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{
		store:      store{db: db, now: time.Now},
		wa:         wa,
		origin:     origin,
		hosts:      [2]string{fmt.Sprintf("localhost:%d", port), fmt.Sprintf("127.0.0.1:%d", port)},
		ceremonies: map[string]ceremony{},
	}, nil
}

// Loopback reports whether the listen host is a loopback address. The
// UI and cookie sessions exist only then: passkeys need a secure
// context, and plain HTTP is one only on localhost.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// uiHost reports whether host (a request's Host header) is the UI's.
func (s *Service) uiHost(host string) bool { return host == s.hosts[0] || host == s.hosts[1] }

//go:embed assets
var assets embed.FS

// csp allows the page's own files only: no inline script or style, no
// framing.
const csp = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// Routes mounts /ui on r: the pages under /ui/ and the passkey and
// session endpoints under /ui/auth/, all behind guard.
func (s *Service) Routes(r chi.Router) {
	files, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	pages := http.StripPrefix("/ui/", http.FileServer(http.FS(files)))
	r.Group(func(r chi.Router) {
		r.Use(s.guard)
		r.Get("/ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
		r.Get("/ui/*", pages.ServeHTTP)
		r.Post("/ui/auth/register/begin", s.registerBegin)
		r.Post("/ui/auth/register/finish", s.registerFinish)
		r.Post("/ui/auth/login/begin", s.loginBegin)
		r.Post("/ui/auth/login/finish", s.loginFinish)
		r.Post("/ui/auth/logout", s.logout)
		r.Get("/ui/auth/state", s.state)
		r.Get("/ui/auth/credentials", s.listCredentials)
		r.Delete("/ui/auth/credentials/{id}", s.removeCredential)
	})
}

// guard runs before every /ui request: the security headers, the Host
// check (DNS rebinding), the redirect from 127.0.0.1 to localhost (the
// passkeys' relying party), the Origin check for writes (CSRF) and a
// body limit.
func (s *Service) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if !s.uiHost(r.Host) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "host not allowed")
			return
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if read && r.Host == s.hosts[1] {
			http.Redirect(w, r, s.origin+r.URL.RequestURI(), http.StatusFound)
			return
		}
		if !read && r.Header.Get("Origin") != s.origin {
			writeErr(w, r, http.StatusForbidden, "forbidden", "origin not allowed")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ ./internal/server/`
Expected: `ok` for both (the Task 2 and 3 tests already send the UI's Host and Origin).

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 6: Commit**

```bash
git add internal/ui
git commit -m "feat(ui): serve the embedded pages behind host, origin and CSP checks" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Frontend: helpers, router, login, setup and passkeys

**Files:**
- Create: `internal/ui/assets/lib.js`, `internal/ui/assets/app.js`

**Interfaces:**
- Consumes: Task 4's page skeleton; Task 2's `/ui/auth/*` endpoints; `GET /v1/rooms` (header).
- Produces `lib.js` exports: `h(tag, attrs = {}, ...children)` (attributes and `on<event>` listeners; `true` = empty attribute; children are nodes or strings → text), `table(headers, rows)`, `class APIError` (`status`, `code`, `message` = `"<code>: <message>"`), `api(method, path, body?)` (same-origin fetch with `X-Conclave-UI: 1`, JSON in and out, throws `APIError`), `errorBox(err)`, `when(ts)` (local time), `confirmButton(label, question, action)` (asks in the page: question, Yes, No).
- Produces in `app.js`: `const routes = [[RegExp, page], ...]`, where `page(view, ...decodedCaptures)` fills the `<div>` `view` and may return a stop function; `render()` swaps `view` into `<main>` only if no newer render started (a slow page cannot overwrite a newer one) and stops the previous page; a `401` from any call shows the login page. Tasks 6 and 7 add routes and imports to `app.js`.
- Until Task 6, `#/` (where login lands) shows "Not found"; the header already links Rooms, Keys, Audit, Passkeys and the rooms.

There is no JavaScript test runner (Global Constraints); the checks are the syntax check, `TestAssets_NoMarkupFromData` and the browser smoke test in Task 10.

- [ ] **Step 1: Write `lib.js`**

`internal/ui/assets/lib.js`:

```js
// Shared helpers. Server data reaches the page only as text: h() turns
// strings into text nodes and attribute values, never into markup.

// h builds an element. attrs are attributes, except on<event> keys,
// which become listeners; true sets an empty attribute, false/null/
// undefined leave it out. Children are nodes or strings (text).
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) {
    if (c !== undefined && c !== null && c !== false) el.append(c);
  }
  return el;
}

// table builds a table with a header row.
export function table(headers, rows) {
  return h('table', {},
    h('thead', {}, h('tr', {}, headers.map(t => h('th', {}, t)))),
    h('tbody', {}, rows));
}

export class APIError extends Error {
  constructor(status, code, message) {
    super(`${code}: ${message}`);
    this.status = status;
    this.code = code;
  }
}

// api calls the server as the logged-in operator: same origin, the
// session cookie, and the header /v1 needs to accept the cookie.
export async function api(method, path, body) {
  const opts = { method, headers: { 'X-Conclave-UI': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = null; }
  if (!res.ok) {
    const e = data && data.error ? data.error : { code: String(res.status), message: text.trim() || res.statusText };
    throw new APIError(res.status, e.code, e.message);
  }
  return data;
}

// errorBox shows an error, for /v1 errors with its code and message.
export function errorBox(err) {
  return h('p', { class: 'error', role: 'alert' }, err instanceof Error ? err.message : String(err));
}

// when formats a server timestamp in local time; '' for none.
export function when(ts) {
  return ts ? new Date(ts).toLocaleString() : '';
}

// confirmButton is a button that asks in the page before action runs.
export function confirmButton(label, question, action) {
  const wrap = h('span', { class: 'confirm' });
  const reset = () => wrap.replaceChildren(h('button', { class: 'danger', onclick: ask }, label));
  function ask() {
    wrap.replaceChildren(question + ' ',
      h('button', { class: 'danger', onclick: run }, 'Yes'), ' ',
      h('button', { onclick: reset }, 'No'));
  }
  async function run() {
    try { await action(); } catch (err) { wrap.replaceChildren(errorBox(err)); }
  }
  reset();
  return wrap;
}
```

- [ ] **Step 2: Write `app.js`**

`internal/ui/assets/app.js`:

```js
// The page: routing, the header, passkey login and setup, passkeys.
import { h, api, errorBox, confirmButton, table, when } from './lib.js';

const main = document.getElementById('app');
const nav = document.getElementById('nav');

// registerPasskey runs a registration ceremony: with a setup token for
// the first passkey, else as the logged-in operator.
async function registerPasskey(name, setupToken = '') {
  const options = await api('POST', '/ui/auth/register/begin', { name, setup_token: setupToken });
  const cred = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options) });
  await api('POST', '/ui/auth/register/finish', cred.toJSON());
}

async function login() {
  const options = await api('POST', '/ui/auth/login/begin');
  const cred = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options) });
  await api('POST', '/ui/auth/login/finish', cred.toJSON());
}

// action makes a click handler that shows errors in msg.
function action(msg, fn) {
  return async () => {
    msg.replaceChildren();
    try { await fn(); } catch (err) { msg.replaceChildren(errorBox(err)); }
  };
}

function loginPage(view, state) {
  if (state.setup_needed) {
    view.replaceChildren(h('h1', {}, 'Set up the web UI'),
      h('p', {}, 'No passkey is registered yet. With the admin key, run'),
      h('pre', {}, 'conclave-mcp ui setup'),
      h('p', {}, 'and open the URL it prints in this browser.'));
    return;
  }
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Log in'),
    h('button', { onclick: action(msg, async () => { await login(); await render(); }) }, 'Log in with a passkey'),
    msg);
}

function setupPage(view, token) {
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Register your passkey'),
    h('p', {}, 'The passkey (Touch ID or another authenticator on this device) is how you log in to this server from now on.'),
    h('label', {}, 'Name ', name), ' ',
    h('button', {
      onclick: action(msg, async () => {
        await registerPasskey(name.value, token);
        history.replaceState(null, '', '#/'); // drop the used token from the address bar
        await render();
      }),
    }, 'Register passkey'),
    msg);
}

async function passkeysPage(view) {
  const { credentials } = await api('GET', '/ui/auth/credentials');
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Passkeys'),
    table(['Name', 'Added', 'Last used', ''], credentials.map(c => h('tr', {},
      h('td', {}, c.name), h('td', {}, when(c.created_at)), h('td', {}, when(c.last_used_at)),
      h('td', {}, credentials.length > 1
        ? confirmButton('Remove', 'Remove this passkey?', async () => {
          await api('DELETE', '/ui/auth/credentials/' + encodeURIComponent(c.id));
          await render();
        })
        : h('span', { class: 'muted' }, 'the last passkey cannot be removed'))))),
    h('h2', {}, 'Add a passkey'),
    h('label', {}, 'Name ', name), ' ',
    h('button', { onclick: action(msg, async () => { await registerPasskey(name.value); await render(); }) }, 'Add passkey'),
    msg);
}

// header returns the navigation: pages, the rooms, logout.
async function header() {
  const { rooms } = await api('GET', '/v1/rooms');
  const msg = h('span');
  return [
    h('a', { href: '#/' }, 'Rooms'), h('a', { href: '#/keys' }, 'Keys'),
    h('a', { href: '#/audit' }, 'Audit'), h('a', { href: '#/passkeys' }, 'Passkeys'),
    h('span', { class: 'rooms' }, (rooms || []).map(r => h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name))),
    msg,
    h('button', { onclick: action(msg, async () => { await api('POST', '/ui/auth/logout'); await render(); }) }, 'Log out'),
  ];
}

// routes maps the address after # to a page. A page fills the element
// it gets and may return a function that stops what it started (the
// live stream of a room).
const routes = [
  [/^#\/passkeys$/, passkeysPage],
];

let leave = null; // stops the shown page's live stream
let seq = 0; // the newest render; an older one that finishes late shows nothing

async function render() {
  if (leave) { leave(); leave = null; }
  const mine = ++seq;
  const view = h('div');
  const show = (links = []) => {
    if (mine !== seq) return false;
    nav.replaceChildren(...links);
    main.replaceChildren(view);
    return true;
  };
  const hash = location.hash || '#/';
  try {
    const setup = /^#setup=(.+)$/.exec(hash);
    if (setup) {
      setupPage(view, setup[1]);
      return show();
    }
    const state = await api('GET', '/ui/auth/state');
    if (!state.logged_in) {
      loginPage(view, state);
      return show();
    }
    const links = await header();
    const route = routes.find(([re]) => re.test(hash));
    if (!route) {
      view.replaceChildren(h('h1', {}, 'Not found'), h('p', {}, h('a', { href: '#/' }, 'Back to the rooms')));
      return show(links);
    }
    const stop = await route[1](view, ...route[0].exec(hash).slice(1).map(decodeURIComponent));
    if (show(links)) leave = stop || null;
    else if (stop) stop();
  } catch (err) {
    if (err.status === 401) { // the session ended: idle, expired or logged out elsewhere
      try { loginPage(view, await api('GET', '/ui/auth/state')); } catch (e) { view.replaceChildren(errorBox(e)); }
    } else {
      view.replaceChildren(errorBox(err));
    }
    show();
  }
}

window.addEventListener('hashchange', render);
render();
```

- [ ] **Step 3: Check the scripts**

Run: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
Expected: `ok`.

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ -run TestAssets -v`
Expected: `--- PASS: TestAssets_NoMarkupFromData`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 4: Commit**

```bash
git add internal/ui/assets
git commit -m "feat(ui): login, setup and passkeys pages" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Frontend: rooms and the room page

**Files:**
- Create: `internal/ui/assets/rooms.js`
- Modify: `internal/ui/assets/lib.js` (append), `internal/ui/assets/app.js` (one import, two routes)

**Interfaces:**
- Consumes: `lib.js` from Task 5; `/v1` endpoints `GET/POST /v1/rooms`, `GET /v1/rooms/{id}`, `GET .../members`, `GET .../claims`, `GET .../messages?limit=500&since=`, `GET .../events?wait=300` (SSE: `: ok` once subscribed, then `event: message.created` / `data: <message JSON>`), `PATCH`/`DELETE .../members/{key_id}`, `POST .../invite` (`{key_id, rights, allow_multi_room}`), `POST .../archive`, `DELETE .../messages/{id}`, `GET /v1/keys` (labels).
- Produces `lib.js` exports: `RIGHTS` (the ten rights in server order), `rerender()` (dispatches `hashchange`), `rightsPicker(selected = [])` → `{el, value()}`.
- Produces `rooms.js` exports: `roomsPage(view)` (`#/`: every room with open requests of the last 24 h and last activity; create room), `roomPage(view, id)` (`#/rooms/<id>`; returns the stop function of its live stream, none for an archived room).
- Open requests follow `conclave-mcp status`: questions, task assignments, handoffs, alerts and decisions that no other key answered with `metadata.reply_to`. Threads: a message whose `reply_to` chain reaches an older message is shown under that root in a `<details>` element (open state kept across live updates).

- [ ] **Step 1: Append to `lib.js`**

Append to `internal/ui/assets/lib.js`:

```js
// Every right a membership can hold, in the server's order.
export const RIGHTS = ['read', 'claim', 'question', 'answer', 'decision', 'handoff',
  'status_update', 'task_assignment', 'alert', 'system'];

// rerender draws the current page again (app.js listens for this).
export function rerender() {
  window.dispatchEvent(new Event('hashchange'));
}

// rightsPicker is a checkbox per right; value() returns the checked ones.
export function rightsPicker(selected = []) {
  const boxes = RIGHTS.map(r => h('input', { type: 'checkbox', value: r, checked: selected.includes(r) }));
  return {
    el: h('span', { class: 'rights' }, boxes.map((b, i) => h('label', {}, b, ' ' + RIGHTS[i]))),
    value: () => boxes.filter(b => b.checked).map(b => b.value),
  };
}
```

- [ ] **Step 2: Write `rooms.js`**

`internal/ui/assets/rooms.js`:

```js
// Rooms: the list, and one room with its messages (live), open
// requests, claims and members.
import { h, api, errorBox, confirmButton, table, when, rerender, rightsPicker } from './lib.js';

const PAGE = 500; // the server's largest message page

// listMessages returns a room's messages after since, oldest first.
async function listMessages(roomID, since = '') {
  const all = [];
  for (;;) {
    let path = `/v1/rooms/${encodeURIComponent(roomID)}/messages?limit=${PAGE}`;
    if (since) path += '&since=' + encodeURIComponent(since);
    const page = (await api('GET', path)).messages || [];
    all.push(...page);
    if (page.length < PAGE) return all;
    since = page[page.length - 1].ts;
  }
}

const replyTo = m => (m.metadata && Number(m.metadata.reply_to)) || 0;

// unanswered: requests no other key has replied to, as `conclave-mcp status`.
function unanswered(msgs) {
  const author = new Map(msgs.map(m => [m.id, m.author_key_id]));
  const answered = new Set();
  for (const m of msgs) {
    const to = replyTo(m);
    if (to && author.get(to) !== m.author_key_id) answered.add(to);
  }
  const requests = ['question', 'task_assignment', 'handoff', 'alert', 'decision'];
  return msgs.filter(m => requests.includes(m.message_type) && !answered.has(m.id));
}

const extMark = trust => trust === 'external' ? h('span', { class: 'ext' }, 'EXTERNAL') : '';

export async function roomsPage(view) {
  const rooms = (await api('GET', '/v1/rooms')).rooms || [];
  const dayAgo = Date.now() - 24 * 3600 * 1000;
  const rows = await Promise.all(rooms.map(async r => {
    const msgs = await listMessages(r.id);
    const open = unanswered(msgs).filter(m => Date.parse(m.ts) > dayAgo).length;
    return h('tr', {},
      h('td', {}, h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name)),
      h('td', {}, String(open)),
      h('td', {}, msgs.length ? when(msgs[msgs.length - 1].ts) : 'no messages'));
  }));
  const name = h('input', { required: true, maxlength: 128 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Rooms'),
    rooms.length ? table(['Room', 'Open requests (24 h)', 'Last activity'], rows) : h('p', {}, 'No rooms yet.'),
    h('h2', {}, 'Create a room'),
    h('form', {
      onsubmit: async e => {
        e.preventDefault();
        msg.replaceChildren();
        try {
          const room = await api('POST', '/v1/rooms', { name: name.value });
          location.hash = '#/rooms/' + encodeURIComponent(room.id);
        } catch (err) { msg.replaceChildren(errorBox(err)); }
      },
    }, h('label', {}, 'Name ', name), h('button', {}, 'Create')),
    msg);
}

export async function roomPage(view, id) {
  const base = '/v1/rooms/' + encodeURIComponent(id);
  const [room, members, held, keys] = await Promise.all([
    api('GET', base), api('GET', base + '/members'), api('GET', base + '/claims'), api('GET', '/v1/keys')]);
  const allKeys = keys.keys || [];
  const labels = new Map(allKeys.map(k => [k.id, k.label]));
  const who = m => labels.get(m.author_key_id) || `key ${m.author_key_id}`;
  const msgs = new Map();
  for (const m of await listMessages(id)) msgs.set(m.id, m);

  const list = h('div');
  const requests = h('div');
  const opened = new Set(); // threads the operator expanded

  // rootOf follows reply_to up to the oldest message still present.
  function rootOf(m) {
    for (let i = 0; i < msgs.size; i++) {
      const p = replyTo(m);
      if (!p || !msgs.has(p)) return m.id;
      m = msgs.get(p);
    }
    return m.id;
  }

  function messageEl(m) {
    const ext = m.trust_origin === 'external';
    const meta = Object.assign({}, m.metadata);
    delete meta.reply_to;
    const metaText = Object.keys(meta).length ? JSON.stringify(meta) : '';
    const content = [metaText ? h('pre', { class: 'muted' }, metaText) : '', h('pre', {}, m.content)];
    return h('div', { class: 'message' },
      h('div', { class: 'meta' },
        [`#${m.id}`, when(m.ts), who(m)].map(t => h('span', {}, t)), extMark(m.trust_origin),
        m.message_type && m.message_type !== 'system' ? h('span', {}, `<${m.message_type}>`) : '',
        replyTo(m) ? h('span', {}, `re #${replyTo(m)}`) : '',
        confirmButton('Delete', 'Delete this message?', async () => {
          await api('DELETE', `${base}/messages/${m.id}`);
          msgs.delete(m.id);
          draw();
        })),
      ext
        ? h('div', { class: 'external' },
          h('div', { class: 'fence' }, `--- external content from ${who(m)} ---`),
          content,
          h('div', { class: 'fence' }, '--- end of external content ---'))
        : content);
  }

  function draw() {
    const all = [...msgs.values()].sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : a.id - b.id));
    const replies = new Map();
    for (const m of all) {
      const r = rootOf(m);
      if (r !== m.id) replies.set(r, [...(replies.get(r) || []), m]);
    }
    list.replaceChildren(...all.filter(m => rootOf(m) === m.id).map(m => {
      const el = messageEl(m);
      const thread = replies.get(m.id);
      if (thread) {
        const d = h('details', { open: opened.has(m.id) },
          h('summary', {}, `${thread.length} ${thread.length === 1 ? 'reply' : 'replies'}`),
          thread.map(messageEl));
        d.addEventListener('toggle', () => (d.open ? opened.add(m.id) : opened.delete(m.id)));
        el.append(d);
      }
      return el;
    }));
    if (!all.length) list.replaceChildren(h('p', {}, 'No messages.'));
    const open = unanswered(all);
    requests.replaceChildren(...(open.length
      ? open.map(m => h('p', {}, `#${m.id} · ${when(m.ts)} · ${who(m)} <${m.message_type}> `, extMark(m.trust_origin), ' ',
        m.content.length > 120 ? m.content.slice(0, 120) + '…' : m.content))
      : [h('p', {}, 'None.')]));
  }

  draw();
  view.replaceChildren(
    h('h1', {}, room.name),
    room.archived_at ? h('p', { class: 'muted' }, 'Archived ' + when(room.archived_at)) : '',
    h('h2', {}, 'Open requests'), requests,
    h('h2', {}, 'Messages'), list,
    h('h2', {}, 'Active claims'), claimsTable(held.claims || []),
    h('h2', {}, 'Members'), membersSection(base, members.members || [], allKeys),
    room.archived_at ? '' : h('p', {}, confirmButton('Archive room', 'Archive this room? Agents can no longer write to it.', async () => {
      await api('POST', base + '/archive');
      location.hash = '#/';
    })));
  return room.archived_at ? null : follow(base, id, msgs, draw);
}

function claimsTable(claims) {
  if (!claims.length) return h('p', {}, 'None.');
  return table(['Path', 'Held by', 'Until', 'Note'], claims.map(c => h('tr', {},
    h('td', {}, c.path), h('td', {}, c.label, ' ', extMark(c.trust_origin)),
    h('td', {}, when(c.expires_at)), h('td', {}, c.note || ''))));
}

function membersSection(base, members, keys) {
  const rows = members.map(m => {
    const picker = rightsPicker(m.rights || []);
    const msg = h('span');
    return h('tr', {},
      h('td', {}, m.label), h('td', {}, m.trust_origin, ' ', extMark(m.trust_origin)),
      h('td', {}, picker.el, ' ',
        h('button', {
          onclick: async () => {
            msg.replaceChildren();
            try {
              await api('PATCH', `${base}/members/${m.key_id}`, { rights: picker.value() });
              msg.replaceChildren('Saved.');
            } catch (err) { msg.replaceChildren(errorBox(err)); }
          },
        }, 'Save rights'), ' ', msg),
      h('td', {}, confirmButton('Remove', `Remove ${m.label} from this room?`, async () => {
        await api('DELETE', `${base}/members/${m.key_id}`);
        rerender();
      })));
  });
  const candidates = keys.filter(k => !k.revoked_at && !members.some(m => m.key_id === k.id));
  const key = h('select', {}, candidates.map(k => h('option', { value: k.id }, `${k.label} (${k.trust_origin})`)));
  const picker = rightsPicker();
  const multi = h('input', { type: 'checkbox' });
  const msg = h('div');
  return h('div', {},
    members.length ? table(['Key', 'Trust', 'Rights', ''], rows) : h('p', {}, 'No members.'),
    h('h2', {}, 'Invite a key'),
    candidates.length
      ? h('form', {
        onsubmit: async e => {
          e.preventDefault();
          msg.replaceChildren();
          try {
            await api('POST', `${base}/invite`, { key_id: Number(key.value), rights: picker.value(), allow_multi_room: multi.checked });
            rerender();
          } catch (err) { msg.replaceChildren(errorBox(err)); }
        },
      }, h('label', {}, 'Key ', key), picker.el,
      h('label', {}, multi, ' even if it is in another room'), h('button', {}, 'Invite'))
      : h('p', {}, 'Every active key is a member. Create one on the Keys page.'),
    msg);
}

// follow reads the room's event stream with fetch (EventSource cannot
// send the X-Conclave-UI header) and reconnects until stopped. On every
// (re)connect it loads what arrived while it was not listening.
function follow(base, id, msgs, draw) {
  const stop = new AbortController();
  const add = list => { for (const m of list) msgs.set(m.id, m); draw(); };
  (async () => {
    while (!stop.signal.aborted) {
      try {
        const res = await fetch(base + '/events?wait=300', { headers: { 'X-Conclave-UI': '1' }, signal: stop.signal });
        if (!res.ok) return; // logged out (401) or archived (410)
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
        let buf = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += value;
          let end;
          while ((end = buf.indexOf('\n\n')) >= 0) {
            const event = buf.slice(0, end);
            buf = buf.slice(end + 2);
            if (event.startsWith(': ok')) { // subscribed: catch up
              const last = [...msgs.values()].reduce((ts, m) => (m.ts > ts ? m.ts : ts), '');
              add(await listMessages(id, last));
            } else if (event.startsWith('event: message.created')) {
              const data = event.split('\n').find(l => l.startsWith('data: '));
              if (data) add([JSON.parse(data.slice(6))]);
            }
          }
        }
      } catch {
        if (stop.signal.aborted) return;
        await new Promise(r => setTimeout(r, 3000));
      }
    }
  })();
  return () => stop.abort();
}
```

- [ ] **Step 3: Route to the pages in `app.js`**

In `internal/ui/assets/app.js`, after `import { h, api, errorBox, confirmButton, table, when } from './lib.js';` add

```js
import { roomsPage, roomPage } from './rooms.js';
```

and replace

```js
const routes = [
  [/^#\/passkeys$/, passkeysPage],
];
```

with

```js
const routes = [
  [/^#\/?$/, roomsPage],
  [/^#\/rooms\/([^/]+)$/, roomPage],
  [/^#\/passkeys$/, passkeysPage],
];
```

- [ ] **Step 4: Check the scripts**

Run: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
Expected: `ok`.

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ -run TestAssets -v`
Expected: `--- PASS: TestAssets_NoMarkupFromData`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/assets
git commit -m "feat(ui): rooms pages with live messages, requests, claims and members" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Frontend: keys and the audit log

**Files:**
- Create: `internal/ui/assets/keys.js`
- Modify: `internal/ui/assets/lib.js` (append), `internal/ui/assets/app.js` (one import, two routes)

**Interfaces:**
- Consumes: `lib.js` (Tasks 5, 6); `/v1` endpoints `GET/POST /v1/keys`, `DELETE /v1/keys/{id}`, `GET /v1/rooms`, `GET .../members`, `POST .../invite`, `GET /v1/audit?actor=&action=&room_id=&since=&limit=` (newest first).
- Produces `lib.js` export: `showSecret(view, title, secret)` (the secret once, a Copy button, Done re-renders).
- Produces `keys.js` exports: `keysPage(view)` (`#/keys`: id, label, trust, active rooms, created, revoked; create key; create agent = `POST /v1/keys` then `POST .../invite`, revoking the key again if the invite fails, like `agent add`; revoke, except for the key labelled `operator`), `auditPage(view)` (`#/audit`: filters actor, action prefix, room id, since).

- [ ] **Step 1: Append to `lib.js`**

Append to `internal/ui/assets/lib.js`:

```js
// showSecret replaces the page in view with a secret shown only once.
export function showSecret(view, title, secret) {
  const copy = h('button', {
    onclick: async () => {
      await navigator.clipboard.writeText(secret);
      copy.textContent = 'Copied';
    },
  }, 'Copy');
  view.replaceChildren(h('h1', {}, title),
    h('div', { class: 'secret' }, h('pre', {}, secret), copy),
    h('p', {}, 'Shown once: the server keeps only a hash of the key.'),
    h('button', { onclick: rerender }, 'Done'));
}
```

- [ ] **Step 2: Write `keys.js`**

`internal/ui/assets/keys.js`:

```js
// Keys (create, create an agent, revoke) and the audit log.
import { h, api, errorBox, confirmButton, table, when, rerender, rightsPicker, showSecret } from './lib.js';

function trustSelect() {
  return h('select', {},
    h('option', { value: 'external' }, 'external (default)'),
    h('option', { value: 'internal' }, 'internal (your own agents)'));
}

// submit makes a form handler that shows errors in msg.
function submit(msg, fn) {
  return async e => {
    e.preventDefault();
    msg.replaceChildren();
    try { await fn(); } catch (err) { msg.replaceChildren(errorBox(err)); }
  };
}

export async function keysPage(view) {
  const [keys, rooms] = await Promise.all([api('GET', '/v1/keys'), api('GET', '/v1/rooms')]);
  const allKeys = keys.keys || [];
  const allRooms = rooms.rooms || [];
  const inRooms = new Map(); // key id -> names of its (active) rooms
  await Promise.all(allRooms.map(async r => {
    for (const m of (await api('GET', `/v1/rooms/${encodeURIComponent(r.id)}/members`)).members || []) {
      inRooms.set(m.key_id, [...(inRooms.get(m.key_id) || []), r.name]);
    }
  }));

  const rows = allKeys.map(k => h('tr', {},
    h('td', {}, String(k.id)), h('td', {}, k.label), h('td', {}, k.trust_origin),
    h('td', {}, (inRooms.get(k.id) || []).join(', ')),
    h('td', {}, when(k.created_at)), h('td', {}, when(k.revoked_at)),
    h('td', {}, k.revoked_at || k.label === 'operator' ? '' // the UI itself acts as operator
      : confirmButton('Revoke', `Revoke ${k.label}? Whoever uses it loses access at once.`, async () => {
        await api('DELETE', `/v1/keys/${k.id}`);
        rerender();
      }))));

  const label = h('input', { required: true, maxlength: 128 });
  const trust = trustSelect();
  const keyMsg = h('div');
  const keyForm = h('form', {
    onsubmit: submit(keyMsg, async () => {
      const created = await api('POST', '/v1/keys', { label: label.value, trust_origin: trust.value });
      showSecret(view, `Key ${created.label} created`, created.key);
    }),
  }, h('label', {}, 'Label ', label), h('label', {}, 'Trust ', trust), h('button', {}, 'Create key'));

  const agent = h('input', { required: true, maxlength: 128 });
  const agentTrust = trustSelect();
  const room = h('select', {}, allRooms.map(r => h('option', { value: r.id }, r.name)));
  const rights = rightsPicker();
  const agentMsg = h('div');
  const agentForm = h('form', {
    onsubmit: submit(agentMsg, async () => {
      const created = await api('POST', '/v1/keys', { label: agent.value, trust_origin: agentTrust.value });
      try {
        await api('POST', `/v1/rooms/${encodeURIComponent(room.value)}/invite`, { key_id: created.id, rights: rights.value() });
      } catch (err) {
        await api('DELETE', `/v1/keys/${created.id}`); // no key that can reach nothing
        throw err;
      }
      showSecret(view, `Agent ${created.label} created`, `export CONCLAVE_API_KEY=${created.key}`);
    }),
  }, h('label', {}, 'Label ', agent), h('label', {}, 'Trust ', agentTrust), h('label', {}, 'Room ', room),
  rights.el, h('button', {}, 'Create agent'));

  view.replaceChildren(h('h1', {}, 'Keys'),
    table(['Id', 'Label', 'Trust', 'Rooms', 'Created', 'Revoked', ''], rows),
    h('h2', {}, 'Create a key'), keyForm, keyMsg,
    h('h2', {}, 'Create an agent'),
    h('p', { class: 'muted' }, 'A key plus an invitation with these rights, like conclave-mcp agent add.'),
    allRooms.length ? agentForm : h('p', {}, 'Create a room first.'), agentMsg);
}

export async function auditPage(view) {
  const allKeys = (await api('GET', '/v1/keys')).keys || [];
  const labels = new Map(allKeys.map(k => [k.id, k.label]));
  const actor = h('select', {}, h('option', { value: '' }, 'anyone'), allKeys.map(k => h('option', { value: k.id }, k.label)));
  const action = h('input', { placeholder: 'e.g. room. or ui.login' });
  const room = h('input', { placeholder: 'room id' });
  const since = h('input', { type: 'datetime-local' });
  const out = h('div');

  async function load() {
    const q = new URLSearchParams({ limit: '500' });
    if (actor.value) q.set('actor', actor.value);
    if (action.value.trim()) q.set('action', action.value.trim());
    if (room.value.trim()) q.set('room_id', room.value.trim());
    if (since.value) q.set('since', new Date(since.value).toISOString());
    try {
      const entries = (await api('GET', '/v1/audit?' + q)).entries || [];
      out.replaceChildren(entries.length
        ? table(['Time', 'Actor', 'Action', 'Target', 'Room', 'Result', 'Detail'], entries.map(e => h('tr', {},
          h('td', {}, when(e.ts)),
          h('td', {}, e.actor_key_id ? labels.get(e.actor_key_id) || `key ${e.actor_key_id}` : 'system'),
          h('td', {}, e.action), h('td', {}, `${e.target_type} ${e.target_id}`),
          h('td', {}, e.room_id || ''), h('td', {}, e.result), h('td', {}, h('code', {}, e.detail || '')))))
        : h('p', {}, 'No entries.'));
    } catch (err) { out.replaceChildren(errorBox(err)); }
  }

  view.replaceChildren(h('h1', {}, 'Audit log'),
    h('form', { onsubmit: e => { e.preventDefault(); load(); } },
      h('label', {}, 'Actor ', actor), h('label', {}, 'Action ', action),
      h('label', {}, 'Room ', room), h('label', {}, 'Since ', since), h('button', {}, 'Filter')),
    out);
  await load();
}
```

- [ ] **Step 3: Route to the pages in `app.js`**

In `internal/ui/assets/app.js`, after `import { roomsPage, roomPage } from './rooms.js';` add

```js
import { keysPage, auditPage } from './keys.js';
```

and replace

```js
  [/^#\/rooms\/([^/]+)$/, roomPage],
  [/^#\/passkeys$/, passkeysPage],
```

with

```js
  [/^#\/rooms\/([^/]+)$/, roomPage],
  [/^#\/keys$/, keysPage],
  [/^#\/audit$/, auditPage],
  [/^#\/passkeys$/, passkeysPage],
```

The finished `app.js`:

```js
// The page: routing, the header, passkey login and setup, passkeys.
import { h, api, errorBox, confirmButton, table, when } from './lib.js';
import { roomsPage, roomPage } from './rooms.js';
import { keysPage, auditPage } from './keys.js';

const main = document.getElementById('app');
const nav = document.getElementById('nav');

// registerPasskey runs a registration ceremony: with a setup token for
// the first passkey, else as the logged-in operator.
async function registerPasskey(name, setupToken = '') {
  const options = await api('POST', '/ui/auth/register/begin', { name, setup_token: setupToken });
  const cred = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options) });
  await api('POST', '/ui/auth/register/finish', cred.toJSON());
}

async function login() {
  const options = await api('POST', '/ui/auth/login/begin');
  const cred = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options) });
  await api('POST', '/ui/auth/login/finish', cred.toJSON());
}

// action makes a click handler that shows errors in msg.
function action(msg, fn) {
  return async () => {
    msg.replaceChildren();
    try { await fn(); } catch (err) { msg.replaceChildren(errorBox(err)); }
  };
}

function loginPage(view, state) {
  if (state.setup_needed) {
    view.replaceChildren(h('h1', {}, 'Set up the web UI'),
      h('p', {}, 'No passkey is registered yet. With the admin key, run'),
      h('pre', {}, 'conclave-mcp ui setup'),
      h('p', {}, 'and open the URL it prints in this browser.'));
    return;
  }
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Log in'),
    h('button', { onclick: action(msg, async () => { await login(); await render(); }) }, 'Log in with a passkey'),
    msg);
}

function setupPage(view, token) {
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Register your passkey'),
    h('p', {}, 'The passkey (Touch ID or another authenticator on this device) is how you log in to this server from now on.'),
    h('label', {}, 'Name ', name), ' ',
    h('button', {
      onclick: action(msg, async () => {
        await registerPasskey(name.value, token);
        history.replaceState(null, '', '#/'); // drop the used token from the address bar
        await render();
      }),
    }, 'Register passkey'),
    msg);
}

async function passkeysPage(view) {
  const { credentials } = await api('GET', '/ui/auth/credentials');
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Passkeys'),
    table(['Name', 'Added', 'Last used', ''], credentials.map(c => h('tr', {},
      h('td', {}, c.name), h('td', {}, when(c.created_at)), h('td', {}, when(c.last_used_at)),
      h('td', {}, credentials.length > 1
        ? confirmButton('Remove', 'Remove this passkey?', async () => {
          await api('DELETE', '/ui/auth/credentials/' + encodeURIComponent(c.id));
          await render();
        })
        : h('span', { class: 'muted' }, 'the last passkey cannot be removed'))))),
    h('h2', {}, 'Add a passkey'),
    h('label', {}, 'Name ', name), ' ',
    h('button', { onclick: action(msg, async () => { await registerPasskey(name.value); await render(); }) }, 'Add passkey'),
    msg);
}

// header returns the navigation: pages, the rooms, logout.
async function header() {
  const { rooms } = await api('GET', '/v1/rooms');
  const msg = h('span');
  return [
    h('a', { href: '#/' }, 'Rooms'), h('a', { href: '#/keys' }, 'Keys'),
    h('a', { href: '#/audit' }, 'Audit'), h('a', { href: '#/passkeys' }, 'Passkeys'),
    h('span', { class: 'rooms' }, (rooms || []).map(r => h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name))),
    msg,
    h('button', { onclick: action(msg, async () => { await api('POST', '/ui/auth/logout'); await render(); }) }, 'Log out'),
  ];
}

// routes maps the address after # to a page. A page fills the element
// it gets and may return a function that stops what it started (the
// live stream of a room).
const routes = [
  [/^#\/?$/, roomsPage],
  [/^#\/rooms\/([^/]+)$/, roomPage],
  [/^#\/keys$/, keysPage],
  [/^#\/audit$/, auditPage],
  [/^#\/passkeys$/, passkeysPage],
];

let leave = null; // stops the shown page's live stream
let seq = 0; // the newest render; an older one that finishes late shows nothing

async function render() {
  if (leave) { leave(); leave = null; }
  const mine = ++seq;
  const view = h('div');
  const show = (links = []) => {
    if (mine !== seq) return false;
    nav.replaceChildren(...links);
    main.replaceChildren(view);
    return true;
  };
  const hash = location.hash || '#/';
  try {
    const setup = /^#setup=(.+)$/.exec(hash);
    if (setup) {
      setupPage(view, setup[1]);
      return show();
    }
    const state = await api('GET', '/ui/auth/state');
    if (!state.logged_in) {
      loginPage(view, state);
      return show();
    }
    const links = await header();
    const route = routes.find(([re]) => re.test(hash));
    if (!route) {
      view.replaceChildren(h('h1', {}, 'Not found'), h('p', {}, h('a', { href: '#/' }, 'Back to the rooms')));
      return show(links);
    }
    const stop = await route[1](view, ...route[0].exec(hash).slice(1).map(decodeURIComponent));
    if (show(links)) leave = stop || null;
    else if (stop) stop();
  } catch (err) {
    if (err.status === 401) { // the session ended: idle, expired or logged out elsewhere
      try { loginPage(view, await api('GET', '/ui/auth/state')); } catch (e) { view.replaceChildren(errorBox(e)); }
    } else {
      view.replaceChildren(errorBox(err));
    }
    show();
  }
}

window.addEventListener('hashchange', render);
render();
```

- [ ] **Step 4: Check the scripts**

Run: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
Expected: `ok`.

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./internal/ui/ -run TestAssets -v`
Expected: `--- PASS: TestAssets_NoMarkupFromData`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/assets
git commit -m "feat(ui): keys and audit pages" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: CLI: `ui setup` and `admin ui-reset`

**Files:**
- Create: `cmd/ui/ui.go`, `cmd/ui/ui_test.go`
- Modify: `cmd/admin/admin.go` (imports ~line 32, `NewCmd` ~lines 36–45, append), `cmd/admin/admin_test.go` (imports, append), `cmd/conclave-mcp/main.go` (imports, `newRootCmd`, `main`)

**Interfaces:**
- Consumes: `POST /v1/ui/setup-tokens` (Task 3); `audit.Write`; the migration 0009 tables.
- Produces: package `cmd/ui`: `NewCmd() *cobra.Command` (`ui setup`), `ExitCode(err error) int` (1 server refusal, 2 usage/client, 3 network). `ui setup` prints `http://localhost:<port>/ui/#setup=<token>` (port of the server URL) on stdout, the expiry on stderr.
- Produces in `cmd/admin`: `newUIResetCmd()` (`admin ui-reset [--db PATH]`, path resolved like `vacuum`), `runUIReset(ctx context.Context, errOut io.Writer, dbPath string) error`: in one transaction deletes all `ui_credentials` and `ui_sessions` and renames a revoked `operator` key to `operator-revoked-<id>`; then audit `ui.reset` (actor NULL, detail `{"passkeys":N,"sessions":M}`); exit 1 if the file is missing.

- [ ] **Step 1: Write the failing tests**

`cmd/ui/ui_test.go`:

```go
package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// runSetup runs ui setup against a stub server and returns stdout,
// stderr, the stub's port and the error.
func runSetup(t *testing.T, h http.HandlerFunc) (string, string, string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := openClient
	openClient = func(*cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "admin"})
	}
	t.Cleanup(func() { openClient = old })
	var out, errOut bytes.Buffer
	cmd := newSetupCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	u, _ := url.Parse(srv.URL)
	return out.String(), errOut.String(), u.Port(), err
}

func TestSetup_PrintsTheLocalhostURL(t *testing.T) {
	out, errOut, port, err := runSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/ui/setup-tokens" || r.Header.Get("Authorization") != "Bearer admin" {
			t.Errorf("request %s %s (%q)", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"abc_DEF-123","expires_at":"2026-11-12T10:10:00.000000000Z"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://localhost:" + port + "/ui/#setup=abc_DEF-123\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
	if !strings.Contains(errOut, "works once") {
		t.Errorf("stderr: %q", errOut)
	}
}

func TestSetup_ServerRefusal(t *testing.T) {
	_, _, _, err := runSetup(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"a passkey is registered"}}`))
	})
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "a passkey is registered") {
		t.Errorf("err %v (exit %d), want the server's message and exit 1", err, ExitCode(err))
	}
}
```

In `cmd/admin/admin_test.go` add to the imports, after `_ "modernc.org/sqlite"`:

```go

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
```

and append:

```go
func TestUIReset(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, closeDB, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys (label, hash, prefix, created_at, is_admin, revoked_at) VALUES ('operator', 'x', 'p1', '2026-11-12T10:00:00Z', 1, '2026-11-12T11:00:00Z')`,
		`INSERT INTO ui_credentials (id, public_key, created_at) VALUES ('c1', x'01', '2026-11-12T10:00:00Z'), ('c2', x'02', '2026-11-12T10:00:00Z')`,
		`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES ('h', '2026-11-12T10:00:00Z', '2026-11-12T10:00:00Z', '2026-11-12T22:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = closeDB()

	var errOut bytes.Buffer
	if err := runUIReset(ctx, &errOut, dbPath); err != nil {
		t.Fatalf("runUIReset: %v", err)
	}
	if !strings.Contains(errOut.String(), "removed 2 passkey(s) and 1 session(s)") {
		t.Errorf("stderr: %q", errOut.String())
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var creds, sessions int
	var label, detail string
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM ui_credentials), (SELECT COUNT(*) FROM ui_sessions), (SELECT label FROM api_keys WHERE prefix = 'p1')`).Scan(&creds, &sessions, &label); err != nil {
		t.Fatal(err)
	}
	if creds != 0 || sessions != 0 || label != "operator-revoked-1" {
		t.Errorf("after reset: %d passkeys, %d sessions, label %q; want 0, 0, operator-revoked-1", creds, sessions, label)
	}
	if err := db.QueryRow(`SELECT detail FROM audit WHERE action = 'ui.reset' AND actor_key_id IS NULL`).Scan(&detail); err != nil || detail != `{"passkeys":2,"sessions":1}` {
		t.Errorf("audit detail %q (%v)", detail, err)
	}
}

func TestUIReset_MissingDBIsExit1(t *testing.T) {
	err := runUIReset(context.Background(), &bytes.Buffer{}, filepath.Join(t.TempDir(), "missing.db"))
	if ExitCode(err) != 1 {
		t.Errorf("missing db: %v (exit %d), want exit 1", err, ExitCode(err))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./cmd/ui/ ./cmd/admin/`
Expected: FAIL, build failed: `undefined: openClient`, `undefined: newSetupCmd` (cmd/ui); `undefined: runUIReset` (cmd/admin).

- [ ] **Step 3: Write `cmd/ui/ui.go`**

```go
// Package ui implements `conclave-mcp ui setup`: it asks the running
// server for a one-time setup token and prints the URL that registers
// the operator's first passkey (docs/web-ui.md).
package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the ui subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Web admin UI setup",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newSetupCmd())
	return cmd
}

// openClient is a seam for tests.
var openClient = func(cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Print the URL that registers the first passkey (admin key, server running)",
		Long: `Asks the running server for a setup token (admin key required) and
prints http://localhost:<port>/ui/#setup=<token> on stdout. Open it in a
browser on the machine the server runs on, within 10 minutes, and
register a passkey; the link works once. The token is in the URL
fragment, which the browser does not send to the server or to proxies.

Once a passkey exists, the server refuses new setup tokens: add more
passkeys on the UI's Passkeys page, or start over with
conclave-mcp admin ui-reset (service stopped).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
			}
			return run(cmd.Context(), cmd, c)
		},
	}
}

func run(ctx context.Context, cmd *cobra.Command, c *client.Client) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Port() == "" {
		return &exitError{Code: 2, Err: fmt.Errorf("server URL %q: want http://<host>:<port>", c.BaseURL)}
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/ui/setup-tokens", Out: &resp}); err != nil {
		var netErr *client.NetError
		if errors.As(err, &netErr) {
			return &exitError{Code: 3, Err: err}
		}
		return &exitError{Code: 1, Err: err}
	}
	until := resp.ExpiresAt
	if t, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt); err == nil {
		until = t.Local().Format("15:04")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "http://localhost:%s/ui/#setup=%s\n", u.Port(), resp.Token)
	fmt.Fprintf(cmd.ErrOrStderr(), "open this URL in a browser on this machine before %s and register a passkey; it works once\n", until)
	return nil
}

// exitError mirrors cmd/rooms.
type exitError struct {
	Code int
	Err  error
}

func (e *exitError) Error() string { return e.Err.Error() }
func (e *exitError) Unwrap() error { return e.Err }

// ExitCode is exported so cmd/conclave-mcp can use it.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 2
}
```

- [ ] **Step 4: Add `admin ui-reset`**

In `cmd/admin/admin.go` add the import `"github.com/hermes/conclave-mcp/internal/audit"` before `"github.com/hermes/conclave-mcp/internal/auth"`. In `NewCmd` replace `Short: "Offline maintenance (vacuum, key bootstrap)",` with `Short: "Offline maintenance (vacuum, key bootstrap, web UI reset)",` and after `cmd.AddCommand(newKeysCmd())` add `cmd.AddCommand(newUIResetCmd())`. Append to the file:

```go
// ---------- admin ui-reset ----------
//
// The way back in after losing every passkey. Offline like
// create-bootstrap: whoever can stop the service and write the
// database file is the operator already.

func newUIResetCmd() *cobra.Command {
	var flagDB string
	cmd := &cobra.Command{
		Use:   "ui-reset",
		Short: "Remove all web UI passkeys and sessions (offline; stop the server first)",
		Long: `Deletes every passkey and every session of the web UI directly in the
SQLite file and records ui.reset in the audit log. Then start the
server and run conclave-mcp ui setup to register a passkey again.

A revoked operator key is renamed operator-revoked-<id>, so that the
next setup creates a fresh one.

Like admin vacuum, this subcommand does NOT talk to a running server.
Stop the server first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbPath, _ := resolveVacuumPaths(flagDB)
			return runUIReset(cmd.Context(), cmd.ErrOrStderr(), dbPath)
		},
	}
	cmd.Flags().StringVar(&flagDB, "db", "", "path to the SQLite file (overrides CONCLAVE_DATA_DIR)")
	return cmd
}

func runUIReset(ctx context.Context, errOut io.Writer, dbPath string) error {
	// sql.Open would create a missing file; a typo must not.
	if _, err := os.Stat(dbPath); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("stat db: %w", err)}
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("open db: %w", err)}
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("begin tx: %w", err)}
	}
	defer tx.Rollback()
	var counts [2]int64
	for i, q := range []string{`DELETE FROM ui_credentials`, `DELETE FROM ui_sessions`} {
		res, err := tx.ExecContext(ctx, q)
		if err != nil {
			return &exitError{Code: 1, Err: fmt.Errorf("%s: %w (has the server run this version once?)", q, err)}
		}
		counts[i], _ = res.RowsAffected()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE api_keys SET label = 'operator-revoked-' || id WHERE label = 'operator' AND revoked_at IS NOT NULL`); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("rename revoked operator key: %w", err)}
	}
	if err := tx.Commit(); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("commit: %w", err)}
	}
	detail, _ := json.Marshal(map[string]int64{"passkeys": counts[0], "sessions": counts[1]})
	if err := audit.Write(ctx, db, 0, "ui.reset", "ui", "", "", "ok", string(detail)); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("audit: %w", err)}
	}
	fmt.Fprintf(errOut, "removed %d passkey(s) and %d session(s); start the server and run conclave-mcp ui setup\n", counts[0], counts[1])
	return nil
}
```

- [ ] **Step 5: Register `ui` in the root command**

In `cmd/conclave-mcp/main.go` add the import (after `cmd/setup`):

```go
	uicli "github.com/hermes/conclave-mcp/cmd/ui"
```

after `root.AddCommand(messages.NewWatchCmd())` add `root.AddCommand(uicli.NewCmd())`, and in `main` after the `setup.ExitCode` block add

```go
	if code == 0 || code == 2 {
		if c := uicli.ExitCode(err); c == 1 || c == 3 {
			code = c
		}
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./cmd/...`
Expected: every package `ok`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 7: Commit**

```bash
git add cmd/ui cmd/admin cmd/conclave-mcp/main.go
git commit -m "feat(cli): ui setup and admin ui-reset" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Docs

**Files:**
- Create: `docs/web-ui.md`
- Modify: `README.md`, `docs/cli-reference.md`, `docs/architecture.md`, `docs/DOCS-PLAN.md`, `CHANGELOG.md`

**Interfaces:**
- Consumes: the behaviour of Tasks 1–8 (names, endpoints, timeouts, audit actions, commands).
- Produces: `docs/web-ui.md` (setup, reset, security model, audit actions, what the UI does not do) and the links to it.

- [ ] **Step 1: Write `docs/web-ui.md`**

````markdown
# Web admin UI

The web UI manages Conclave from a browser on the machine the server
runs on: rooms, members and their rights, keys, the audit log, and what
the agents write in each room, live. It calls the same `/v1` endpoints
as the CLI, so every permission rule is the same. Sending messages from
the UI is not part of it yet.

Login uses a passkey (Touch ID or another authenticator of the device).
There is no password, so there is nothing an agent could read from a
file or a key store.

## Requirements

- The server listens on a loopback address: `--host 127.0.0.1` (the
  default), `localhost` or `::1`. On any other address `/ui` answers 404
  and the session cookie counts for nothing: passkeys need a secure
  context, and plain HTTP is one only on `localhost`.
- A current browser on the same machine (Safari 18, Chrome 129,
  Firefox 119 or newer).

## Setup

1. With the server running and the admin key in `CONCLAVE_API_KEY`:

   ```sh
   conclave-mcp ui setup
   ```

   It prints `http://localhost:7432/ui/#setup=<token>`. The token sits in
   the URL fragment, which the browser does not send to the server or to
   a proxy.
2. Open the URL in a browser on this machine within 10 minutes and
   register a passkey. The link works once. Registering logs you in.
3. Later, open `http://localhost:7432/ui/` and log in with the passkey.
   `http://127.0.0.1:7432/ui/` redirects there: a passkey belongs to
   `localhost`, and an IP address cannot hold one.

Do the setup right after upgrading: until the first passkey exists, any
holder of the admin key can ask for a setup token.

Add a second passkey (a security key, say) on the Passkeys page. The
last passkey cannot be removed there.

## Lost every passkey

Stop the service, then run

```sh
conclave-mcp admin ui-reset --db /path/to/data.db
```

It deletes all passkeys and sessions and records `ui.reset` in the
audit log. Start the service and run `conclave-mcp ui setup` again.
Whoever can stop the service and write the database file is the
operator already, as with `admin keys create-bootstrap`.

## Security model

| Topic | How |
|---|---|
| Who the UI acts as | An admin key labelled `operator`, created by the first registration. Its secret is random and never shown or stored in plain text, so only UI sessions act as it. The audit log shows UI actions as `operator`, apart from CLI use of the bootstrap admin key. |
| Trust anchor | The first passkey needs a setup token (10 minutes, single use) and is accepted only while no passkey exists. Later passkeys can only be added from a logged-in session, so an agent holding the admin key cannot add its own. `POST /v1/ui/setup-tokens` answers 409 once a passkey exists. |
| Passkeys | WebAuthn with user verification required, discoverable credentials, no attestation. The server stores public keys only. |
| Session | Cookie `conclave_ui`: 32 random bytes, `HttpOnly`, `SameSite=Strict`, `Path=/`, not `Secure` (Safari rejects `Secure` cookies on `http://localhost`). The database keeps only its SHA-256. A session ends after 1 hour without use and 12 hours after login; logout deletes it. |
| `/v1` with the cookie | Accepted only with the header `X-Conclave-UI: 1` (a site cannot send it cross-origin without CORS, which the server does not allow), a `Host` of `localhost:<port>` or `127.0.0.1:<port>` (against DNS rebinding) and, for anything but GET, the `Origin` `http://localhost:<port>`. `/v1/mcp` takes bearer keys only. |
| Pages | `Content-Security-Policy: default-src 'self'; base-uri 'none'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`. The scripts put message text, labels and every other value into the page as text, never as markup. External messages appear in a marked block between `--- external content from <label> ---` and `--- end of external content ---`, as in the CLI. |
| Revoking `operator` | Locks the UI out of `/v1` at once. To use the UI again: `admin ui-reset` (it renames a revoked `operator` key, so the next setup creates a fresh one), then `ui setup`. |

## Audit actions

| Action | When |
|---|---|
| `ui.setup_token` | `ui setup` got a setup token (actor: the admin key). |
| `ui.passkey.add` | A passkey was registered (`via`: `setup_token` or `session`). |
| `ui.passkey.remove` | A passkey was removed. |
| `ui.login`, `ui.logout` | A session started or ended. |
| `ui.reset` | `admin ui-reset` ran (how many passkeys and sessions it removed). |

## Not in the UI

- Sending messages. That needs a mark agents can verify as the
  operator's; it comes later.
- Offline commands (`admin vacuum`, `admin keys create-bootstrap`,
  `admin ui-reset`): they need the service stopped.
- `init`, hooks and `agent add --store`: they set up a repository or a
  local key store, not the server.
- Access from another machine.
````

- [ ] **Step 2: Update the other docs**

`README.md` (it must stay at or under 200 lines; with these 9 lines it has 196). After the paragraph that ends with `agent to it: [docs/agent-integration.md](docs/agent-integration.md).` (end of Quickstart) insert:

```markdown

## Web UI

Rooms, rights, keys, the audit log and the agents' messages (live) in
a browser on the same machine. With the server on a loopback address,
run `conclave-mcp ui setup` with the admin key, open the printed URL
and register a passkey (Touch ID or similar); then log in at
`http://localhost:7432/ui/`. See [docs/web-ui.md](docs/web-ui.md).
```

and in `## Documentation`, after `* [docs/agent-integration.md](docs/agent-integration.md) — wiring an agent.`, add `* [docs/web-ui.md](docs/web-ui.md) — the web admin UI.`

`docs/cli-reference.md`:
- Quick reference: after the `admin vacuum` line add
  `conclave-mcp admin ui-reset [--db PATH]               # offline: drop all web UI passkeys`
  and after the `agent add` line add
  `conclave-mcp ui setup                                 # URL that registers the web UI passkey`
- After the `admin keys create-bootstrap` section (it ends with "...naming the conflicting label.") insert:

````markdown
## `admin ui-reset`

```
conclave-mcp admin ui-reset [--db PATH]
```

Deletes every passkey and session of the web UI directly in the
SQLite file and records `ui.reset` in the audit log. A revoked
`operator` key is renamed `operator-revoked-<id>`, so that the next
setup creates a fresh one. Then start the server and run `ui setup`.
See [web-ui.md](web-ui.md).

| Flag | Default | Notes |
|---|---|---|
| `--db` | `${CONCLAVE_DATA_DIR}/data.db` (`./.conclave-mcp/data.db`) | Path to the SQLite file. |

**Stop the server first.**
````

- After the `audit list` section (it ends with "`ROOM`, `MESSAGE`, `METADATA`.") insert:

````markdown
## `ui setup`

```
conclave-mcp ui setup
```

Needs the admin key and a running server on a loopback address.
Prints the one-time URL that registers the web UI's first passkey,
`http://localhost:<port>/ui/#setup=<token>`, on STDOUT and how long
it works on STDERR. Open it in a browser on the same machine within
10 minutes. Once a passkey exists the server refuses
(`409 conflict`): add more passkeys on the UI's Passkeys page, or
start over with `admin ui-reset`. See [web-ui.md](web-ui.md).
````

- In `## See also`, after the `architecture.md` line add `* [docs/web-ui.md](web-ui.md) — the web admin UI.`

`docs/architecture.md`:
- Package layout: after `cmd/setup           init and agent add` add `cmd/ui              ui setup (asks the server for a setup URL)`; before `internal/agent      ...` add `internal/ui         web admin UI: embedded pages, passkey login, UI sessions on /v1`; change `migrations/         numbered SQL migrations (0001..0008)` to `(0001..0009)`.
- Data model: after the `audit_log(...)` entry (inside the same code block) add

```
ui_credentials(id, public_key, sign_count, transports, flags, name, created_at, last_used_at)
ui_sessions(token_hash, created_at, last_seen_at, expires_at)
ui_setup_tokens(token_hash, expires_at, used_at)
  the web UI (migration 0009): passkey public keys, and the SHA-256 of
  session and setup tokens
```

  (preceded by an empty line), and in the sentence after that block change `migrations/0001..0008` to `migrations/0001..0009`.
- HTTP API table: after the `/v1/audit` row add

```markdown
| `POST` | `/v1/ui/setup-tokens` | One-time token that registers the web UI's first passkey (409 once one exists). Loopback listen address only. | admin |
```

- Before `All endpoints return JSON. Errors are JSON of the form` insert the paragraph

```markdown
On a loopback listen address the server also serves the web UI
under `/ui/` and its passkey and session endpoints under
`/ui/auth/`; see [web-ui.md](web-ui.md).

```

- In `## Auth`, before `The first API key on a fresh install is created with` insert

```markdown
On a loopback listen address `/v1` (except `/v1/mcp`) also takes
the web UI's session cookie `conclave_ui`, together with the
header `X-Conclave-UI: 1`, a UI `Host` and, unless GET, the UI
`Origin`. Such a request acts as the admin key labelled
`operator` (`internal/ui`, [web-ui.md](web-ui.md)).

```

- In `## See also`, after the `trust-model.md` line add `* [docs/web-ui.md](web-ui.md) — the web admin UI.`

`docs/DOCS-PLAN.md`: after `* **docs/trust-model.md** — internal vs external, who classifies, consequences.` add `* **docs/web-ui.md** — the web admin UI: setup, reset, security model.`; after the table row `| docs/trust-model.md | ... |` add `| docs/web-ui.md | Operators using the web admin UI. |`.

`CHANGELOG.md`: replace `## [Unreleased]` (and the empty line after it) with

```markdown
## [Unreleased]

### Added

- Web admin UI at `http://localhost:<port>/ui/` when the server listens
  on a loopback address: rooms, members and rights, keys (create, create
  an agent, revoke), the audit log, and each room's messages with open
  requests, claims and live updates. Login with a passkey only (WebAuthn,
  user verification required); no password exists. `conclave-mcp ui
  setup` prints the one-time setup URL, `conclave-mcp admin ui-reset`
  (offline) removes all passkeys and sessions. The UI acts as a new admin
  key labelled `operator`. New endpoint `POST /v1/ui/setup-tokens`
  (admin), new audit actions `ui.setup_token`, `ui.passkey.add`,
  `ui.passkey.remove`, `ui.login`, `ui.logout`, `ui.reset`, migration
  0009. See `docs/web-ui.md`.

```

- [ ] **Step 3: Check**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./tests/docs/`
Expected: `ok` (README.md at most 200 lines; it has 196).

Run: `grep -rn 'ui-reset\|ui setup\|web-ui.md' README.md docs/cli-reference.md docs/architecture.md docs/DOCS-PLAN.md CHANGELOG.md | wc -l`
Expected: `21`.

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok`.
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 4: Commit**

```bash
git add docs/web-ui.md README.md docs/cli-reference.md docs/architecture.md docs/DOCS-PLAN.md CHANGELOG.md
git commit -m "docs: web admin UI" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: End to end with the real binary, browser smoke test

**Files:**
- Create: `tests/e2e/ui_test.go`

**Interfaces:**
- Consumes: everything above; e2e helpers `bootstrapKeyFor`, `portFromAddr`, `(*subprocessEnv).URL`, `.BinaryPath`, `.call`, `env.addr` (`tests/e2e/helpers_test.go`, `surface_test.go`).
- Produces: `TestE2E_WebUI`. It passes on its first run: it checks the assembled pieces, which all exist by now.

- [ ] **Step 1: Write the test**

`tests/e2e/ui_test.go`:

```go
package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
)

// TestE2E_WebUI: the real binary serves /ui, ui setup prints a working
// setup URL, a passkey registered with a software authenticator gets a
// session cookie, the cookie reaches an admin endpoint as the operator
// key, and forged cross-site requests are refused.
func TestE2E_WebUI(t *testing.T) {
	env, admin := bootstrapKeyFor(t)
	port := strconv.Itoa(portFromAddr(env.addr))
	host, origin := "localhost:"+port, "http://localhost:"+port
	cookies := map[string]string{}
	// do sends a request as the UI page would: the localhost Host, the
	// UI's Origin on writes, the X-Conclave-UI header, the cookies so far.
	do := func(method, path, body string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, env.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conclave-UI", "1")
		if method != "GET" {
			req.Header.Set("Origin", origin)
		}
		for k, v := range hdr {
			if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
		for k, v := range cookies {
			req.AddCookie(&http.Cookie{Name: k, Value: v})
		}
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(cookies, c.Name)
			} else {
				cookies[c.Name] = c.Value
			}
		}
		return resp, string(b)
	}

	// The pages, with the CSP.
	resp, body := do("GET", "/ui/", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `src="app.js"`) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("/ui/: %d %q\n%s", resp.StatusCode, resp.Header.Get("Content-Security-Policy"), body)
	}
	if resp, _ := do("GET", "/ui/app.js", "", nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("/ui/app.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// ui setup with the admin key prints the setup URL.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, env.BinaryPath(), "ui", "setup")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CONCLAVE_API_KEY=" + admin.Plaintext, "CONCLAVE_SERVER=" + env.URL()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ui setup: %v\n%s", err, stderr.String())
	}
	prefix := origin + "/ui/#setup="
	setupURL := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(setupURL, prefix) {
		t.Fatalf("ui setup printed %q, want %s<token>", setupURL, prefix)
	}
	token := strings.TrimPrefix(setupURL, prefix)

	// Register a passkey with the token: the response logs us in.
	pk := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "Conclave", Origin: origin}
	resp, body = do("POST", "/ui/auth/register/begin", `{"setup_token":"`+token+`","name":"e2e"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("register begin: %d %s", resp.StatusCode, body)
	}
	opts, err := virtualwebauthn.ParseAttestationOptions(body)
	if err != nil {
		t.Fatal(err)
	}
	pk.Options.UserHandle = []byte(opts.UserID)
	if resp, body := do("POST", "/ui/auth/register/finish", virtualwebauthn.CreateAttestationResponse(rp, pk, cred, *opts), nil); resp.StatusCode != 201 || cookies["conclave_ui"] == "" {
		t.Fatalf("register finish: %d %s", resp.StatusCode, body)
	}

	// The cookie reaches an admin endpoint as the operator key.
	if resp, body := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 200 || !strings.Contains(body, `"label":"operator"`) || !strings.Contains(body, `"admin":true`) {
		t.Errorf("whoami with the cookie: %d %s", resp.StatusCode, body)
	}
	if resp, body := do("GET", "/v1/keys", "", nil); resp.StatusCode != 200 {
		t.Errorf("keys with the cookie: %d %s", resp.StatusCode, body)
	}
	if resp, body := do("POST", "/v1/rooms", `{"name":"e2e-ui-room"}`, nil); resp.StatusCode != 201 {
		t.Errorf("create a room with the cookie: %d %s", resp.StatusCode, body)
	}

	// Forged cross-site requests carry the cookie but fail the checks.
	for _, c := range []struct {
		name, host string
		hdr        map[string]string
		want       int
	}{
		{"another origin", "localhost:" + port, map[string]string{"Origin": "http://attacker.example"}, 403},
		{"no X-Conclave-UI header", "localhost:" + port, map[string]string{"X-Conclave-UI": ""}, 401},
		{"rebound Host", "attacker.example:" + port, map[string]string{"Origin": "http://attacker.example:" + port}, 403},
	} {
		host = c.host
		if resp, body := do("POST", "/v1/rooms", `{"name":"forged"}`, c.hdr); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, resp.StatusCode, body, c.want)
		}
	}
	host = "localhost:" + port
	if r := env.call("GET", "/v1/rooms", admin.Plaintext, nil); r.Status != 200 || strings.Contains(string(r.Body), "forged") {
		t.Errorf("a forged request created a room: %d %s", r.Status, r.Body)
	}

	// Logout ends the session on the server.
	session := cookies["conclave_ui"]
	if resp, _ := do("POST", "/ui/auth/logout", "", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	cookies["conclave_ui"] = session
	if resp, _ := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("whoami after logout: %d, want 401", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run it**

Run: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./tests/e2e/ -run TestE2E_WebUI -count=1 -v`
Expected: `--- PASS: TestE2E_WebUI` (the harness builds the binary first; a `subprocess wait error: signal: killed` log line from the cleanup is normal).

Then the whole suite, vet and format:
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` → every package `ok` (the e2e package takes about a minute).
`docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go vet ./...` → no output.
`docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` → no output.

- [ ] **Step 3: Commit**

```bash
git add tests/e2e/ui_test.go
git commit -m "test(e2e): web UI through the real binary" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 4: Browser smoke test (controller)**

This is for the controller, after Task 10's commit; nothing of it is committed. It needs the built-in browser and `sqlite3` (part of macOS). Use a free port that no running Conclave service uses (7499 below; the default 7432 may be in use).

1. Build a native binary and start a throwaway server:

```bash
D="$(mktemp -d)"
ARCH="$(uname -m | sed 's/x86_64/amd64/')"
docker run --rm -v "$PWD":/src -v "$D":/out -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src -e CGO_ENABLED=0 -e GOOS=darwin -e GOARCH="$ARCH" golang:1.25 go build -o /out/conclave-mcp ./cmd/conclave-mcp
"$D/conclave-mcp" serve --port 7499 --db "$D/data.db" > "$D/serve.log" 2>&1 & sleep 1; kill $!; wait $! 2>/dev/null   # migrate, stop
ADMIN="$("$D/conclave-mcp" admin keys create-bootstrap --db "$D/data.db" --label smoke-admin 2>/dev/null)"
"$D/conclave-mcp" serve --port 7499 --db "$D/data.db" > "$D/serve.log" 2>&1 &
export CONCLAVE_API_KEY="$ADMIN" CONCLAVE_BASE_URL=http://127.0.0.1:7499
"$D/conclave-mcp" ui setup        # prints http://localhost:7499/ui/#setup=<token>
```

2. Start page: open `http://127.0.0.1:7499/ui/`. Expected: the address becomes `http://localhost:7499/ui/` and the page shows "Set up the web UI" with `conclave-mcp ui setup`.
3. Setup page: open the printed URL. Expected: "Register your passkey", a Name field and a "Register passkey" button. Do not press it (that is the Touch ID step). In the page console run
   `await fetch('/ui/auth/register/begin', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({setup_token: location.hash.slice(7)})}).then(r => r.json()).then(o => PublicKeyCredential.parseCreationOptionsFromJSON(o).rp.id)`
   Expected: `"localhost"` (the browser's own parser accepts the server's options; begin does not use the token up).
4. Console: no errors and no `Content-Security-Policy` violation (read the console messages).
5. Logged-in pages without Touch ID: seed data and a test session, then set the cookie in the page.

```bash
B=http://127.0.0.1:7499; H="Authorization: Bearer $ADMIN"
ROOM="$(curl -s -H "$H" -H 'Content-Type: application/json' -d '{"name":"smoke"}' $B/v1/rooms | sed 's/.*"id":"\([^"]*\)".*/\1/')"
EXT="$(curl -s -H "$H" -H 'Content-Type: application/json' -d '{"label":"partner"}' $B/v1/keys)"
EXTID="$(echo "$EXT" | sed 's/.*"id":\([0-9]*\).*/\1/')"; EXTKEY="$(echo "$EXT" | sed 's/.*"key":"\([^"]*\)".*/\1/')"
curl -s -H "$H" -H 'Content-Type: application/json' -d "{\"key_id\":$EXTID,\"rights\":[\"read\",\"question\",\"claim\"]}" $B/v1/rooms/$ROOM/invite
curl -s -H "Authorization: Bearer $EXTKEY" -H 'Content-Type: application/json' -d '{"content":"<img src=x onerror=alert(1)> review src/main.go?","message_type":"question"}' $B/v1/rooms/$ROOM/messages
curl -s -H "Authorization: Bearer $EXTKEY" -H 'Content-Type: application/json' -d '{"paths":["src/main.go"],"note":"reviewing"}' $B/v1/rooms/$ROOM/claims
NOW="$(date -u +%Y-%m-%dT%H:%M:%S.000000000Z)"; LATER="$(date -u -v+2H +%Y-%m-%dT%H:%M:%S.000000000Z)"
sqlite3 "$D/data.db" "INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES ('operator','x','smokeop1','$NOW','internal',1);
INSERT INTO ui_credentials (id, public_key, name, created_at) VALUES ('smoke', x'00', 'smoke', '$NOW');
INSERT INTO ui_sessions VALUES ('$(printf smoke-session | shasum -a 256 | cut -d' ' -f1)', '$NOW', '$NOW', '$LATER');"
```

   In the page console: `document.cookie = 'conclave_ui=smoke-session; path=/; SameSite=Strict'; location.hash = '#/'; location.reload()`. Expected, page by page:
   - `#/`: the room `smoke` with 1 open request and its last activity; the header lists Rooms, Keys, Audit, Passkeys, `smoke`, Log out.
   - the room: the question inside an EXTERNAL block between `--- external content from partner ---` and `--- end of external content ---`, with `<img src=x onerror=alert(1)>` shown as text and no alert; the open request; the claim marked EXTERNAL; members with rights checkboxes. Send `curl -s -H "$H" -H 'Content-Type: application/json' -d '{"content":"live check"}' $B/v1/rooms/$ROOM/messages` and see "live check" appear without reloading. "Save rights" on `partner` shows "Saved."; Delete on "live check", then Yes, removes it.
   - `#/keys`: `smoke-admin`, `partner`, `operator` (no Revoke button on `operator`); "Create an agent" with a label, room `smoke` and `read` shows the `export CONCLAVE_API_KEY=tk_…` page once; Done returns to the list.
   - `#/audit`: entries newest first, the UI's actions with actor `operator`; the action filter `room.` narrows the list.
   - `#/passkeys`: `smoke`, "the last passkey cannot be removed".
   - Log out: the "Log in" page with "Log in with a passkey".
   - The console stays free of errors and CSP violations.
6. Clean up: `kill %1 2>/dev/null; pkill -f "$D/conclave-mcp serve"; rm -rf "$D"; unset CONCLAVE_API_KEY CONCLAVE_BASE_URL`.

The Touch ID step itself (a real registration and login) is checked by hand once by the operator after the merge: `conclave-mcp ui setup` on the real server, register, log out, log in.
