package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// The feed: prefix is the feeds': a room invite with such a name is
// refused.
func TestRoomInvite_FeedPrefixReserved(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.bot(t, "b1")
	wantErr(t, h.invite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), "b1", "read"), http.StatusBadRequest, "bad_request")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms", n)
	}
}

// Follows across servers (docs/superpowers/specs/2026-11-14-follow-timelines-design.md).

// followBot adds a bot whose follow policy is policy; "" leaves it
// without a profile row.
func (g *roomRig) followBot(t *testing.T, handle, policy string) auth.Claims {
	t.Helper()
	id := g.bot(t, handle)
	if policy != "" {
		if _, err := g.db.Exec(`INSERT INTO bot_profiles(key_id, updated_at, follow_policy) VALUES (?, '2026-01-01T00:00:00Z', ?)`,
			id, policy); err != nil {
			t.Fatal(err)
		}
	}
	return auth.Claims{KeyID: id}
}

// followTo is c's follow of the bot at to.
func (g *roomRig) followTo(c auth.Claims, to string) *httptest.ResponseRecorder {
	return g.do(c, http.MethodPost, "/follows", fmt.Sprintf(`{"to":%q}`, to))
}

type followView struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

// followOf returns the follow of a 201 or 200 answer.
func followOf(t *testing.T, rec *httptest.ResponseRecorder) followView {
	t.Helper()
	var v followView
	if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.ID == 0 {
		t.Fatalf("follow: %d %s", rec.Code, rec.Body)
	}
	return v
}

// follows returns c's list at path (/follows, /followers, /follows/requests) under name.
func (g *roomRig) follows(t *testing.T, c auth.Claims, path, name string) []followView {
	t.Helper()
	rec := g.do(c, http.MethodGet, path, "")
	var out map[string][]followView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	return out[name]
}

// follow is a follow request of h's bot follower (an address) toward
// g's bot handle.
func (h *fakeHost) follow(t *testing.T, g *roomRig, follower, handle string) *httptest.ResponseRecorder {
	t.Helper()
	return g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/follows",
		map[string]string{"follower": follower, "handle": handle}))
}

// feedInvite is a feed invite of g's bot handle into room, named name.
func (h *fakeHost) feedInvite(t *testing.T, g *roomRig, room, name, handle string) *httptest.ResponseRecorder {
	t.Helper()
	return g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/invite",
		map[string]any{"name": name, "handle": handle, "rights": []string{"read", "status_update"}, "kind": "feed"}))
}

// The followed bot's server decides by its policy: none refuses, approve
// waits, open accepts and brings the follower's server into the feed. A
// refusal reaches the follower with its status and code, and neither
// server keeps a follow.
func TestFollowRemote_Policy(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.followBot(t, "a1", "")
	b.followBot(t, "none", "none")
	b.followBot(t, "appr", "approve")
	b.followBot(t, "norow", "")
	b.followBot(t, "open", "open")
	b.bot(t, "boss")
	if _, err := b.db.Exec(`UPDATE api_keys SET is_admin = 1 WHERE handle = 'boss'`); err != nil {
		t.Fatal(err)
	}
	to := func(h string) string { return h + "@" + b.svc.Domain() }
	wantErr(t, a.followTo(a1, to("none")), http.StatusForbidden, "follow_refused")
	wantErr(t, a.followTo(a1, to("nobody")), http.StatusNotFound, "not_found")
	wantErr(t, a.followTo(a1, to("boss")), http.StatusNotFound, "not_found")
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 0 {
			t.Errorf("%s keeps %d follows after the refusals", g.svc.Domain(), n)
		}
		if n := audited(t, g, "follow.refused", "policy"); n != 1 {
			t.Errorf("%s audited %d policy refusals, want 1", g.svc.Domain(), n)
		}
		if n := audited(t, g, "follow.refused", "not a bot"); n != 2 {
			t.Errorf("%s audited %d not-a-bot refusals, want 2", g.svc.Domain(), n)
		}
	}

	for _, h := range []string{"appr", "norow"} {
		if v := followOf(t, a.followTo(a1, to(h))); v.Status != "pending" || v.To != to(h) || v.From != "a1@"+a.svc.Domain() {
			t.Errorf("follow %s: %+v", h, v)
		}
		if n := count(t, a.db, `SELECT COUNT(*) FROM follows WHERE target = ? AND target_key_id IS NULL AND status = 'pending'`, to(h)); n != 1 {
			t.Errorf("a has no pending follow of %s", h)
		}
	}
	reqs := b.follows(t, b.admin, "/follows/requests", "requests")
	if len(reqs) != 2 || reqs[0].From != "a1@"+a.svc.Domain() || reqs[0].To != to("appr") || reqs[1].To != to("norow") {
		t.Errorf("b's requests: %+v", reqs)
	}

	v := followOf(t, a.followTo(a1, to("open")))
	if v.Status != "accepted" {
		t.Fatalf("follow open: %+v", v)
	}
	if again := followOf(t, a.followTo(a1, to("open"))); again != v {
		t.Errorf("following again: %+v, want %+v", again, v)
	}
	var feed string
	if err := a.db.QueryRow(`SELECT r.id FROM rooms r JOIN room_members m ON m.room_id = r.id AND m.key_id = ? AND m.rights = 'read'
		WHERE r.feed = ? AND r.home_domain = ? AND r.kind = 'room' AND r.archived_at IS NULL`, a1.KeyID, to("open"), b.svc.Domain()).Scan(&feed); err != nil {
		t.Fatalf("a's replica of the feed: %v", err)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms r JOIN room_members m ON m.room_id = r.id JOIN api_keys k ON k.id = m.key_id
		WHERE r.id = ? AND r.feed = ? AND r.home_domain IS NULL AND k.label = ? AND m.rights = 'read'`, feed, to("open"), "a1@"+a.svc.Domain()); n != 1 {
		t.Error("a1's stand-in is not a reader of b's feed")
	}
	if got := b.follows(t, b.admin, "/followers", "followers"); len(got) != 1 || got[0].From != "a1@"+a.svc.Domain() || got[0].Status != "accepted" {
		t.Errorf("b's followers: %+v", got)
	}
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'follow.request'`); n != 3 {
			t.Errorf("%s audited %d requests, want 3", g.svc.Domain(), n)
		}
	}
}

