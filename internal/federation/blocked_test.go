package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// Blocking single remote bots (docs/superpowers/specs/2026-11-14-moderation-operations-design.md).

const blockedPath = "/v1/federation/blocked-bots"

// block blocks address on g through the admin API.
func (g *roomRig) block(t *testing.T, address string) {
	t.Helper()
	if rec := adminDo(g.svc, admin, http.MethodPost, blockedPath, fmt.Sprintf(`{"address":%q,"note":"spam"}`, address)); rec.Code != http.StatusCreated {
		t.Fatalf("block %s: %d %s", address, rec.Code, rec.Body)
	}
}

// blockRow blocks address on g without the cleanup of a block, so a
// guard alone decides.
func blockRow(t *testing.T, g *roomRig, address string) {
	t.Helper()
	if _, err := g.db.Exec(`INSERT INTO blocked_bots(address, created_at) VALUES (?, '2026-11-14T00:00:00.000000000Z')`, address); err != nil {
		t.Fatal(err)
	}
}

// blockAudit counts g's federation.bot.block rows of address whose
// detail holds part.
func blockAudit(t *testing.T, g *roomRig, address, part string) int {
	t.Helper()
	return count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.bot.block' AND target_id = ? AND detail LIKE ?`,
		address, "%"+part+"%")
}

