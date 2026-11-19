package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Replicas keyed by host and room ID
// (docs/superpowers/specs/2026-11-18-replica-keys-room-invites-design.md).

// roomWithID makes room id, named name, hosted on g and owned by its
// admin key: as a server does that picks an id another one has.
func (g *roomRig) roomWithID(t *testing.T, id, name string) {
	t.Helper()
	now := storage.FormatTS(time.Now())
	if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility) VALUES (?, ?, ?, ?, 'private')`,
		id, name, g.admin.KeyID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'owner', ?, ?)`,
		id, g.admin.KeyID, now, rights.All().Storage()); err != nil {
		t.Fatal(err)
	}
}

// localOf returns g's id of host's room wire: wire itself with no
// remote_id, or an id of g's with remote_id wire.
func localOf(t *testing.T, g *roomRig, host, wire string) string {
	t.Helper()
	var id string
	if err := g.db.QueryRow(`SELECT id FROM rooms WHERE home_domain = ? AND (id = ?2 AND remote_id IS NULL OR remote_id = ?2)`,
		host, wire).Scan(&id); err != nil {
		t.Fatalf("no replica of %s@%s on %s: %v", wire, host, g.svc.Domain(), err)
	}
	return id
}

// contents lists the contents of room's messages on g, in order.
func contents(t *testing.T, g *roomRig, room string) []string {
	t.Helper()
	var out []string
	for _, m := range storedMessages(t, g, room) {
		out = append(out, m.Content)
	}
	return out
}