// Only bots of allowed peers follow: a server that has seen the
// follower's server, without allowing it, refuses; the follower's server
// asks only allowed peers.
func TestFollowRemote_SeenPeer(t *testing.T) {
	a, b := newLiveRig(t), newLiveRig(t)
	allow(t, a.svc, b.svc.id)
	b.svc.mode = modeOpen
	if _, err := b.svc.peers.RecordSeen(context.Background(), a.svc.Domain(), a.svc.id.Public); err != nil {
		t.Fatal(err)
	}
	a1 := a.followBot(t, "a1", "")
	b.followBot(t, "b1", "open")
	wantErr(t, a.followTo(a1, "b1@"+b.svc.Domain()), http.StatusForbidden, "peer_not_allowed")
	wantErr(t, a.followTo(a1, "b1@c.example"), http.StatusForbidden, "peer_not_allowed")
	wantErr(t, b.fedDo(signedReq(t, a.svc.id, b.svc.Domain(), http.MethodPost, "/fed/v1/follows/withdraw",
		map[string]string{"follower": "a1@" + a.svc.Domain(), "handle": "b1"})), http.StatusForbidden, "peer_not_allowed")
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM follows`) + count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
			t.Errorf("%s keeps %d follows and rooms", g.svc.Domain(), n)
		}
	}
}

// A server takes at most 100 follow requests per peer an hour.
func TestFollowRemote_IncomingLimit(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.followBot(t, "b1", "approve")
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g.svc.now = func() time.Time { return now }
	g.svc.limiters[h.id.Domain] = rate.NewLimiter(rate.Inf, 0) // the per-peer request limit is not under test
	for i := range 100 {
		if rec := h.follow(t, g, h.addr(fmt.Sprintf("x%d", i)), "b1"); rec.Code != http.StatusCreated {
			t.Fatalf("follow %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := h.follow(t, g, h.addr("x100"), "b1")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if n := audited(t, g, "follow.refused", "limit"); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	now = now.Add(time.Minute)
	if rec := h.follow(t, g, h.addr("x101"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("a minute later: %d %s", rec.Code, rec.Body)
	}
}

// The receiver takes followers of the signing server only, for bots of
// its own, and answers the same follower its current status: a rejected
// request reads as pending.
func TestFollowRemote_Receiver(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "")
	wantErr(t, h.follow(t, g, "x@c.example", "b1"), http.StatusBadRequest, "bad_request")
	wantErr(t, h.follow(t, g, "Bad name@"+h.id.Domain, "b1"), http.StatusBadRequest, "bad_request")
	wantErr(t, h.follow(t, g, h.addr("x"), "nobody"), http.StatusNotFound, "not_found")
	for range 2 {
		rec := h.follow(t, g, h.addr("x"), "b1")
		if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != `{"status":"pending"}` {
			t.Fatalf("follow: %d %s", rec.Code, rec.Body)
		}
	}
	reqs := g.follows(t, b1, "/follows/requests", "requests")
	if len(reqs) != 1 || reqs[0].From != h.addr("x") {
		t.Fatalf("requests: %+v", reqs)
	}
	if rec := g.do(b1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/reject", reqs[0].ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	if rec := h.follow(t, g, h.addr("x"), "b1"); strings.TrimSpace(rec.Body.String()) != `{"status":"pending"}` {
		t.Errorf("after the rejection: %d %s", rec.Code, rec.Body)
	}
	// An accepted follower's server asks again only when it lacks the
	// feed: it is invited again.
	h.setAnswer(answerWith(http.StatusCreated, `{"max_events_bytes":100000,"kind":"feed"}`))
	h.follow(t, g, h.addr("y"), "b1")
	reqs = g.follows(t, b1, "/follows/requests", "requests")
	if rec := g.do(b1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/accept", reqs[0].ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if rec := h.follow(t, g, h.addr("y"), "b1"); strings.TrimSpace(rec.Body.String()) != `{"status":"accepted"}` {
		t.Errorf("accepted again: %d %s", rec.Code, rec.Body)
	}
	invites := 0
	for _, r := range h.received() {
		if strings.Contains(r, "/invite ") && strings.Contains(r, `"kind":"feed"`) && strings.Contains(r, `"handle":"y"`) &&
			strings.Contains(r, `"feed":"b1@`+g.svc.Domain()+`"`) {
			invites++
		}
	}
	if invites != 2 {
		t.Errorf("%d feed invites of y, want 2", invites)
	}
}

// A feed invite needs a follow of the invited bot toward the feed's bot
// on the origin; with one, it makes a read-only replica that is the
// feed, and the follow is accepted. The host cannot add another bot of
// ours or raise the follower's rights.
func TestFollowRemote_FeedInvite(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "")
	b2 := g.followBot(t, "b2", "")
	room := ulid.Make().String()
	name := "feed:" + h.addr("x") + "#" + room // a feed whose name was taken on its host
	wantErr(t, h.feedInvite(t, g, room, name, "b1"), http.StatusForbidden, "invite_refused")
	follow := func(c auth.Claims, target string) {
		t.Helper()
		if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
			c.KeyID, target); err != nil {
			t.Fatal(err)
		}
	}
	follow(b1, "x@c.example") // a bot of another server than the origin
	wantErr(t, h.feedInvite(t, g, room, "feed:x@c.example", "b1"), http.StatusForbidden, "invite_refused")
	wantErr(t, h.feedInvite(t, g, room, "proj", "b1"), http.StatusBadRequest, "bad_request")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Fatalf("%d rooms after the refusals", n)
	}

	follow(b1, h.addr("x"))
	rec := h.feedInvite(t, g, room, name, "b1")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"kind":"feed"`) {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND home_domain = ? AND feed = ? AND kind = 'room' AND archived_at IS NULL`,
		room, h.id.Domain, h.addr("x")); n != 1 {
		t.Error("the replica is not the feed of x")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read'`, room, b1.KeyID); n != 1 {
		t.Error("b1 is not a reader of the replica")
	}
	if got := g.follows(t, b1, "/follows", "follows"); len(got) != 2 || got[1].To != h.addr("x") || got[1].Status != "accepted" {
		t.Errorf("b1's follows: %+v", got)
	}
	if rec := g.do(g.admin, http.MethodGet, "/rooms?kind=feed", ""); !strings.Contains(rec.Body.String(), room) {
		t.Errorf("feeds: %s", rec.Body)
	}
	h.push(t, g, room, memberEv(1, "b2@"+local, "read"), memberEv(2, "b1@"+local, "read", "status_update"))
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b2.KeyID); n != 0 {
		t.Error("a member event added b2 to the feed")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read'`, room, b1.KeyID); n != 1 {
		t.Error("a member event raised b1's rights")
	}
	wantErr(t, g.do(b1, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"hi","message_type":"status_update"}`),
		http.StatusForbidden, "missing_right")
	wantErr(t, h.feedInvite(t, g, room, name, "b2"), http.StatusForbidden, "invite_refused")
	wantErr(t, h.invite(t, g, room, "proj", "b1", "read"), http.StatusConflict, "conflict")
	// As on its host, an archived feed ends its accepted follows.
	h.push(t, g, room, roomEv(3, name, true))
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE target = ?`, h.addr("x")); n != 0 {
		t.Error("the follow of the archived feed is kept")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 1 {
		t.Error("the archive ended another follow")
	}
}