func TestBlockedBots_API(t *testing.T) {
	g := newRoomRig(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		return adminDo(g.svc, admin, method, path, body)
	}
	post := func(address string) *httptest.ResponseRecorder {
		return do(http.MethodPost, blockedPath, fmt.Sprintf(`{"address":%q,"note":"spam"}`, address))
	}
	for _, bad := range []string{"", "c1", "c1@", "@a.example", "admin@a.example", "directory@a.example", "c 1@a.example",
		"c1@a_b.example", "c1@a.example:443", "c1@x@a.example"} {
		wantErr(t, post(bad), http.StatusBadRequest, "invalid_address")
	}
	wantErr(t, post("c1@"+g.svc.Domain()), http.StatusBadRequest, "own_domain")
	wantErr(t, do(http.MethodPost, blockedPath, "{"), http.StatusBadRequest, "invalid_json")
	wantErr(t, do(http.MethodDelete, blockedPath+"/c1", ""), http.StatusBadRequest, "invalid_address")
	wantErr(t, do(http.MethodDelete, blockedPath+"/c1@a.example", ""), http.StatusNotFound, "not_found")
	wantErr(t, adminDo(g.svc, []string{"read"}, http.MethodGet, blockedPath, ""), http.StatusForbidden, "forbidden")
	if n := count(t, g.db, `SELECT COUNT(*) FROM blocked_bots`) + count(t, g.db, `SELECT COUNT(*) FROM audit`); n != 0 {
		t.Fatalf("refusals left %d rows", n)
	}

	type row struct {
		Address   string `json:"address"`
		Note      string `json:"note"`
		CreatedAt string `json:"created_at"`
		CreatedBy int64  `json:"created_by"`
	}
	rowOf := func(rec *httptest.ResponseRecorder, status int) row {
		t.Helper()
		var r row
		if rec.Code != status || json.Unmarshal(rec.Body.Bytes(), &r) != nil {
			t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, status)
		}
		return r
	}
	if r := rowOf(post("C1@A.Example"), http.StatusCreated); r.Address != "c1@a.example" || r.Note != "spam" || r.CreatedAt == "" || r.CreatedBy != 1 {
		t.Errorf("blocked: %+v", r)
	}
	if r := rowOf(post("c1@a.example"), http.StatusOK); r.Address != "c1@a.example" {
		t.Errorf("blocked again: %+v", r)
	}
	list := func() string {
		t.Helper()
		rec := do(http.MethodGet, blockedPath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got := list(); !strings.HasPrefix(got, `{"blocked_bots":[{"address":"c1@a.example","note":"spam","created_at":"`) {
		t.Errorf("list: %s", got)
	}
	if n := blockAudit(t, g, "c1@a.example", `"note":"spam"`); n != 2 {
		t.Errorf("%d federation.bot.block rows, want 2", n)
	}
	if rec := do(http.MethodDelete, blockedPath+"/C1@a.example", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, do(http.MethodDelete, blockedPath+"/c1@a.example", ""), http.StatusNotFound, "not_found")
	if got := list(); got != `{"blocked_bots":[]}` {
		t.Errorf("list after unblock: %s", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.bot.unblock' AND target_id = 'c1@a.example'`); n != 1 {
		t.Errorf("%d federation.bot.unblock rows, want 1", n)
	}
}

// Blocking revokes the stand-in and takes it out of every room hosted
// here, with member.remove events that reach the other member servers;
// unblocking lifts the revocation and restores no membership.
func TestBlockBot_LeavesHostedRooms(t *testing.T) {
	ctx := context.Background()
	g, m, room, c1 := fedRoom(t, "read,answer")
	m3 := newMemberServer(t, g)
	g.invite(t, room, m3.addr("d1"), "read")
	other := g.room(t, "other")
	g.invite(t, other, m.addr("c1"), "read")
	m.setReply(applyAll)
	m3.setReply(applyAll)
	g.svc.deliverDue(ctx)

	g.block(t, m.addr("c1"))
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE id = ? AND revoked_at IS NOT NULL`, c1); n != 1 {
		t.Error("the stand-in is not revoked")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE key_id = ?`, c1); n != 0 {
		t.Errorf("the stand-in is in %d rooms", n)
	}
	removal := `{"address":"` + m.addr("c1") + `"}`
	for _, r := range []string{room, other} {
		if evs := g.events(t, r); evs[len(evs)-1].Type != fedrooms.EventMemberRemove || string(evs[len(evs)-1].Payload) != removal {
			t.Errorf("last event of %s: %+v", r, evs[len(evs)-1])
		}
	}
	g.svc.deliverDue(ctx)
	got := false
	for _, evs := range m3.pushes(t, room) {
		for _, e := range evs {
			got = got || e.Type == fedrooms.EventMemberRemove && string(e.Payload) == removal
		}
	}
	if !got {
		t.Error("the third member server did not get the removal")
	}
	if n := blockAudit(t, g, m.addr("c1"), `"rooms":2`); n != 1 {
		t.Errorf("%d federation.bot.block rows with 2 rooms, want 1", n)
	}

	if rec := adminDo(g.svc, admin, http.MethodDelete, blockedPath+"/"+m.addr("c1"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE id = ? AND revoked_at IS NULL`, c1); n != 1 {
		t.Error("the stand-in is still revoked")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE key_id = ?`, c1); n != 0 {
		t.Errorf("unblocking brought back %d memberships", n)
	}
	if again := g.invite(t, room, m.addr("c1"), "read"); again != c1 {
		t.Errorf("invited again as key %d, want the stand-in %d", again, c1)
	}
}

// Blocking a bot of another server ends its follows of our bots,
// accepted (it leaves the feed, which its server applies) and pending.
func TestBlockBot_EndsItsFollows(t *testing.T) {
	ctx := context.Background()
	a, b := dmPair(t)
	_, b1, feed := openFollow(t, a, b)
	a.followBot(t, "a2", "approve")
	b2 := b.followBot(t, "b2", "")
	if v := followOf(t, b.followTo(b2, "a2@"+a.svc.Domain())); v.Status != "pending" {
		t.Fatalf("follow of a2: %+v", v)
	}
	a.block(t, "b1@"+b.svc.Domain())
	a.block(t, "b2@"+b.svc.Domain())
	a.svc.deliverDue(ctx)
	if n := count(t, a.db, `SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("a keeps %d follows", n)
	}
	if got := members(t, a, feed); got != "a1" {
		t.Errorf("a's feed: %s", got)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM follows WHERE follower_key_id = ?`, b1.KeyID); n != 0 {
		t.Error("b keeps b1's follow")
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, feed); n != 1 {
		t.Error("b's replica is not archived")
	}
	if blockAudit(t, a, "b1@"+b.svc.Domain(), `"rooms":1`) != 1 || blockAudit(t, a, "b2@"+b.svc.Domain(), `"follows":1`) != 1 {
		t.Error("the block audit rows lack the counts")
	}
}

// Blocking a bot of another server ends our bots' follows of it: an
// accepted one leaves the replica of its feed, a pending one is
// withdrawn, and its server is told; unblocking restores neither.
func TestBlockBot_EndsOurFollows(t *testing.T) {
	ctx := context.Background()
	a, b := dmPair(t)
	_, _, feed := openFollow(t, a, b)
	a.followBot(t, "a2", "approve")
	b2 := b.followBot(t, "b2", "")
	if v := followOf(t, b.followTo(b2, "a2@"+a.svc.Domain())); v.Status != "pending" {
		t.Fatalf("follow of a2: %+v", v)
	}
	b.block(t, "a1@"+a.svc.Domain())
	b.block(t, "a2@"+a.svc.Domain())
	// b's side ends at once, before a's removal events arrive.
	if n := count(t, b.db, `SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("b keeps %d follows", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ?`, feed); n != 0 {
		t.Errorf("b's replica keeps %d members", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, feed); n != 1 {
		t.Error("b's replica is not archived")
	}
	a.svc.deliverDue(ctx)
	sides(t, a, b, feed)
	if blockAudit(t, b, "a1@"+a.svc.Domain(), `"replicas":1`) != 1 || blockAudit(t, b, "a2@"+a.svc.Domain(), `"follows":1`) != 1 {
		t.Error("the block audit rows lack the counts")
	}
	for _, h := range []string{"a1", "a2"} {
		if rec := adminDo(b.svc, admin, http.MethodDelete, blockedPath+"/"+h+"@"+a.svc.Domain(), ""); rec.Code != http.StatusNoContent {
			t.Fatalf("unblock: %d %s", rec.Code, rec.Body)
		}
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("unblocking brought back %d follows", n)
	}
}

// A blocked bot's message and claims proposals are refused; it may
// still leave.
func TestBlockedBot_RoomProposals(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer,claim")
	blockRow(t, g, m.addr("c1"))
	before := g.state(t)
	claims := "/fed/v1/rooms/" + room + "/claims?author=" + m.addr("c1")
	for _, req := range []*http.Request{
		m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/messages", m.proposal(room, m.addr("c1"), "answer", "hi", nil, nil)),
		m.req(t, http.MethodGet, claims, nil),
		m.req(t, http.MethodPost, claims, map[string]any{"paths": []string{"x/"}}),
	} {
		wantErr(t, g.fedDo(req), http.StatusForbidden, "bot_blocked")
	}
	if got := g.state(t); got != before {
		t.Errorf("refused proposals stored something: %s, want %s", got, before)
	}
	if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/leave", map[string]string{"author": m.addr("c1")})); rec.Code != http.StatusNoContent {
		t.Errorf("leave: %d %s", rec.Code, rec.Body)
	}
}

// A blocked bot's follow requests and withdrawals are refused.
func TestBlockedBot_FollowRequests(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	g.followBot(t, "b1", "open")
	blockRow(t, g, m.addr("c1"))
	for _, path := range []string{"/fed/v1/follows", "/fed/v1/follows/withdraw"} {
		wantErr(t, g.fedDo(m.req(t, http.MethodPost, path, map[string]string{"follower": m.addr("c1"), "handle": "b1"})),
			http.StatusForbidden, "bot_blocked")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows`) + count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE remote_domain IS NOT NULL`); n != 0 {
		t.Errorf("refused requests left %d rows", n)
	}
	if n := audited(t, g, "follow.refused", "blocked"); n != 2 {
		t.Errorf("%d follow.refused rows for the block, want 2", n)
	}
}

// A direct invite from a blocked bot and the feed invite of one are
// refused.
func TestBlockedBot_Invites(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.dmBot(t, "b1", "peers")
	blockRow(t, g, h.addr("x"))
	wantErr(t, h.directInvite(t, g, ulid.Make().String(), h.pairOf(g, "b1"), "b1"), http.StatusForbidden, "bot_blocked")
	wantErr(t, h.feedInvite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), "b1"), http.StatusForbidden, "bot_blocked")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`) + count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE remote_domain IS NOT NULL`); n != 0 {
		t.Errorf("refused invites left %d rows", n)
	}
	if n := audited(t, g, "direct.refused", "blocked"); n != 1 {
		t.Errorf("%d direct.refused rows for the block, want 1", n)
	}
}

