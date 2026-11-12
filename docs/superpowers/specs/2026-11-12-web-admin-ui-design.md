# Web admin UI, part 1: passkey login and management

Status: decided (2026-11-12). Decisions below are final for part 1; the
reasoning is recorded next to each one.

## Goal

The operator manages Conclave from a browser instead of the CLI: rooms,
members and rights, keys, audit, and reading what agents wrote. Login uses
a passkey (Touch ID or another platform authenticator), so no password exists
that an agent could read from a file or the key store.

Part 2 (a later spec) adds the operator channel: messages and decisions sent
from the UI that agents can verify as coming from the operator, with each
decision confirmed by user presence. Part 1 builds the login and the
principal that part 2 needs.

## Scope

**In part 1:**
- Passkey login, setup and logout; adding and removing passkeys.
- Everything the running server's API offers to an admin key:
  - rooms: list, create, archive;
  - members: list, invite with rights, change rights, remove;
  - keys: list, create (secret shown once), revoke;
  - create an agent in one step: key plus invitation with rights, like
    `agent add`;
  - messages: list, threads, open requests, claims, live updates;
  - audit log with filters;
  - message delete (admin only).

**Not in part 1:**

| Left out | Why |
|---|---|
| Sending messages from the UI | That is the operator channel (part 2); it needs the authenticity mark. |
| Offline commands (`admin vacuum`, `admin keys create-bootstrap`, `admin ui-reset`) | They need the service stopped; the CLI keeps them. |
| `init`, hooks, `--store` | They configure a repository or a local key store, not the server. |
| Remote access (non-loopback) | Passkeys need a secure context; plain HTTP is only secure on `localhost`. TLS setups can come later. |
| A password fallback | Passkeys sync through the platform key store; `admin ui-reset` covers loss. |

## Decisions

