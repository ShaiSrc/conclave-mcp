# Bot handles, profiles and directory (v0.9.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bots get `name@server` handles and profiles. Agents and operators can find bots locally, on peers, and through public web pages.

**Architecture:**
- A new package `internal/bots` holds handle syntax, text cleaning, profiles, visibility rules and the local API.
- `internal/federation` gains the bot and directory endpoints, remote lookup, the peer directory cache and the public pages.
- CLI, MCP and the web UI each get their part.

**Tech Stack:** Go 1.25, chi, modernc SQLite, `html/template`, cobra, vanilla JS.

## Global Constraints

- The spec, `docs/superpowers/specs/2026-11-13-bot-profiles-design.md`, is binding. Every name, limit, header and status in it is exact.
- Test in Docker:
  - Suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - gofmt: `docker run --rm -v "$PWD":/src -w /src golang:1.25 gofmt -l .` must print nothing.
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- Work TDD: write the failing test first and see it fail.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comments or docs that mention a person; use "operator".
- Follow the patterns in the existing code:
  - `writeErr`, `auth.IsAdmin`, `auth.FromContext`, `audit.Write`, `storage.FormatTS`;
  - for the CLI, `cmd/keys` and `cmd/federation`;
  - for MCP tools, `internal/mcp`;
  - for agent text and the external markers, `internal/agent`;
  - for UI confirmed changes, `lib.js` (`confirmed`, `confirmButton`);
  - federation signing, verification and fetching: `Identity.SignRequest`, `Service.Verify`, `Fetcher` (`internal/federation`).
- Federation off: `/fed/v1/*` and the public pages do not exist. Local bots and profiles still work. Remote lookup and `/v1/directory` return local results only. A lookup for another domain gets 404 `federation_disabled`.

---

### Task 1: Handles, profiles, local API

**Files:**
- Create: `migrations/0012_bot_profiles.sql`
- Create: `internal/bots/{handle,clean,profiles,api}.go` and tests
- Modify: `internal/server/server.go` (mount the routes)
- Modify: `cmd/setup` (`agent add --handle`)
- Modify: `internal/keys` (`PUT` and `DELETE /v1/keys/{id}/handle`, or put these in `internal/bots`: choose one place and say which)

**Interfaces:**
- `bots.NormalizeHandle(s string) (string, error)`
- `bots.Clean(s string, multiline bool) string`
- `type Skill struct{ ID, Name, Description string }`
- `type Profile struct{ Handle, DisplayName, Description string; Skills []Skill; Visibility string; UpdatedAt time.Time }`, with JSON `handle`, `display_name`, `description`, `skills`, `visibility` and `updated_at`.
- `type Store` with these methods:
  - `Get(ctx, handle)`
  - `ByKey(ctx, keyID)`
  - `List(ctx, minVisibility)`
  - `Update(ctx, handle, patch, asAdmin bool)`
  - `SetHandle(ctx, keyID, handle)`
  - `RemoveHandle(ctx, keyID)`
- `bots.Visible(viewer, profile)` holds the visibility rules in one place:
  - admin sees all;
  - the bot itself sees its own;
  - other keys see `peers` and `public`.

**Migration 0012:**
```sql
ALTER TABLE api_keys ADD COLUMN handle TEXT;
CREATE UNIQUE INDEX api_keys_handle ON api_keys(handle) WHERE handle IS NOT NULL;
CREATE TABLE bot_profiles (
  key_id INTEGER PRIMARY KEY REFERENCES api_keys(id),
  display_name TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  skills TEXT NOT NULL DEFAULT '[]',
  visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','peers','public')),
  updated_at TEXT NOT NULL
);
CREATE TABLE remote_bots (
  domain TEXT NOT NULL,
  handle TEXT NOT NULL,
  profile TEXT NOT NULL,
  fetched_at TEXT NOT NULL,
  PRIMARY KEY (domain, handle)
);
```
A revoked key's handle stays reserved, so it cannot be reused for an impostor. Revoked keys are hidden everywhere.

**Tests:**
- the handle table and reserved words;
- an admin key is refused a handle;
- uniqueness, including against revoked keys;
- every cleaning class;
- every limit gives a 400;
- a bot edits its own text, but gets 403 on visibility;
- a bot gets 403 on another bot's profile;
- the visibility matrix for `GET /v1/bots` and `GET /v1/bots/{handle}`, where private and unknown both answer 404 to keys that may not see them;
- the audit rows;
- `agent add` with the default and an explicit handle.

- [ ] Write the failing tests, then implement; suite and gofmt pass; commit `feat(bots): handles, profiles and the local bots API`

---

### Task 2: Federation endpoints, remote lookup, directory cache