// A replica skips the events of a blocked bot, and member events adding
// it, as refused ones.
func TestBlockedBot_ReplicaEvents(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	g.block(t, h.addr("x"))
	if got := h.push(t, g, room, memberEv(1, h.addr("x"), "read"), msg(2, 1, room, h.id.Domain, h.addr("x"), "hi", nil, "", nil),
		memberEv(3, h.addr("y"), "read")); got != 3 {
		t.Fatalf("applied %d, want 3", got)
	}
	if got := members(t, g, room); got != "b1 "+h.addr("y") {
		t.Errorf("members: %s", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM messages WHERE room_id = ?`, room) +
		count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE label = ?`, h.addr("x")); n != 0 {
		t.Errorf("the blocked bot's events left %d rows", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused' AND detail LIKE '%"count":2%'`); n != 1 {
		t.Error("the refusals are not audited")
	}
}

// Our bots cannot invite, start a conversation with, follow or look up a
// blocked bot; its server is not asked.
func TestBlockedBot_LocalPaths(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "proj")
	b1 := g.dmBot(t, "b1", "")
	g.block(t, m.addr("c1"))
	wantErr(t, g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+m.addr("c1")+`","rights":["read"]}`),
		http.StatusForbidden, "bot_blocked")
	wantErr(t, g.dm(b1, m.addr("c1"), "hi"), http.StatusForbidden, "bot_blocked")
	wantErr(t, g.followTo(b1, m.addr("c1")), http.StatusForbidden, "bot_blocked")
	wantErr(t, lookup(g.svc, g.db, b1, m.addr("c1")), http.StatusNotFound, "not_found")
	if got := m.received(); len(got) != 0 {
		t.Errorf("the member server was asked: %q", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 1 {
		t.Errorf("%d rooms, want 1", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows`) + count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE remote_domain IS NOT NULL`); n != 0 {
		t.Errorf("refusals left %d rows", n)
	}
	if audited(t, g, "direct.refused", "blocked") != 1 || audited(t, g, "follow.refused", "blocked") != 1 {
		t.Error("the refusals are not audited")
	}
}

// operator@ and key-<id>@ address keys of another server too, so they
// can be blocked, in any case of ASCII letters; a host's events of them
// are then refused. Other reserved names are no bot's.
func TestBlockedBots_ReservedNames(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	post := func(address string) *httptest.ResponseRecorder {
		return adminDo(g.svc, admin, http.MethodPost, blockedPath, fmt.Sprintf(`{"address":%q}`, address))
	}
	for raw, want := range map[string]string{"Operator@" + h.id.Domain: h.addr("operator"), "KEY-3@" + h.id.Domain: h.addr("key-3")} {
		var out blockedBot
		if rec := post(raw); rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Address != want {
			t.Errorf("block %s: %d %s, want %s", raw, rec.Code, rec.Body, want)
		}
	}
	wantErr(t, post("admin@a.example"), http.StatusBadRequest, "invalid_address")
	wantErr(t, post("key-3@"+g.svc.Domain()), http.StatusBadRequest, "own_domain")
	if got := h.push(t, g, room, memberEv(1, h.addr("operator"), "read"), memberEv(2, h.addr("key-3"), "read")); got != 2 {
		t.Fatalf("applied %d, want 2", got)
	}
	if got := members(t, g, room); got != "b1" {
		t.Errorf("members: %s", got)
	}
	wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, blockedPath+"/operator@"+h.id.Domain, ""), http.StatusNoContent)
	if n := count(t, g.db, `SELECT COUNT(*) FROM blocked_bots`); n != 1 {
		t.Errorf("%d blocked bots after the unblock, want 1", n)
	}
}

// Claims do not outlive a membership: a bot block, a peer block and a
// member removal delete the claims the member held in the room hosted
// here; another member's stay.
func TestClaims_EndWithMembership(t *testing.T) {
	for _, end := range []string{"bot block", "peer block", "member removal"} {
		g, m, room, c1 := fedRoom(t, "read,answer,claim")
		if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/claims?author="+m.addr("c1"),
			map[string]any{"paths": []string{"x/"}})); rec.Code/100 != 2 {
			t.Fatalf("%s: claim: %d %s", end, rec.Code, rec.Body)
		}
		l1 := g.localBot(t, room, "l1", "read,claim")
		if rec := g.do(l1, http.MethodPost, "/rooms/"+room+"/claims", `{"paths":["y/"]}`); rec.Code/100 != 2 {
			t.Fatalf("%s: local claim: %d %s", end, rec.Code, rec.Body)
		}
		switch end {
		case "bot block":
			g.block(t, m.addr("c1"))
		case "peer block":
			g.blockPeer(t, m.id.Domain)
		case "member removal":
			wantStatus(t, g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, c1), ""), http.StatusNoContent)
		}
		if n := count(t, g.db, `SELECT COUNT(*) FROM claims WHERE key_id = ?`, c1); n != 0 {
			t.Errorf("%s: c1 keeps %d claims", end, n)
		}
		if n := count(t, g.db, `SELECT COUNT(*) FROM claims WHERE key_id = ?`, l1.KeyID); n != 1 {
			t.Errorf("%s: l1 has %d claims, want 1", end, n)
		}
	}
	// A local member's removal ends its claims too.
	g, _, room, _ := fedRoom(t, "read")
	l1 := g.localBot(t, room, "l1", "read,claim")
	if rec := g.do(l1, http.MethodPost, "/rooms/"+room+"/claims", `{"paths":["y/"]}`); rec.Code/100 != 2 {
		t.Fatalf("local claim: %d %s", rec.Code, rec.Body)
	}
	wantStatus(t, g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, l1.KeyID), ""), http.StatusNoContent)
	if n := count(t, g.db, `SELECT COUNT(*) FROM claims WHERE key_id = ?`, l1.KeyID); n != 0 {
		t.Errorf("l1 keeps %d claims", n)
	}
}
