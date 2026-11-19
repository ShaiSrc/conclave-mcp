package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/bots"
)

// A bot whose room invites are none answers a room invite from another
// server exactly as an unknown bot; the audit row tells them apart. A
// bot with peers takes it, and direct and feed invites of a bot with
// none still follow its DM policy and its follows.
func TestRoomInvites_None(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	g.bot(t, "b2")
	ctx, none, peers := context.Background(), bots.InvitesNone, bots.Peers
	if _, err := g.svc.botStore.Update(ctx, "b1", bots.Patch{RoomInvites: &none, Visibility: &peers}, true); err != nil {
		t.Fatal(err)
	}
	room := ulid.Make().String()
	unknown := h.invite(t, g, room, "proj", "nobody", "read")
	refused := h.invite(t, g, room, "proj", "b1", "read")
	wantErr(t, unknown, http.StatusNotFound, "not_found")
	if refused.Code != unknown.Code || refused.Body.String() != unknown.Body.String() {
		t.Errorf("refused: %d %s, unknown: %d %s", refused.Code, refused.Body, unknown.Code, unknown.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms after the refusal", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND target_id = ? AND room_id = ?
		AND result = 'error:room_invites' AND detail LIKE '%"handle":"b1"%'`, h.id.Domain, room); n != 1 {
		t.Errorf("room_invites audit rows: %d", n)
	}
	if rec := h.invite(t, g, room, "proj", "b2", "read"); rec.Code != http.StatusCreated {
		t.Errorf("b2 (peers): %d %s", rec.Code, rec.Body)
	}

	if rec := h.directInvite(t, g, ulid.Make().String(), h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("direct invite: %d %s", rec.Code, rec.Body)
	}
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1.KeyID, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	if rec := h.feedInvite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("feed invite: %d %s", rec.Code, rec.Body)
	}

	// Peers never read the setting.
	for _, path := range []string{"/fed/v1/bots/b1", "/fed/v1/directory"} {
		rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"handle":"b1"`) || strings.Contains(rec.Body.String(), "room_invites") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// A host cannot bring a bot that refuses room invites into a room by a
// member event either. A bot in the room before keeps its membership,
// and the host still sets its rights.
func TestRoomInvites_NoneMemberEvent(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	b3 := g.bot(t, "b3")
	if rec := h.invite(t, g, room, "proj", "b3", "read"); rec.Code != http.StatusCreated {
		t.Fatalf("invite b3: %d %s", rec.Code, rec.Body)
	}
	b4 := g.bot(t, "b4")
	none := bots.InvitesNone
	for _, b := range []string{"b3", "b4"} {
		if _, err := g.svc.botStore.Update(context.Background(), b, bots.Patch{RoomInvites: &none}, true); err != nil {
			t.Fatal(err)
		}
	}
	d := g.svc.Domain()
	if got := h.push(t, g, room, memberEv(1, "b4@"+d, "read"), memberEv(2, "b3@"+d, "read", "answer")); got != 2 {
		t.Fatalf("applied %d", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b4); n != 0 {
		t.Error("a member event brought b4 in")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read,answer'`, room, b3); n != 1 {
		t.Error("b3's rights were not set")
	}
}

// A bot whose room invites are none answers every refusal to another
// server exactly as that path answers an unknown handle: status, body
// and headers. The audit row keeps the real reason. What its policies
// accept still goes through.
func TestRoomInvites_NoneRefusalsLookUnknown(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.bot(t, "b1")                 // DM policy local, follow policy approve
	g.dmBot(t, "b2", bots.DMPeers) // takes direct conversations from peers
	ctx, none, followNone := context.Background(), bots.InvitesNone, bots.FollowNone
	for _, b := range []string{"b1", "b2"} {
		if _, err := g.svc.botStore.Update(ctx, b, bots.Patch{RoomInvites: &none}, true); err != nil {
			t.Fatal(err)
		}
	}
	same := func(what string, unknown, got *httptest.ResponseRecorder) {
		t.Helper()
		if got.Code != unknown.Code || got.Body.String() != unknown.Body.String() || !reflect.DeepEqual(got.Header(), unknown.Header()) {
			t.Errorf("%s: %d %v %s, unknown: %d %v %s", what, got.Code, got.Header(), got.Body, unknown.Code, unknown.Header(), unknown.Body)
		}
	}
	direct := func(handle string) *httptest.ResponseRecorder {
		return h.directInvite(t, g, ulid.Make().String(), h.pairOf(g, handle), handle)
	}
	feed := func(handle string) *httptest.ResponseRecorder {
		return h.feedInvite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), handle)
	}

	same("direct, DM policy local", direct("nobody"), direct("b1"))
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'direct.refused' AND result = 'error:dm_refused'
		AND detail = '{"reason":"policy","to":"b1"}'`); n != 1 {
		t.Errorf("DM policy audit rows: %d", n)
	}
	same("feed, no follow", feed("nobody"), feed("b1"))
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND result = 'error:invite_refused'
		AND detail LIKE '%"handle":"b1"%'`); n != 1 {
		t.Errorf("feed audit rows: %d", n)
	}
	unknownFollow := h.follow(t, g, h.addr("x"), "nobody")
	if rec := h.follow(t, g, h.addr("x"), "b1"); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Errorf("follow, policy approve: %d %s", rec.Code, rec.Body)
	}
	if _, err := g.svc.botStore.Update(ctx, "b2", bots.Patch{FollowPolicy: &followNone}, true); err != nil {
		t.Fatal(err)
	}
	same("follow, policy none", unknownFollow, h.follow(t, g, h.addr("x"), "b2"))
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'follow.refused' AND result = 'error:follow_refused'
		AND detail LIKE '%"reason":"policy"%'`); n != 1 {
		t.Errorf("follow policy audit rows: %d", n)
	}

	// b2 takes direct conversations: one goes through, but over the
	// limit, or from a blocked bot, it is refused like any other.
	if rec := direct("b2"); rec.Code != http.StatusCreated {
		t.Fatalf("direct, DM policy peers: %d %s", rec.Code, rec.Body)
	}
	lims, err := g.svc.limitersOf(ctx, h.id.Domain)
	if err != nil {
		t.Fatal(err)
	}
	lims.dmInvites = rate.NewLimiter(0, 0)
	same("direct, over the limit", direct("nobody"), h.directInvite(t, g, ulid.Make().String(), dmName("b2@"+g.svc.Domain(), h.addr("y")), "b2"))
	if _, err := g.db.Exec(`INSERT INTO blocked_bots(address, created_at) VALUES (?, '2026-11-14T00:00:00.000000000Z')`, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	same("direct, blocked", direct("nobody"), direct("b2"))
}