| Topic | Decision | Why |
|---|---|---|
| How the UI talks to the server | Static HTML/CSS/JS embedded in the binary calls the existing `/v1` API with a session token in the `X-Conclave-UI` header. | Parity with the CLI for free: every endpoint the CLI uses already exists and is already permission-checked. No second implementation of any rule. |
| Frontend | Vanilla JS, no framework, no build step, embedded with `embed.FS`. | Small UI, single operator; nothing to install or keep updated. |
| Login | Passkeys only (WebAuthn), user verification required, attestation none, discoverable credentials. | No secret an agent can read; Touch ID needs physical presence. |
| Server library | `github.com/go-webauthn/webauthn` v0.17.4. | The standard Go implementation; v0.17.4 is the newest release that builds with Go 1.25 (v0.18 needs Go 1.26). Hand-rolling CBOR/COSE parsing is not an option. |
| Origin | `http://localhost:<port>`, RP ID `localhost`. Requests for `/ui` on `127.0.0.1` redirect to `localhost`. | An IP address is not a valid RP ID, and passkeys are bound to the RP ID. |
| Who the UI acts as | A dedicated admin key labelled `operator`, created at UI setup, whose secret is random and never shown or stored in plain text. Sessions act as this key. | The audit log shows UI actions as `operator`, separate from CLI use of the bootstrap admin key. Part 2 builds the authentic channel on this principal. |
| Trust anchor for registration | The first passkey can only be registered with a one-time setup token while no passkey exists. Later passkeys are added from a logged-in session. | Software authenticators can register too, so the ceremony is the only anchor. Once the operator has a passkey, an agent holding the admin key can no longer add its own. |
| Setup token | `conclave-mcp ui setup` (needs an admin key, server running) asks the server for a token valid 10 minutes and prints `http://localhost:<port>/ui/#setup=<token>`. | The fragment never reaches server logs or proxies. |
| Losing all passkeys | `conclave-mcp admin ui-reset` (offline, service stopped) deletes all passkeys and sessions and writes an audit entry; then `ui setup` again. | Same model as `create-bootstrap`: whoever can stop the service and write the database file is already the operator. |
| Session | 32 random bytes; the database stores only the SHA-256 hash. `login/finish` and the first `register/finish` return it as `{"session": "<token>"}`. The page keeps it in a JavaScript variable only and sends it as `X-Conclave-UI: <token>`; there is no session cookie, and none is read. Each tab logs in on its own; a reload needs a passkey login. Idle timeout 1 hour, absolute 12 hours. Logout deletes the row. | Browsers send cookies for `localhost` to every port on `localhost` (RFC 6265 §8.5; `SameSite` ignores ports), so any local process listening on another port could collect a cookie and replay it. `localStorage` and `sessionStorage` are kept unencrypted in the browser profile on disk, readable by any process of the same user. Server-side sessions can be revoked; hashing means a database read does not yield a usable token. |
| CSRF and DNS rebinding | Sessions are accepted on `/v1` only as the `X-Conclave-UI` header; non-GET requests also need `Origin` equal to the UI origin. Every `/ui` and session-authenticated `/v1` request needs `Host` to be `localhost:<port>` or `127.0.0.1:<port>`. | No credential is sent automatically, so a cross-site request carries none; a site cannot set the header cross-origin without CORS, which the server does not allow. The Host check stops a rebinding site from reaching the loopback server. |
| Content safety | Strict CSP (`default-src 'self'`, no inline script or style, `frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`. The JS only ever sets `textContent`, never `innerHTML` with data. | Message content and labels come from agents, some external; they must never become markup. |
| Live updates | The UI reads `/v1/rooms/{id}/events` with `fetch` streaming (so it can send the header), not `EventSource`. | `EventSource` cannot send headers; the token travels only in the header. |
| When the UI is served | Only when the server listens on a loopback address; otherwise `/ui` returns 404 and UI sessions count for nothing. | See remote access above. |

## Server changes

**Migration `0009_web_ui.sql`:**

```sql
CREATE TABLE ui_credentials (
    id            TEXT PRIMARY KEY,      -- base64url credential id
    public_key    BLOB NOT NULL,
    sign_count    INTEGER NOT NULL DEFAULT 0,
    flags         INTEGER NOT NULL DEFAULT 0, -- authenticator flags (backup eligible/state) the library checks on login
    transports    TEXT NOT NULL DEFAULT '',
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

The `operator` key is an ordinary `api_keys` row (`is_admin = 1`, label
`operator`, `trust_origin = internal`), created once by the first successful
registration. The WebAuthn user handle is a fixed random value stored in
`meta` (`ui_user_handle`).

**New package `internal/ui`:**
- `ui.go`: embedded assets under `/ui/` (index.html, app.js, app.css), the
  security headers, the loopback and Host checks, the `127.0.0.1` redirect.
- `auth.go`: the WebAuthn ceremonies and session handling:
  - `POST /ui/auth/register/begin` and `/finish`: needs a valid setup token
    while no credential exists, or a valid session; the first registration
    creates the `operator` key and logs the operator in (the response
    carries `session`).
  - `POST /ui/auth/login/begin` and `/finish`: discoverable login (no user
    name); on success the response is `{"session": "<token>"}`.
  - `POST /ui/auth/logout`.
  - Endpoints that need a session read it from `X-Conclave-UI`.
  - `GET /ui/auth/state`: `{setup_needed, logged_in}`, for the start page.
  - Ceremony data between begin and finish stays in memory, keyed by a
    random cookie, valid 5 minutes.
- `sessions.go`: create, look up (sliding idle expiry), delete, purge expired.
- Passkey management for a logged-in session: `GET /ui/auth/credentials`,
  `DELETE /ui/auth/credentials/{id}` (not the last one).

**`/v1` auth:** the verifier middleware accepts, in addition to the Bearer
key (which wins when both are sent), a session token in `X-Conclave-UI`
when the request passes the Host check and (non-GET) the Origin check.
Such a request gets the claims of the `operator` key. No cookie counts
as a session. Nothing else in `/v1`
changes. New admin-only endpoint `POST /v1/ui/setup-tokens` returns
`{token, expires_at}`; `GET /v1/ui/setup-tokens` is not offered.

**Audit:** `ui.setup_token`, `ui.passkey.add`, `ui.passkey.remove`,
`ui.login`, `ui.logout`, `ui.reset`.

**CLI:** `conclave-mcp ui setup` prints the setup URL;
`conclave-mcp admin ui-reset` (offline, `--db` like `create-bootstrap`).

## UI

One page with hash routes; header with the room list and logout.

| Route | Shows | Actions |
|---|---|---|
| `#/` | Rooms you manage, each with open requests and last activity | Create room |
| `#/rooms/<id>` | Messages (newest last, threads expandable, live), open requests, active claims, members with rights and internal/external mark | Invite (key + rights + multi-room), change rights, remove member, archive room, delete message |
| `#/keys` | Keys with label, trust, rooms, created, revoked | Create key (secret shown once with copy button), create agent (key + room + rights), revoke |
| `#/audit` | Audit entries, newest first, filters for actor, action prefix, room, since | – |
| `#/passkeys` | Registered passkeys | Add passkey, remove (not the last one) |
| `#setup=<token>` | First-time setup | Register passkey |

External messages show the same `EXTERNAL` mark and fence idea as the CLI:
a visibly separated block, plain text. Destructive actions (revoke, remove,
archive, delete) ask for confirmation in the page. Errors from `/v1` are shown
with their code and message.

## Testing

- **Unit (Go):** session create/lookup/expiry; Host, Origin and header checks
  (no header 401, a valid token only as a cookie 401 and not logged in,
  wrong Origin 403, wrong Host 403); the
  registration gate (setup token only while no credential exists; session
  otherwise; token single-use and expiring); setup-token endpoint admin-only;
  last passkey cannot be removed; UI disabled on non-loopback listen
  addresses; CSP and headers on `/ui`.
- **Ceremonies end to end in Go** with a software authenticator in tests
  (`github.com/descope/virtualwebauthn`, test-only): setup → register →
  login → `/v1` call with the session token as `operator` → logout.
- **e2e:** the real binary serves `/ui/` and the assets; the token from the
  finish response reaches an admin endpoint; a forged request (wrong
  Origin, no header, the token only as a cookie) is refused.
- **Browser smoke test** in the built-in browser: start page, setup page
  rendering, CSP has no violations in the console. The Touch ID step itself
  is checked by hand once.

## Rollout

Additive: no existing behaviour changes, so a minor release. After upgrading:
`conclave-mcp ui setup`, open the printed URL, register a passkey. The
CHANGELOG and a new `docs/web-ui.md` describe setup, reset and the security
model.