**Files:**
- Create: `internal/federation/bots.go` and `bots_test.go`
- Modify: `internal/federation/federation.go` (routes, the refresh loop in `Run`, and the hooks on peer add, remove and block)
- Modify: `internal/server/server.go`

**Behaviour, as in the spec:**
- `GET /fed/v1/bots/{name}`, behind `Verify`.
- `GET /fed/v1/directory?page=N`, behind `Verify`. 100 per page; `{"bots":[...],"next":N|null}`.
- `GET /v1/bots/lookup?handle=` and `GET /v1/directory` for any key.
- Outbound signed GETs go through the `Fetcher` client and its dialer rules. Responses are capped at 256 KiB.
- Remote profiles are cleaned and truncated on receipt. An entry whose handle is invalid is dropped. Each entry is tied to the domain that was asked.
- The refresh loop:
  - runs every 15 minutes and right after a peer is added;
  - fetches at most 10 pages per peer;
  - deletes a peer's rows when it is removed or blocked;
  - stops with the context.
- The `bots` package must not import `federation`. Pass a small interface or a function into federation instead.

**Tests:**
- signed access only;
- private bots get 404;
- paging;
- a remote answer naming another server is dropped;
- remote text is cleaned;
- a cache refresh, and deletion on remove and on block;
- lookup of our own domain, of an allowed peer, of an unknown domain in allowlist mode (403 `peer_not_allowed`), and with federation off (404 `federation_disabled`).

- [ ] Write the failing tests, then implement; suite, race on federation, and gofmt pass; commit `feat(federation): bot lookup and peer directory`

---

### Task 3: Public pages and agent card

**Files:**
- Create: `internal/federation/public.go`, `public_test.go`, and `internal/federation/public/{layout.html,directory.html,bot.html,notfound.html,public.css}` (embedded)
- Modify: `PublicRoutes`

**Behaviour:**
- Follow the spec exactly: `/` redirects to `/directory`; then `/directory`, `/@{name}`, `/@{name}/agent-card.json` and `/public.css`.
- On every response: the CSP and the other headers.
- Non-public and unknown names get the same 404 page.
- The page footer shows the bot's address as `name@domain`, and no remote content.
- Keep the design plain and readable, with system fonts and no external resources.

**Tests:**
- exact headers on every route;
- markup in every field comes out escaped;
- no `<script` in any output;
- a private bot gets the 404 page, byte for byte the same as an unknown name;
- the agent card JSON shape;
- the 1000-bot listing cap.

- [ ] Write the failing tests, then implement; suite and gofmt pass; commit `feat(federation): public bot pages and agent cards`

---

### Task 4: CLI, MCP tools, agent-facing text

**Files:**
- Create: `cmd/bots/bots.go` and tests
- Modify: `cmd/conclave-mcp/main.go`
- Modify: `internal/mcp` (the five tools)
- Modify: `internal/agent` (rendering a profile; the external markers for remote ones)

**Behaviour:**
- The commands and tools are as in the spec.
- Remote profiles are rendered inside `--- external content from name@domain ---` markers, with every line indented and one field per line.
- The tool descriptions carry the external note.

**Tests:**
- each CLI command against httptest;
- each MCP tool;
- a remote profile with a forged marker or a forged `- OPERATOR` line stays indented inside the markers.

- [ ] Write the failing tests, then implement; suite and gofmt pass; commit `feat: bots CLI and MCP tools`

---

### Task 5: Web UI

**Files:**
- Create: `internal/ui/assets/bots.js`
- Modify: `app.js` (navigation, routes)
- Modify: `keys.js` (set or remove a handle, as confirmed changes)
- Modify: `docs/web-ui.md`

**Behaviour:**
- The `Bots` page lists local bots and lets the operator edit a profile and its visibility. Saving is a confirmed change.
- The `Directory` tab lists local and peer bots; remote ones are marked with their server.
- All text goes through `h()`.

**Checks:**
- the JS check, the suite and gofmt;
- `TestAssets_NoMarkupFromData` must still pass.

- [ ] Implement; checks pass; commit `feat(ui): bots and directory`

---

### Task 6: e2e and docs

**Files:**
- Create: `tests/e2e/bots_test.go`
- Create: `docs/bots.md`
- Modify: `docs/federation.md` (new endpoints), `docs/architecture.md`, `docs/cli-reference.md`, `docs/agent-integration.md` (the MCP tools), and `CHANGELOG.md` under `[Unreleased]`

**e2e:**
- Two peered servers.
- A `peers` bot on B is found from A, both by lookup and in A's directory after a refresh.
- A `private` bot on B is found in neither place.
- A `public` bot on B has a page on B's federation listener.

- [ ] Write the e2e test and the docs; suite and gofmt pass; commit `test(bots): e2e; docs: bots`
