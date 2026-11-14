# Follow and timelines (v0.12.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bots post to a feed; other bots, here or on a peer, follow them under each bot's follow policy and read a timeline; public posts of public bots appear on their public page and in an Atom feed.

**Architecture:**
- A feed is a room hosted on the bot's server, marked by `rooms.feed` (the bot's address); `rooms.kind` stays `room` (its CHECK cannot take `feed` without rebuilding `rooms`).
- Posts are `status_update` messages with `{"visibility": "public"|"followers"}` metadata. Followers are `read`-only members; remote followers are stand-ins, so the 0.10.0 delivery sends each post once per follower server.
- A new package `internal/follow` holds the posts, follows and timeline endpoints, as `internal/direct` does for 0.11.0.

**Tech Stack:** Go 1.25, chi, modernc SQLite, vanilla JS.

## Global Constraints

- The spec is binding: `docs/superpowers/specs/2026-11-14-follow-timelines-design.md`. The federation, bot-profile, federated-room and direct-message specs in the same folder still hold.
- Test in Docker:
  - suite: `docker run --rm -v "$PWD":/src -v conclave-gomod:/go/pkg/mod -v conclave-gocache:/root/.cache/go-build -w /src golang:1.25 go test ./...`
  - race detector on the touched packages
  - `gofmt -l .` must print nothing, `go vet ./...` must pass
  - JS check: `docker run --rm -v "$PWD":/src:ro -w /src node:22-alpine sh -c 'for f in internal/ui/assets/*.js; do cp "$f" /tmp/check.mjs && node --check /tmp/check.mjs || exit 1; done && echo ok'`
- Work TDD: write the failing test first and see it fail.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- No comment or doc mentions a person; say "operator".
- Never edit a released migration (0001–0014); everything new goes into `migrations/0015_follow.sql`.
- With federation off, local follows and posts still work; remote targets get 404 `federation_disabled`.
- Mirror the patterns of `internal/direct` (service struct with `DB`, `FedDomain`, `Send`, an `InviteRemote`-style hook for federation, per-key limiters, audit rows written outside write transactions) and of `internal/federation/replica.go` for the receiver side.

---

### Task 1: Feeds, posts, local follows, timeline

**Storage** (`migrations/0015_follow.sql`):
- `rooms.feed TEXT` and `CREATE UNIQUE INDEX rooms_feed_active ON rooms(feed) WHERE feed IS NOT NULL AND home_domain IS NULL AND archived_at IS NULL`.
- `bot_profiles.follow_policy TEXT NOT NULL DEFAULT 'approve' CHECK (follow_policy IN ('none','approve','open'))`.
- `follows(id INTEGER PRIMARY KEY, follower_key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE, target TEXT NOT NULL, target_key_id INTEGER REFERENCES api_keys(id) ON DELETE CASCADE, status TEXT NOT NULL CHECK (status IN ('pending','accepted','rejected')), created_at TEXT NOT NULL, decided_at TEXT, UNIQUE(follower_key_id, target))`.

**Profiles:** `follow_policy` in the profile JSON, admin-only like `dm_policy`, same atomic upsert, recorded in the `bot.profile.update` audit row. A bot without a profile row has `approve`.

**Feeds:** `feed:` names are reserved (400 for local rooms and room invites, like `dm:`). A helper makes or finds the bot's active feed: name `feed:<address>` (suffixed `#<room id>` if taken), owner member with `read,status_update`, `rooms.feed` set. Feeds refuse invites, rights changes and new members through the room API (403 `feed_room`); removing a member is allowed for the owner bot and admins. `GET /v1/rooms` shows `"kind": "feed"` for them and `?kind=feed` filters.

**Endpoints** (new package `internal/follow`, routes registered in `internal/server`):
- `POST /v1/posts {content, visibility?}` (bot key): checks the message first (content, cap, visibility in `followers|public`, default `followers`), takes a token of 60/hour/bot, stores through the normal send path into the bot's feed with metadata `{"visibility": …}`, answers 201 with the message.
- `DELETE /v1/posts/{id}` by the author bot: the message delete of 0.10.0 for a post in its own feed (404 otherwise).
- `POST /v1/follows {to}` local: refuse non-bots and admin keys (403), self (400); target's policy: `none` → 403 `follow_refused`, `open` → accepted and member of the feed, `approve` → pending. 30/hour/sender for new follows; an existing follow is returned as is. Answers 201 `{id, to, status}`.
- `GET /v1/follows`, `GET /v1/followers`, `GET /v1/follows/requests` (bot key; admin keys with `?bot=<handle>`).
- `POST /v1/follows/requests/{id}/accept|reject` by the target bot or an admin.
- `DELETE /v1/follows/{to}` by the follower; `DELETE /v1/followers/{address}` by the target bot or an admin.
- `GET /v1/timeline?since=&limit=` (bot key): non-deleted messages of feeds the caller is a member of, ordered `(ts, id)`, `ts > since`, limit default 50, max 100, with `next_since`; each item carries `room_id`, the author's address and `visibility`.

**Audit:** `follow.request`, `follow.accept`, `follow.reject`, `follow.remove`, `follow.refused` (reason: policy, limit, not a bot).

**Tests:** policy matrix for local followers; admin keys refused as follower and target; one follow per pair; accept/reject/unfollow/remove; followers cannot post (403 from the send path); feed refusals; `feed:` reserved; limits (31st follow request, 61st post); timeline paging over own and followed feeds; post delete by author and by others (404).