// Two hosts invite our bots under one room ID: each room gets a replica
// of its own, the second under an id made here, and each takes only its
// own host's events, messages, claims, catch-up, removal, leave and
// purge. Author signatures name the host's id: B, which hosts the id
// itself, keeps C's room under an id of its own too, and its bot's
// message there verifies on C and on A.
func TestReplicaKeys_TwoHostsOneID(t *testing.T) {
	ctx := context.Background()
	a, b, c := newLiveRig(t), newLiveRig(t), newLiveRig(t)
	for _, p := range [][2]*roomRig{{a, b}, {b, a}, {a, c}, {c, a}, {b, c}, {c, b}} {
		allow(t, p[0].svc, p[1].svc.id())
	}
	A, B, C := a.svc.Domain(), b.svc.Domain(), c.svc.Domain()
	a1, a2 := auth.Claims{KeyID: a.bot(t, "a1")}, auth.Claims{KeyID: a.bot(t, "a2")}
	b2 := auth.Claims{KeyID: b.bot(t, "b2")}
	room := b.room(t, "on-b")
	c.roomWithID(t, room, "on-c")
	hb, hc := b.localBot(t, room, "hb", "read,answer"), c.localBot(t, room, "hc", "read,answer")
	for _, h := range []*roomRig{b, c} {
		h.invite(t, room, "a1@"+A, "read,answer,claim")
		h.invite(t, room, "a2@"+A, "read,answer")
	}
	c.invite(t, room, "b2@"+B, "read,answer")
	deliver := func() { b.svc.deliverDue(ctx); c.svc.deliverDue(ctx) }
	deliver()

	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND home_domain = ? AND remote_id IS NULL AND name = 'on-b'`, room, B); n != 1 {
		t.Fatal("B's room is not replicated under its id")
	}
	onC, bOnC := localOf(t, a, C, room), localOf(t, b, C, room)
	if onC == room || bOnC == room {
		t.Fatalf("C's room took the id of B's: %s on A, %s on B", onC, bOnC)
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = 'on-c'`, onC); n != 1 {
		t.Error("A's replica of C's room is not named for it")
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND result != 'ok'`); n != 0 {
		t.Errorf("%d refused invites audited", n)
	}
	// The room view shows the host's id where it differs.
	if rec := a.do(a1, http.MethodGet, "/rooms/"+onC, ""); !strings.Contains(rec.Body.String(), `"remote_id":"`+room+`"`) {
		t.Errorf("view of C's replica: %d %s", rec.Code, rec.Body)
	}
	if rec := a.do(a1, http.MethodGet, "/rooms/"+room, ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "remote_id") {
		t.Errorf("view of B's replica: %d %s", rec.Code, rec.Body)
	}
	if rec := a.do(a1, http.MethodGet, "/rooms", ""); strings.Count(rec.Body.String(), `"remote_id":"`+room+`"`) != 1 {
		t.Errorf("room list: %s", rec.Body)
	}

	// Each host's messages, and our bots' writes through it, land in its
	// own replica only.
	post(t, b, hb, room, `{"content":"b: hello","message_type":"answer"}`)
	post(t, c, hc, room, `{"content":"c: hello","message_type":"answer"}`)
	deliver()
	post(t, a, a1, room, `{"content":"a1 to b","message_type":"answer"}`)
	post(t, a, a1, onC, `{"content":"a1 to c","message_type":"answer"}`)
	post(t, b, b2, bOnC, `{"content":"b2 to c","message_type":"answer"}`)
	deliver()
	wantB, wantC := []string{"b: hello", "a1 to b"}, []string{"c: hello", "a1 to c", "b2 to c"}
	for _, s := range []struct {
		g    *roomRig
		room string
		want []string
	}{{b, room, wantB}, {a, room, wantB}, {c, room, wantC}, {a, onC, wantC}, {b, bOnC, wantC}} {
		if got := contents(t, s.g, s.room); !slices.Equal(got, s.want) {
			t.Errorf("%s on %s: %q, want %q", s.room, s.g.svc.Domain(), got, s.want)
		}
	}
	// b2's author signature names C's id: A verifies it in its replica.
	for _, m := range storedMessages(t, a, onC) {
		if m.Content == "b2 to c" && (m.Label != "b2@"+B || m.Unverified || m.OriginSig == "") {
			t.Errorf("b2's message on A: %+v", m)
		}
	}

	// A claim in C's replica is held on C, in C's room.
	if rec := a.do(a1, http.MethodPost, "/rooms/"+onC+"/claims", `{"paths":["src/"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if n, m := count(t, c.db, `SELECT COUNT(*) FROM claims WHERE room_id = ?`, room), count(t, b.db, `SELECT COUNT(*) FROM claims`); n != 1 || m != 0 {
		t.Errorf("claims: %d on C, %d on B", n, m)
	}
	if rec := a.do(a1, http.MethodGet, "/rooms/"+onC+"/claims", ""); !strings.Contains(rec.Body.String(), `"path":"src/"`) {
		t.Errorf("claims of C's replica: %s", rec.Body)
	}
	if rec := a.do(a1, http.MethodGet, "/rooms/"+room+"/claims", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "src/") {
		t.Errorf("claims of B's replica: %d %s", rec.Code, rec.Body)
	}

	// Catch-up pulls C's room into C's replica.
	if _, err := a.db.Exec(`DELETE FROM messages WHERE room_id = ?`, onC); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE rooms SET applied_seq = 0 WHERE id = ?`, onC); err != nil {
		t.Fatal(err)
	}
	a.svc.catchUp(ctx)
	if got := contents(t, a, onC); !slices.Equal(got, wantC) {
		t.Errorf("C's replica after catch-up: %q", got)
	}
	if got := contents(t, a, room); !slices.Equal(got, wantB) {
		t.Errorf("B's replica after catch-up: %q", got)
	}

	// B removes a2: it leaves B's replica only.
	var a2OnB int64
	if err := b.db.QueryRow(`SELECT id FROM api_keys WHERE label = ?`, "a2@"+A).Scan(&a2OnB); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, b.do(b.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, a2OnB), ""), http.StatusNoContent)
	deliver()
	member := func(g *roomRig, room string, key int64) bool {
		return count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, key) == 1
	}
	if member(a, room, a2.KeyID) || !member(a, onC, a2.KeyID) {
		t.Error("B's removal of a2 reached the wrong replica")
	}

	// a1 leaves C's replica: C lets it go, B keeps it.
	wantStatus(t, a.do(a.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", onC, a1.KeyID), ""), http.StatusNoContent)
	standIn := func(g *roomRig) int64 {
		var id int64
		if err := g.db.QueryRow(`SELECT id FROM api_keys WHERE label = ?`, "a1@"+A).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if member(c, room, standIn(c)) || !member(b, room, standIn(b)) || !member(a, room, a1.KeyID) {
		t.Error("a1's leave of C's room reached the wrong room")
	}

	// Purging C deletes C's replica and leaves B's.
	a.blockPeer(t, C)
	wantStatus(t, a.purge(C), http.StatusOK)
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE id = ?`, onC); n != 0 {
		t.Error("C's replica outlived the purge")
	}
	if got := contents(t, a, room); !slices.Equal(got, wantB) {
		t.Errorf("B's replica after the purge: %q", got)
	}
}

// A replica with an id of its own checks author signatures and the
// suffixed name of a feed against its host's id, and an existing
// replica, with remote_id NULL, works as before.
func TestReplicaKeys_SignaturesAndNames(t *testing.T) {
	g := newRoomRig(t)
	h, other := newFakeHost(t, g), newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "")
	room, feedID := ulid.Make().String(), ulid.Make().String()
	for _, id := range []string{room, feedID} {
		if rec := other.invite(t, g, id, "first-"+id, "b1", "read"); rec.Code != http.StatusCreated {
			t.Fatalf("other host's invite: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := h.invite(t, g, room, "proj", "b1", "read"); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"room_id":"`+room+`"`) {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	onH := localOf(t, g, h.id.Domain, room)
	if onH == room {
		t.Fatal("the room took the other host's replica's id")
	}
	c := testIdentity(t, "c.example")
	allow(t, g.svc, c)
	host := h.id.Domain
	// A third server's signature verifies for the host's id; one of ours
	// for our id here is refused.
	if got := h.push(t, g, room, msg(1, 1, room, host, "c1@c.example", "signed for the host's id", nil, "", c),
		msg(2, 2, onH, host, "b1@"+local, "signed for our id", nil, "", g.svc.id())); got != 2 {
		t.Fatalf("push applied %d", got)
	}
	if got := storedMessages(t, g, onH); len(got) != 1 || got[0].Content != "signed for the host's id" || got[0].Unverified {
		t.Errorf("messages: %+v", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused' AND room_id = ? AND detail LIKE '%"count":1%'`,
		onH); n != 1 {
		t.Error("not exactly the message signed for our id was refused")
	}

	// A feed's name is suffixed with its host's id.
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1.KeyID, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	name := fedrooms.Suffixed(fedrooms.FeedPrefix+h.addr("x"), feedID)
	if rec := h.feedInvite(t, g, feedID, name, "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("feed invite: %d %s", rec.Code, rec.Body)
	}
	feed := localOf(t, g, host, feedID)
	if got := h.push(t, g, feedID, roomEv(1, name, true)); got != 1 {
		t.Fatalf("push applied %d", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, feed); n != 1 ||
		count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused' AND room_id = ?`, feed) != 0 {
		t.Error("the feed's archive under its suffixed name was refused")
	}

	// The other host's replicas, as 0.14 stored them, take their pushes.
	for _, id := range []string{room, feedID} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND home_domain = ? AND remote_id IS NULL AND archived_at IS NULL`,
			id, other.id.Domain); n != 1 {
			t.Errorf("the other host's replica %s changed", id)
		}
	}
	if got := other.push(t, g, room, msg(1, 1, room, other.id.Domain, other.addr("o1"), "old replica", nil, "", nil)); got != 1 {
		t.Fatalf("push to the old replica applied %d", got)
	}
	if got := contents(t, g, room); !slices.Equal(got, []string{"old replica"}) {
		t.Errorf("old replica: %q", got)
	}
}

// A feed under an id that another host's replica holds here: the follow
// is accepted into a replica of its own, posts arrive there, and
// unfollowing leaves through the feed's id on its host; so does a block
// of the feed's bot after following again.
func TestReplicaKeys_FeedUnderTakenID(t *testing.T) {
	ctx := context.Background()
	a, b := dmPair(t)
	A := a.svc.Domain()
	h := newFakeHost(t, b)
	b.bot(t, "b9")
	invite := a.follow.InviteRemote
	a.follow.InviteRemote = func(ctx context.Context, roomID, name, address string, set []string) (int, error) {
		if count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ?`, roomID) == 0 { // another host takes the feed's id on b first
			if rec := h.invite(t, b, roomID, "squat", "b9", "read"); rec.Code != http.StatusCreated {
				t.Errorf("squat: %d %s", rec.Code, rec.Body)
			}
		}
		return invite(ctx, roomID, name, address, set)
	}
	a1 := a.followBot(t, "a1", "open")
	b1 := b.followBot(t, "b1", "")
	to := "a1@" + A
	if v := followOf(t, b.followTo(b1, to)); v.Status != "accepted" {
		t.Fatalf("follow: %+v", v)
	}
	a.svc.deliverDue(ctx)
	var feed string
	if err := a.db.QueryRow(`SELECT id FROM rooms WHERE feed = ? AND home_domain IS NULL`, to).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	onB := localOf(t, b, A, feed)
	if onB == feed || count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND home_domain = ?`, feed, h.id.Domain) != 1 {
		t.Fatalf("feed %s on b: %s", feed, onB)
	}
	if rec := a.do(a1, http.MethodPost, "/posts", `{"content":"hello followers"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)
	var tl struct {
		Posts []struct {
			RoomID  string `json:"room_id"`
			Content string `json:"content"`
		} `json:"posts"`
	}
	if rec := b.do(b1, http.MethodGet, "/timeline", ""); json.Unmarshal(rec.Body.Bytes(), &tl) != nil || len(tl.Posts) != 1 ||
		tl.Posts[0].RoomID != onB || tl.Posts[0].Content != "hello followers" {
		t.Fatalf("b1's timeline: %d %s", rec.Code, rec.Body)
	}
	followers := func() int {
		return count(t, a.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id
			WHERE m.room_id = ? AND k.remote_domain IS NOT NULL`, feed)
	}
	wantStatus(t, b.do(b1, http.MethodDelete, "/follows/"+to, ""), http.StatusNoContent)
	if n := followers(); n != 0 {
		t.Errorf("the unfollow left %d followers in a's feed", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NULL`, feed); n != 1 {
		t.Error("the unfollow touched the other host's replica")
	}

	if v := followOf(t, b.followTo(b1, to)); v.Status != "accepted" {
		t.Fatalf("follow again: %+v", v)
	}
	if again := localOf(t, b, A, feed); again != onB || followers() != 1 {
		t.Fatalf("followed again into %s with %d followers", again, followers())
	}
	b.block(t, to)
	if n := followers(); n != 0 {
		t.Errorf("the block left %d followers in a's feed", n)
	}
}

// A direct conversation under an id taken here gets a replica of its
// own, and both bots write to it.
func TestReplicaKeys_DirectUnderTakenID(t *testing.T) {
	ctx := context.Background()
	a, b := dmPair(t)
	A, B := a.svc.Domain(), b.svc.Domain()
	a1 := a.dmBot(t, "a1", "peers")
	b1 := b.dmBot(t, "b1", "peers")
	h := newFakeHost(t, a)
	a.bot(t, "a9")
	invite := b.direct.InviteRemote
	b.direct.InviteRemote = func(ctx context.Context, roomID, name, address string, set []string) (int, error) {
		if rec := h.invite(t, a, roomID, "squat", "a9", "read"); rec.Code != http.StatusCreated {
			t.Errorf("squat: %d %s", rec.Code, rec.Body)
		}
		return invite(ctx, roomID, name, address, set)
	}
	room := dmRoom(t, b.dm(b1, "a1@"+A, "hello a1"))
	b.svc.deliverDue(ctx)
	onA := localOf(t, a, B, room)
	if onA == room {
		t.Fatal("the conversation took the other host's replica's id")
	}
	if again := dmRoom(t, a.dm(a1, "b1@"+B, "hello b1")); again != onA {
		t.Errorf("a1 answered in %s, want %s", again, onA)
	}
	b.svc.deliverDue(ctx)
	want := []string{"hello a1", "hello b1"}
	if got, on := contents(t, b, room), contents(t, a, onA); !slices.Equal(got, want) || !slices.Equal(on, want) {
		t.Errorf("conversation: %q on b, %q on a", got, on)
	}
}

// When both bots start at once, the room with the lower id is the
// pair's: the host's id counts, also for a replica with an id of its
// own here, and the bot's next message goes to that replica's host.
func TestReplicaKeys_LowerRoomByHostID(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	lower, mine := ulid.Make().String(), ulid.Make().String()
	if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind, pair)
		VALUES (?, ?, ?, '2026-11-14T00:00:00Z', 'private', 'direct', ?)`, mine, h.pairOf(g, "b1"), b1.KeyID, h.pairOf(g, "b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights)
		VALUES (?, ?, 'reader', '2026-11-14T00:00:00Z', 'read')`, mine, b1.KeyID); err != nil {
		t.Fatal(err)
	}
	g.roomWithID(t, lower, "squat")
	if rec := h.directInvite(t, g, lower, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite into the lower room: %d %s", rec.Code, rec.Body)
	}
	if onG := localOf(t, g, h.id.Domain, lower); onG < mine {
		t.Fatalf("the replica's id %s is below %s: the test shows nothing", onG, mine)
	}
	g.dm(b1, h.addr("x"), "hi") // the fake host answers 404
	if got := h.received(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /fed/v1/rooms/"+lower+"/messages ") {
		t.Errorf("b1's message went to %q", got)
	}
}

// A direct invite under the id of the pair's conversation hosted here is
// refused as the other start of the same conversation: the host's id is
// not lower, so it does not win, and no second conversation of the pair
// begins under a local id of its own.
func TestReplicaKeys_DirectInviteUnderOurID(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	mine := ulid.Make().String()
	if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind, pair)
		VALUES (?, ?, ?, '2026-11-14T00:00:00Z', 'private', 'direct', ?)`, mine, h.pairOf(g, "b1"), b1.KeyID, h.pairOf(g, "b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights)
		VALUES (?, ?, 'reader', '2026-11-14T00:00:00Z', 'read')`, mine, b1.KeyID); err != nil {
		t.Fatal(err)
	}
	if rec := h.directInvite(t, g, mine, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusConflict {
		t.Fatalf("invite under our conversation's id: %d %s, want 409", rec.Code, rec.Body)
	}
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM rooms WHERE pair = ? AND archived_at IS NULL`, h.pairOf(g, "b1")).Scan(&n); err != nil || n != 1 {
		t.Errorf("active conversations of the pair: %d %v, want 1", n, err)
	}
}
