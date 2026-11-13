# Confirmed changes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A UI session can only read; every change from the web UI runs only after a passkey assertion bound to that exact request.

**Architecture:** `ui.Auth` refuses every non-GET session request unless the request carries a confirmed mark (an unexported context value). `POST /ui/auth/confirm/begin` stores method, path and body in a `kindConfirm` ceremony; `finish` verifies the assertion and runs the stored request through the server's router with the mark. The UI's own changes (`/ui/agents`, passkey add and remove) check the mark too.

**Tech Stack:** Go 1.25, chi, modernc SQLite, go-webauthn v0.17.4, virtualwebauthn (tests), vanilla JS.

Spec: `docs/superpowers/specs/2026-11-13-confirmed-changes-design.md`.

## Global Constraints

- Branch `feat/confirmed-changes`. Go runs only in Docker from the repo root:
  `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...` (and `go vet ./...` the same way). gofmt: `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` prints nothing.
- JS syntax check after touching `internal/ui/assets/*.js`:
  `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'` → `ok`. `TestAssets_NoMarkupFromData` stays green.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Nothing personal in the repo. No new dependencies.
- Exact strings (verbatim):
  - session change without confirmation (`/v1` and the UI's own changes): 403 `forbidden` `changes made with a UI session need a passkey confirmation: make them in the web UI`
  - endpoints: `POST /ui/auth/confirm/begin` (`{method, path, body}`), `POST /ui/auth/confirm/finish`, `POST /ui/agents`
  - begin: 400 `bad_request` `method must be POST, PUT, PATCH or DELETE`; 400 `bad_request` `path is not a change the web UI can confirm`
  - finish without a ceremony: 400 `bad_request` `no confirmation in progress, or it took longer than 5 minutes`; another session: 403 `forbidden` `this confirmation was started in another session`; dead session: 401 `unauthenticated` `session expired: log in again`; rejected assertion: 401 `unauthenticated` `passkey not accepted`, audited as `ui.confirm` `error:rejected`
  - `/ui/agents`: 400 `bad_request` `room_id is required`; 400 `bad_request` `pick at least one right for the agent`; after a failed invite the invite's status and code with ` The key was revoked; its label stays taken, so a retry needs another label.` appended to its message
  - status in the page: `Waiting for your passkey…`

---

### Task 1: Sessions read only; the confirm ceremony

**Files:** Modify `internal/ui/auth.go` (ceremony fields, `kindConfirm`), `internal/ui/v1.go` (`Auth` refusal), `internal/ui/ui.go` (`Router` field, routes), `internal/server/server.go` (wire `Router`). Create `internal/ui/confirm.go`, `internal/ui/confirm_test.go`. Modify tests `internal/ui/helpers_test.go` (`newTestUI` sets `s.Router`; `confirmChange` helper), `internal/ui/v1_test.go` (POST cases now 403; an echo route), `internal/server/ui_test.go`.

- [ ] **Step 1: Failing tests** (`confirm_test.go`, `v1_test.go`): session POST/PATCH/DELETE to `/v1` 403 with the exact message, GET 200; a confirmed PATCH reaches an echo handler with begin's method, path, body and the operator key with scopes `admin`,`ui`; begin refusals (no session 401, foreign Origin 403, GET 400, paths `/ui/auth/logout`, `/v1/../ui/agents`, `//x/v1/keys`, `http://localhost:7432/v1/keys` 400); finish refusals (no UV + audit, other session + audit, second finish, login ceremony at confirm finish, confirm ceremony at message and login finish, after logout), the echo never called by a refused one. A confirmed `POST /v1/rooms/{id}/messages` with the real messages service: 403, nothing stored.
- [ ] **Step 2: Run, see them fail.**
- [ ] **Step 3: Implement.** `ceremony` gets `method`, `path` and `body` (the message ceremony's `message` is renamed `body`). `confirm.go`: `confirmedKey`, `confirmed(r)`, `changePath`, `confirmBegin`, `confirmFinish`, `run(w, r, method, path, body)` (new request with r's context minus chi's route context plus the mark, `Host` of r, the UI `Origin`, the session header, JSON content type; served by `s.Router`; 503 if nil). `Auth`: after the session and operator-key checks, a non-GET without the mark gets the 403.
- [ ] **Step 4: Package tests, full suite, vet. Green.**
- [ ] **Step 5: gofmt, commit** `feat(ui): sessions only read; changes need a passkey confirmation`.

### Task 2: The UI's own changes

**Files:** Modify `internal/ui/auth.go` (`registerGate`, `removeCredential`), `internal/ui/confirm.go` (`createAgent`), `internal/ui/ui.go` (route), tests in `internal/ui/confirm_test.go`, `internal/ui/auth_test.go`.

- [ ] **Step 1: Failing tests:** session `register/begin` without the mark 403; through a confirmation it answers creation options and a second passkey registers and logs in. `DELETE /ui/auth/credentials/{id}` without the mark 403, confirmed 204. `/ui/agents` without the mark 403; confirmed with real keys and rooms services creates the key (201, `key` present) and the membership with the rights; a missing room gives 404 with the note and the key is revoked; no rights 400.
- [ ] **Step 2: Run, see them fail.**
- [ ] **Step 3: Implement.** `registerGate`: logged in but not confirmed → 403. `removeCredential`: not confirmed → 403. `createAgent`: decode, validate, `POST /v1/keys` and the invite as recorded sub-requests (`run` into an `httptest.ResponseRecorder`), revoke on a failed invite, copy the response.
- [ ] **Step 4: Tests, vet. Green. Step 5: gofmt, commit** `feat(ui): confirmed agent creation and passkey changes`.

### Task 3: The pages

**Files:** `internal/ui/assets/lib.js` (`confirmed(method, path, body)`, `WAITING`, `confirmButton` shows it), `keys.js`, `rooms.js`, `app.js`.

- [ ] **Step 1:** `confirmed` runs begin, `navigator.credentials.get`, finish; a 401 from finish while `/ui/auth/state` still says logged in becomes a plain error (a refused passkey, not a dead session).
- [ ] **Step 2:** Every state-changing call goes through `confirmed`: create key, revoke, create agent (`/ui/agents`, no client-side rollback), create room, archive, invite, save rights, remove member, delete message, add passkey (logged in), remove passkey. Forms show `Waiting for your passkey…`.
- [ ] **Step 3:** JS check, `TestAssets_NoMarkupFromData`. Commit `feat(ui): pages confirm every change with a passkey`.

### Task 4: e2e, docs, changelog

- [ ] **Step 1:** `tests/e2e/ui_test.go`: session `POST /v1/rooms` 403 with the message; the same through `confirm/begin|finish` 201; the session message POST expects the new 403 message.
- [ ] **Step 2:** `docs/web-ui.md` (Changes section, security model rows, audit `ui.confirm`, "Not in the UI"), CHANGELOG `[Unreleased]`.
- [ ] **Step 3:** Full suite, vet, gofmt, JS check. Browser smoke test against a locally built server. Commit, push, PR.