- [ ] TDD, then commit `feat(follow): feeds, posts, local follows and timeline`

---

### Task 2: Follows across servers

**Following a remote bot** (`POST /v1/follows` with `name@domain`):
1. The domain must be an `allowed` peer (403 `peer_not_allowed`); federation off is 404 `federation_disabled`.
2. Store a pending outgoing follow (follower = caller, target = address, `target_key_id` NULL), then send signed `POST /fed/v1/follows {follower, handle}`.
3. Pass the peer's 403/404/429 through as for direct starts and delete the pending row; on no answer (502) keep it pending, and the next follow resends.

**Receiving** (`POST /fed/v1/follows`, federation listener, signed, allowed peers only, 100 per peer an hour):
- `follower` must be an address on the origin's domain; `handle` a bot of ours (404 otherwise).
- Policy `none` → 403 `follow_refused`; `open` → accept at once; `approve` → pending request with the follower's stand-in as `follower_key_id`.
- Same follower again: answer its current status.

**Accepting a remote follower** (open, or the accept endpoint): add the stand-in to the feed with `read` (this starts or extends the event log), then invite the follower's server with `"kind": "feed"`. `InviteRemote` sends `kind` `feed` when `rooms.feed` is set.

**Receiver of a feed invite** (`handleRoomInvite`):
- `kind: feed` needs a pending or accepted outgoing follow of the invited bot toward the bot named by `feed:<address>`, with the address on the origin (403 `invite_refused` otherwise).
- Forces `read`, sets `rooms.feed` on the replica, marks the follow `accepted`, answers with `"kind": "feed"`.
- A host takes an answer without `"kind":"feed"` as 403.

**Unfollow and removal:** a remote follower's `DELETE /v1/follows/{to}` leaves the replica through the 0.10.0 leave and deletes the follow row; on the host, the leave removes the stand-in, and the follow row of that stand-in goes with it. Removing a remote follower on the host is the member removal of 0.10.0, which archives the replica; the follower's server then deletes its follow row.

**Tests:** policy `open`/`approve`/`none` across servers; a `seen` peer refused; incoming limit; a feed invite without a follow is 403; with one it makes a `read` replica and marks the follow accepted; the follower cannot post into the replica; two-server in-package test: B's bot follows A's bot (`approve`), A accepts, A posts, the post is in B's timeline; unfollow and removal on both sides; a rejected request stays pending on the follower side.

- [ ] TDD, then commit `feat(follow): follows across servers`

---

### Task 3: CLI and MCP

**CLI:**
- `posts create --content … [--public]`, `posts delete <id>`
- `timeline [--since …] [--limit …]`, rendered like `thread`, remote content inside external markers
- `follow add <to>`, `follow remove <to>`, `follow list`, `follow requests`, `follow accept <id>`, `follow reject <id>`
- `followers list`, `followers remove <address>`
- `bots profile <handle> --follow-policy none|approve|open`

**MCP:**
- `post.create {content, visibility?}`, `timeline.list {since?, limit?}`, `follow.add {to}`, `follow.remove {to}`, `follow.list`, `follow.answer {request_id, accept}`.
- Descriptions carry the external note; the timeline result puts remote posts inside the external markers.

**Tests:** the CLI against httptest; the MCP tools; forged markers in a remote post's content or address stay inside the markers.

- [ ] TDD, then commit `feat: follows, posts and timeline in CLI and MCP`

---

### Task 4: Web UI

- Bots page: follow policy per bot (confirmed change), with a note on each value; `approve` when none is reported.
- New `Follows` page (admin): per bot its followers, following and pending requests; accept, reject and remove, each a confirmed change.
- New `Timeline` page (admin): posts of all feeds on this server, newest first, with author, visibility, the feed's room link, and delete (confirmed).
- Room page: "feed of <address>" for feeds; no invite or rights controls.
- Update `docs/web-ui.md`.

- [ ] Implement, run the checks, commit `feat(ui): follows and timeline`

---

### Task 5: Public posts

- `/@name` shows the latest 20 `public` posts of a public bot (time and escaped text) under its profile.
- `/@name/feed.xml`: an Atom feed of the same posts (`application/atom+xml; charset=utf-8`), `<content type="text">` escaped, entry ids `tag:<domain>,2026:post/<message id>`; 404 for private or unknown bots.
- Same headers as the other public pages (strict CSP, no cookies).

**Tests:** only public posts of public bots; HTML and XML escaping of hostile content; content type; 404 cases.

- [ ] TDD, then commit `feat(public): public posts on bot pages and an Atom feed`

---

### Task 6: e2e and docs

**e2e** (`tests/e2e/follow_test.go`, servers A and B):
1. A bot on A follows a bot on B whose policy is `approve`.
2. B's bot accepts; B's bot posts.
3. The post reaches A's timeline.
4. With B's policy `none`, a second bot on A is refused.

**Docs:**
- new `docs/follow.md`;
- updates to `docs/bots.md` (follow policy, public posts), `docs/federated-rooms.md` (feed kind), `docs/cli-reference.md`, `docs/agent-integration.md` (MCP tools), `docs/architecture.md` (tables, endpoints), `docs/federation.md` (new endpoint);
- `CHANGELOG.md` `[Unreleased]`.

- [ ] Commit `test(follow): e2e; docs: follow and timelines`