// A peer that answers a feed invite without "kind":"feed" does not know
// feeds: the host takes it as a refusal, and the follow stays pending.
func TestFollowRemote_InviteAnswerKind(t *testing.T) {
	a := newLiveRig(t)
	h := newFakeHost(t, a)
	a1 := a.followBot(t, "a1", "")
	if _, err := a.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain)
		VALUES (?, '', '', '2026-01-01T00:00:00Z', 'external', ?)`, h.addr("x"), h.id.Domain); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
		SELECT id, ?, ?, 'pending', '2026-11-14T00:00:00Z' FROM api_keys WHERE label = ?`, "a1@"+a.svc.Domain(), a1.KeyID, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	h.setAnswer(answerWith(http.StatusCreated, `{"room_id":"x","max_events_bytes":100000,"kind":"room"}`))
	reqs := a.follows(t, a1, "/follows/requests", "requests")
	wantErr(t, a.do(a1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/accept", reqs[0].ID), ""), http.StatusForbidden, "invite_refused")
	if n := count(t, a.db, `SELECT COUNT(*) FROM follows WHERE status = 'pending'`); n != 1 {
		t.Error("the follow is no longer pending")
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE k.remote_domain IS NOT NULL`); n != 0 {
		t.Error("the stand-in joined the feed")
	}
}

// The follower's server passes the peer's refusals on and keeps nothing;
// without an answer the follow stays pending, and the next follow sends
// it again.
func TestFollowRemote_PeerAnswers(t *testing.T) {
	a := newLiveRig(t)
	h := newFakeHost(t, a)
	a1 := a.followBot(t, "a1", "")
	pending := func() int {
		return count(t, a.db, `SELECT COUNT(*) FROM follows WHERE follower_key_id = ? AND target = ? AND status = 'pending'`, a1.KeyID, h.addr("x"))
	}
	h.setAnswer(answerWith(0, ""))
	if rec := a.followTo(a1, h.addr("x")); rec.Code != http.StatusBadGateway {
		t.Errorf("no answer: %d %s", rec.Code, rec.Body)
	}
	if pending() != 1 {
		t.Fatal("the follow is not kept pending")
	}
	h.setAnswer(answerWith(http.StatusCreated, `{"status":"pending"}`))
	if v := followOf(t, a.followTo(a1, h.addr("x"))); v.Status != "pending" {
		t.Errorf("sent again: %+v", v)
	}
	got := h.received()
	if len(got) != 2 || strings.TrimSpace(got[1]) != "POST /fed/v1/follows "+`{"follower":"a1@`+a.svc.Domain()+`","handle":"x"}` {
		t.Errorf("the host got %q", got)
	}
	// Withdrawing the pending follow tells the peer; without an answer
	// the follow goes all the same.
	h.setAnswer(answerWith(0, ""))
	if rec := a.do(a1, http.MethodDelete, "/follows/"+h.addr("x"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body)
	}
	if got := h.received(); len(got) != 3 ||
		strings.TrimSpace(got[2]) != "POST /fed/v1/follows/withdraw "+`{"follower":"a1@`+a.svc.Domain()+`","handle":"x"}` {
		t.Errorf("the host got %q", got)
	}
	if pending() != 0 {
		t.Fatal("the withdrawn follow is kept")
	}
	// A refusal is audited by its code: policy for follow_refused, peer
	// for peer_not_allowed, refused for any other 403.
	for _, c := range []struct {
		status       int
		code, reason string
	}{{http.StatusForbidden, "follow_refused", "policy"}, {http.StatusForbidden, "peer_not_allowed", "peer"},
		{http.StatusForbidden, "forbidden", "refused"}, {http.StatusNotFound, "not_found", "not a bot"},
		{http.StatusTooManyRequests, "rate_limited", "limit"}} {
		h.setAnswer(answerWith(c.status, `{"error":{"code":"`+c.code+`","message":"no"}}`))
		rec := a.followTo(a1, h.addr("x"))
		wantErr(t, rec, c.status, c.code)
		if c.status == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "3600" {
			t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
		}
		if pending() != 0 {
			t.Errorf("%d: the follow is kept", c.status)
		}
		if n := audited(t, a, "follow.refused", c.reason); n != 1 {
			t.Errorf("%s: %d refusals audited as %s, want 1", c.code, n, c.reason)
		}
	}
}

// B's bot follows A's bot, A's bot accepts and posts: the post is in B's
// timeline, and B's bot cannot post into the feed.
func TestFollowRemote_Timeline(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.followBot(t, "a1", "approve")
	b1 := b.followBot(t, "b1", "")
	to := "a1@" + a.svc.Domain()
	if v := followOf(t, b.followTo(b1, to)); v.Status != "pending" {
		t.Fatalf("follow: %+v", v)
	}
	reqs := a.follows(t, a1, "/follows/requests", "requests")
	if len(reqs) != 1 || reqs[0].From != "b1@"+b.svc.Domain() {
		t.Fatalf("a1's requests: %+v", reqs)
	}
	if v := followOf(t, a.do(a1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/accept", reqs[0].ID), "")); v.Status != "accepted" {
		t.Fatalf("accept: %+v", v)
	}
	if got := b.follows(t, b1, "/follows", "follows"); len(got) != 1 || got[0].Status != "accepted" {
		t.Fatalf("b1's follows: %+v", got)
	}
	if rec := a.do(a1, http.MethodPost, "/posts", `{"content":"hello followers"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)
	rec := b.do(b1, http.MethodGet, "/timeline", "")
	var tl struct {
		Posts []struct {
			RoomID        string `json:"room_id"`
			AuthorAddress string `json:"author_address"`
			Content       string `json:"content"`
			Visibility    string `json:"visibility"`
			TrustOrigin   string `json:"trust_origin"`
		} `json:"posts"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tl) != nil || len(tl.Posts) != 1 {
		t.Fatalf("b1's timeline: %d %s", rec.Code, rec.Body)
	}
	if p := tl.Posts[0]; p.Content != "hello followers" || p.AuthorAddress != to || p.Visibility != "followers" || p.TrustOrigin != "external" {
		t.Errorf("post: %+v", p)
	}
	wantErr(t, b.do(b1, http.MethodPost, "/rooms/"+tl.Posts[0].RoomID+"/messages", `{"content":"hi","message_type":"status_update"}`),
		http.StatusForbidden, "missing_right")
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
		t.Errorf("b refused %d batches", n)
	}
}

// openFollow has b's bot b1 follow a's bot a1, whose policy is open, and
// delivers a's feed to b; it returns the feed's id.
func openFollow(t *testing.T, a, b *roomRig) (a1, b1 auth.Claims, feed string) {
	t.Helper()
	a1, b1 = a.followBot(t, "a1", "open"), b.followBot(t, "b1", "")
	if v := followOf(t, b.followTo(b1, "a1@"+a.svc.Domain())); v.Status != "accepted" {
		t.Fatalf("follow: %+v", v)
	}
	a.svc.deliverDue(context.Background())
	if err := b.db.QueryRow(`SELECT id FROM rooms WHERE feed = ? AND archived_at IS NULL`, "a1@"+a.svc.Domain()).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	return a1, b1, feed
}

// sides checks that the follow of b1 toward a1 is gone on both servers:
// no follow row, no stand-in in a's feed, b's replica archived.
func sides(t *testing.T, a, b *roomRig, feed string) {
	t.Helper()
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 0 {
			t.Errorf("%s keeps %d follows", g.svc.Domain(), n)
		}
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.remote_domain IS NOT NULL`, feed); n != 0 {
		t.Error("b1's stand-in is still in a's feed")
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, feed); n != 1 {
		t.Error("b's replica is not archived")
	}
}

// Unfollowing leaves the replica through the host: the follow is gone on
// both servers, and following again brings the replica back.
func TestFollowRemote_Unfollow(t *testing.T) {
	a, b := dmPair(t)
	_, b1, feed := openFollow(t, a, b)
	if rec := b.do(b1, http.MethodDelete, "/follows/a1@"+a.svc.Domain(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unfollow: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(context.Background())
	sides(t, a, b, feed)
	wantErr(t, b.do(b1, http.MethodDelete, "/follows/a1@"+a.svc.Domain(), ""), http.StatusNotFound, "not_found")
	if v := followOf(t, b.followTo(b1, "a1@"+a.svc.Domain())); v.Status != "accepted" {
		t.Errorf("following again: %+v", v)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NULL`, feed); n != 1 {
		t.Error("the replica is not active again")
	}
}

// Removing a remote follower takes its stand-in out of the feed, which
// archives the replica; the follower's server then drops its follow.
func TestFollowRemote_RemoveFollower(t *testing.T) {
	a, b := dmPair(t)
	a1, _, feed := openFollow(t, a, b)
	if rec := a.do(a1, http.MethodDelete, "/followers/b1@"+b.svc.Domain(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(context.Background())
	sides(t, a, b, feed)
}

// A rejection tells nobody: the follower's side stays pending, and
// following again sends the request again, which still reads pending.
func TestFollowRemote_RejectedStaysPending(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.followBot(t, "a1", "approve")
	b1 := b.followBot(t, "b1", "")
	to := "a1@" + a.svc.Domain()
	followOf(t, b.followTo(b1, to))
	reqs := a.follows(t, a1, "/follows/requests", "requests")
	if v := followOf(t, a.do(a1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/reject", reqs[0].ID), "")); v.Status != "rejected" {
		t.Fatalf("reject: %+v", v)
	}
	if got := b.follows(t, b1, "/follows", "follows"); len(got) != 1 || got[0].Status != "pending" {
		t.Errorf("b1's follows: %+v", got)
	}
	if v := followOf(t, b.followTo(b1, to)); v.Status != "pending" {
		t.Errorf("following again: %+v", v)
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM follows WHERE status = 'rejected'`); n != 1 {
		t.Error("a's rejection changed")
	}
}

// A follower server's bots hold read only in the feed: the host refuses
// their proposals, so forwarding is no way around POST /v1/posts.
func TestFollowRemote_FollowerProposalRefused(t *testing.T) {
	a, b := dmPair(t)
	_, _, feed := openFollow(t, a, b)
	author := "b1@" + b.svc.Domain()
	for _, typ := range []string{"status_update", "question"} {
		nonce := newNonce()
		sig := b.svc.id.SignMessage(MessageSig{Room: feed + "@" + a.svc.Domain(), Author: author, ContentType: "text/plain",
			MessageType: typ, Content: "hi", Nonce: nonce})
		wantErr(t, a.fedDo(signedReq(t, b.svc.id, a.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+feed+"/messages",
			map[string]any{"author": author, "content": "hi", "content_type": "text/plain", "message_type": typ, "sig": sig, "nonce": nonce})),
			http.StatusForbidden, "missing_right")
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM messages WHERE room_id = ?`, feed); n != 0 {
		t.Errorf("%d messages in the feed", n)
	}
}

// A follower server learns the feed's bot and its own bots, never the
// other followers: member events about the host's followers or other
// servers' bots, the snapshot's included, reach it as skips.
func TestFollowRemote_FollowersPrivate(t *testing.T) {
	ctx := context.Background()
	a, b := dmPair(t)
	c := newLiveRig(t)
	allow(t, a.svc, c.svc.id)
	allow(t, c.svc, a.svc.id)
	a1, a2 := a.followBot(t, "a1", "open"), a.followBot(t, "a2", "")
	b1, c1 := b.followBot(t, "b1", ""), c.followBot(t, "c1", "")
	to := "a1@" + a.svc.Domain()
	for _, f := range []struct {
		g *roomRig
		c auth.Claims
	}{{a, a2}, {b, b1}, {c, c1}} {
		if v := followOf(t, f.g.followTo(f.c, to)); v.Status != "accepted" {
			t.Fatalf("follow: %+v", v)
		}
	}
	if rec := a.do(a1, http.MethodPost, "/posts", `{"content":"hi"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)
	if rec := c.do(c1, http.MethodDelete, "/follows/"+to, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unfollow: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)
	var feed string
	if err := a.db.QueryRow(`SELECT id FROM rooms WHERE feed = ?`, to).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	addrs := []string{to, "a2@" + a.svc.Domain(), "b1@" + b.svc.Domain(), "c1@" + c.svc.Domain()}
	for _, x := range []struct {
		g       *roomRig
		members string
		unknown []string
	}{{b, to + " b1", []string{addrs[1], addrs[3]}}, {c, to, []string{addrs[1], addrs[2]}}} {
		if got := members(t, x.g, feed); got != x.members {
			t.Errorf("%s's replica: %q, want %q", x.g.svc.Domain(), got, x.members)
		}
		for _, addr := range x.unknown {
			if n := count(t, x.g.db, `SELECT COUNT(*) FROM api_keys WHERE label = ?`, addr); n != 0 {
				t.Errorf("%s knows %s", x.g.svc.Domain(), addr)
			}
		}
		if n := count(t, x.g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
			t.Errorf("%s refused %d batches", x.g.svc.Domain(), n)
		}
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'hi'`, feed); n != 1 {
		t.Error("b lacks the post")
	}
}

// members lists the members of room on g: stand-ins by address, our
// bots by handle, sorted.
func members(t *testing.T, g *roomRig, room string) string {
	t.Helper()
	rows, err := g.db.Query(`SELECT CASE WHEN k.remote_domain IS NULL THEN k.handle ELSE k.label END AS a
		FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE m.room_id = ? ORDER BY a`, room)
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
	return strings.Join(out, " ")
}

// A bot takes at most 1000 pending requests: a request of a bot of
// another server beyond that is 429 with Retry-After 3600, and so is one
// whose stand-in the server cannot make.
func TestFollowRemote_PendingCap(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "approve")
	g.followBot(t, "b2", "approve")
	if _, err := g.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
		INSERT INTO api_keys(label, hash, prefix, created_at) SELECT 'k' || i, 'x', 'p' || i, '2026-01-01T00:00:00Z' FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
		SELECT id, ?, ?, 'pending', '2026-01-01T00:00:00Z' FROM api_keys WHERE label LIKE 'k%'`, "b1@"+g.svc.Domain(), b1.KeyID); err != nil {
		t.Fatal(err)
	}
	rec := h.follow(t, g, h.addr("x"), "b1")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") != "3600" {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	if n := audited(t, g, "follow.refused", "limit"); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	if _, err := g.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain)
		SELECT 's' || i || '@' || ?2, '', '', '2026-01-01T00:00:00Z', 'external', ?2 FROM n`, fedrooms.MaxStandIns, h.id.Domain); err != nil {
		t.Fatal(err)
	}
	rec = h.follow(t, g, h.addr("y"), "b2")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") != "3600" {
		t.Errorf("stand-in limit: Retry-After %q", rec.Header().Get("Retry-After"))
	}
}

// An archived replica of a feed ends the follows of our bots in it,
// not those of bots in a newer replica of the same feed.
func TestFollowRemote_OldReplicaArchived(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1, b2 := g.followBot(t, "b1", ""), g.followBot(t, "b2", "")
	for _, c := range []auth.Claims{b1, b2} {
		if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
			c.KeyID, h.addr("x")); err != nil {
			t.Fatal(err)
		}
	}
	old, newer, name := ulid.Make().String(), ulid.Make().String(), "feed:"+h.addr("x")
	for _, inv := range [][2]string{{old, "b1"}, {old, "b2"}, {newer, "b1"}} {
		if rec := h.feedInvite(t, g, inv[0], name, inv[1]); rec.Code != http.StatusCreated {
			t.Fatalf("invite %v: %d %s", inv, rec.Code, rec.Body)
		}
	}
	h.push(t, g, old, roomEv(1, name, true))
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, old); n != 1 {
		t.Fatal("the old replica is not archived")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE follower_key_id = ? AND status = 'accepted'`, b1.KeyID); n != 1 {
		t.Error("b1's follow, accepted into the newer replica, ended")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE follower_key_id = ?`, b2.KeyID); n != 0 {
		t.Error("b2's follow, in the archived replica only, is kept")
	}
}

// The feed's address travels in the invite: a long one, whose feed name
// its host cut, is followed all the same. The name must be the feed's,
// the address on the origin's domain; without the field the name is
// read as before.
func TestFollowRemote_LongFeedAddress(t *testing.T) {
	g := newRoomRig(t)
	long := testIdentity(t, strings.Repeat("d", 60)+"."+strings.Repeat("e", 40)+".example")
	allow(t, g.svc, long)
	b1 := g.followBot(t, "b1", "")
	addr := strings.Repeat("x", 32) + "@" + long.Domain
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1.KeyID, addr); err != nil {
		t.Fatal(err)
	}
	room := ulid.Make().String()
	name := fedrooms.Suffixed(fedrooms.FeedPrefix+addr, room)
	if len(name) != 128 || strings.Contains(name, addr) {
		t.Fatalf("name %q is not cut", name)
	}
	invite := func(name, feed string) *httptest.ResponseRecorder {
		body := map[string]any{"name": name, "handle": "b1", "rights": []string{"read"}, "kind": "feed"}
		if feed != "" {
			body["feed"] = feed
		}
		return g.fedDo(signedReq(t, long, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/invite", body))
	}
	wantErr(t, invite(name, ""), http.StatusForbidden, "invite_refused")
	wantErr(t, invite("feed:other", addr), http.StatusBadRequest, "bad_request")
	wantErr(t, invite(fedrooms.FeedPrefix+addr+"x", addr), http.StatusBadRequest, "bad_request")
	wantErr(t, invite("feed:y@c.example", "y@c.example"), http.StatusForbidden, "invite_refused")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Fatalf("%d rooms after the refusals", n)
	}
	if rec := invite(name, addr); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"kind":"feed"`) {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND feed = ? AND archived_at IS NULL`, room, addr); n != 1 {
		t.Error("the replica is not the feed of the long address")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE target = ? AND status = 'accepted'`, addr); n != 1 {
		t.Error("the follow is not accepted")
	}
}

// Blocking or removing a peer ends our bots' follows of its bots,
// pending and accepted; other follows stay.
func TestFollowRemote_PeerDropEndsFollows(t *testing.T) {
	for _, op := range []string{"block", "remove"} {
		t.Run(op, func(t *testing.T) {
			g := newRoomRig(t)
			h := newFakeHost(t, g)
			b1, b2 := g.followBot(t, "b1", ""), g.followBot(t, "b2", "")
			for _, f := range []struct {
				c      auth.Claims
				target string
				key    any
				status string
			}{{b1, h.addr("x"), nil, "accepted"}, {b1, h.addr("y"), nil, "pending"}, {b1, "b2@" + g.svc.Domain(), b2.KeyID, "pending"},
				{b2, "z@c.example", nil, "pending"}} {
				if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
					VALUES (?, ?, ?, ?, '2026-11-14T00:00:00Z')`, f.c.KeyID, f.target, f.key, f.status); err != nil {
					t.Fatal(err)
				}
			}
			if op == "block" {
				wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+h.id.Domain+"/block", ""), http.StatusOK)
			} else {
				wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, "/v1/federation/peers/"+h.id.Domain, ""), http.StatusNoContent)
			}
			if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE target LIKE ?`, "%@"+h.id.Domain); n != 0 {
				t.Errorf("%d follows of the peer's bots are kept", n)
			}
			if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 2 {
				t.Errorf("%d follows, want the 2 others", n)
			}
		})
	}
}

// Withdrawing a pending follow of a bot of another server deletes the
// request there too.
func TestFollowRemote_Withdraw(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.followBot(t, "a1", "approve")
	b1 := b.followBot(t, "b1", "")
	to := "a1@" + a.svc.Domain()
	if v := followOf(t, b.followTo(b1, to)); v.Status != "pending" {
		t.Fatalf("follow: %+v", v)
	}
	if got := a.follows(t, a1, "/follows/requests", "requests"); len(got) != 1 {
		t.Fatalf("a1's requests: %+v", got)
	}
	if rec := b.do(b1, http.MethodDelete, "/follows/"+to, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unfollow: %d %s", rec.Code, rec.Body)
	}
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 0 {
			t.Errorf("%s keeps %d follows", g.svc.Domain(), n)
		}
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM audit WHERE action = 'follow.remove'`); n != 1 {
		t.Errorf("a audited %d removals, want 1", n)
	}
}

// The receiver deletes a pending or rejected request of a bot of the
// signing server, never an accepted follow, within the peer's follow
// request limit.
func TestFollowRemote_WithdrawReceiver(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "")
	withdraw := func(follower string) *httptest.ResponseRecorder {
		return g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/follows/withdraw",
			map[string]string{"follower": follower, "handle": "b1"}))
	}
	for _, x := range []string{"x", "y", "z"} {
		if rec := h.follow(t, g, h.addr(x), "b1"); rec.Code != http.StatusCreated {
			t.Fatalf("follow %s: %d %s", x, rec.Code, rec.Body)
		}
	}
	reqs := g.follows(t, b1, "/follows/requests", "requests")
	if rec := g.do(b1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/reject", reqs[1].ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	h.setAnswer(answerWith(http.StatusCreated, `{"max_events_bytes":100000,"kind":"feed"}`))
	if rec := g.do(b1, http.MethodPost, fmt.Sprintf("/follows/requests/%d/accept", reqs[2].ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, withdraw("x@c.example"), http.StatusBadRequest, "bad_request")
	for _, x := range []string{"x", "y"} {
		if rec := withdraw(h.addr(x)); rec.Code != http.StatusNoContent {
			t.Errorf("withdraw %s: %d %s", x, rec.Code, rec.Body)
		}
	}
	wantErr(t, withdraw(h.addr("x")), http.StatusNotFound, "not_found")
	wantErr(t, withdraw(h.addr("z")), http.StatusNotFound, "not_found")
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows WHERE status = 'accepted'`); n != 1 || count(t, g.db, `SELECT COUNT(*) FROM follows`) != 1 {
		t.Errorf("follows left: %d accepted", n)
	}
	g.svc.mu.Lock()
	g.svc.followLimits[h.id.Domain] = rate.NewLimiter(0, 0)
	g.svc.mu.Unlock()
	wantErr(t, withdraw(h.addr("z")), http.StatusTooManyRequests, "rate_limited")
}
