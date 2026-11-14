package federation

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// Guards of a replica against its host: what it pulls, which events it
// takes, and how our bots get out.

// refusedCount is the count of the last federation.room.event_refused
// row of g, 0 for none.
func refusedCount(t *testing.T, g *roomRig) int {
	t.Helper()
	return count(t, g.db, `SELECT COALESCE((SELECT json_extract(detail, '$.count') FROM audit
		WHERE action = 'federation.room.event_refused' ORDER BY id DESC LIMIT 1), 0)`)
}

// A push past a gap pulls two pages at most: the host's next push, from
// the seq the replica answered, brings the rest.
func TestReplicaPush_PullBounded(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	var log []fedrooms.Event
	for seq := int64(1); seq <= 1000; seq++ {
		log = append(log, msg(seq, 1000+seq, room, h.id.Domain, h.addr("h1"), "m", nil, "", nil))
	}
	h.setLog(room, log...)
	if got := h.push(t, g, room, log[999]); got != 2*maxRoomEvents {
		t.Errorf("applied %d, want %d", got, 2*maxRoomEvents)
	}
	if n := len(h.pulled()); n != 2 {
		t.Errorf("%d pulls, want 2", n)
	}
	if got := h.push(t, g, room, log[2*maxRoomEvents:3*maxRoomEvents]...); got != 3*maxRoomEvents {
		t.Errorf("the next push applied up to %d, want %d", got, 3*maxRoomEvents)
	}
}

// A replica pulls once at a time: a push's gap while a pull of the
// replica runs pulls nothing more.
func TestReplicaPush_OnePullAtATime(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	m1, m2 := msg(1, 11, room, h.id.Domain, h.addr("h1"), "one", nil, "", nil), msg(2, 12, room, h.id.Domain, h.addr("h1"), "two", nil, "", nil)
	h.setLog(room, m1, m2)
	gate := make(chan struct{})
	h.mu.Lock()
	h.gate = gate
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.svc.pull(context.Background(), room, h.id.Domain, maxPullPages, &refusals{})
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, ok := g.svc.pulling.Load(room); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pull did not start")
		}
	}
	pushed := make(chan int64, 1)
	go func() { pushed <- h.push(t, g, room, m2) }()
	select {
	case got := <-pushed:
		if got != 0 {
			t.Errorf("the push applied %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("the push waited for a second pull")
	}
	close(gate)
	<-done
	if n := len(h.pulled()); n != 2 { // the running pull's page and its empty next page
		t.Errorf("%d pulls, want 2", n)
	}
}

// An archived replica takes no events: a push is answered as applied,
// so the host's cursor moves on, but stores nothing, adds no member and
// pulls nothing. Its host cannot bring back a bot that left.
func TestReplicaPush_Archived(t *testing.T) {
	g, h, room, b1 := replica(t, "read")
	h.setAnswer(answerWith(http.StatusNoContent, ""))
	if rec := g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, b1), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body)
	}
	h.setLog(room, roomEv(1, "proj", false))
	if got := h.push(t, g, room, memberEv(4, "b1@"+local, "read"), msg(5, 15, room, h.id.Domain, h.addr("h1"), "hi", nil, "", nil)); got != 5 {
		t.Errorf("applied %d, want 5", got)
	}
	if n := len(h.pulled()); n != 0 {
		t.Errorf("%d pulls", n)
	}
	if count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ?`, room) != 0 ||
		count(t, g.db, `SELECT COUNT(*) FROM messages WHERE room_id = ?`, room) != 0 ||
		count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, room) != 1 {
		t.Error("the archived replica took the push")
	}
}

// A room event cannot give a replica of a room a name reserved for
// direct conversations or feeds.
func TestReplicaEvents_ReservedNames(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	if got := h.push(t, g, room, roomEv(1, "dm:b1@"+local+"~"+h.addr("x"), false), roomEv(2, "feed:"+h.addr("x"), false),
		roomEv(3, "renamed", false), roomEv(4, "dm:x", true)); got != 4 {
		t.Fatalf("applied %d", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = 'renamed' AND archived_at IS NULL`, room); n != 1 {
		t.Error("a reserved name was applied")
	}
	if n := refusedCount(t, g); n != 3 {
		t.Errorf("%d events refused, want 3", n)
	}
}

// A direct replica takes the two bots of its pair only, as members and
// as authors, and keeps its name: its host cannot swap the other bot
// for a third one, of its own server or another.
func TestDirectRemote_PairOnly(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	room, name := ulid.Make().String(), h.pairOf(g, "b1")
	if rec := h.directInvite(t, g, room, name, "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	ceo := "ceo@c.example"
	direct := func(seq int64, name string) fedrooms.Event {
		return event(seq, fedrooms.EventRoom, fedrooms.RoomPayload{Name: name, Kind: kindDirect})
	}
	if got := h.push(t, g, room,
		memberEv(1, h.addr("x"), "read", "answer"),
		msg(2, 12, room, h.id.Domain, h.addr("x"), "from x", nil, "", nil),
		removeEv(3, h.addr("x")),
		memberEv(4, ceo, "read", "answer"),
		memberEv(5, h.addr("y"), "read", "answer"),
		msg(6, 16, room, h.id.Domain, ceo, "from the ceo", nil, "", nil),
		msg(7, 17, room, h.id.Domain, h.addr("y"), "from y", nil, "", nil),
		direct(8, dmName("b1@"+local, ceo)),
		direct(9, fedrooms.Suffixed(name, room)), // its host's name, taken there: no rename
	); got != 9 {
		t.Fatalf("applied %d", got)
	}
	for _, addr := range []string{ceo, h.addr("y")} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE label = ?`, addr); n != 0 {
			t.Errorf("a stand-in of %s was made", addr)
		}
	}
	if got := storedMessages(t, g, room); len(got) != 1 || got[0].Content != "from x" {
		t.Errorf("stored %+v, want x's message only", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = ? AND pair = ?`, room, name, name); n != 1 {
		t.Error("the conversation was renamed")
	}
	if n := refusedCount(t, g); n != 5 {
		t.Errorf("%d events refused, want 5", n)
	}
	for _, c := range g.dmList(t, auth.Claims{KeyID: b1.KeyID}) {
		if c.Address != h.addr("x") {
			t.Errorf("listed with %s", c.Address)
		}
	}
}

// A feed replica takes its bot only, as remote member and as author, and
// keeps its name.
func TestFollowRemote_FeedBotOnly(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.followBot(t, "b1", "")
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1.KeyID, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	room, name := ulid.Make().String(), "feed:"+h.addr("x")
	if rec := h.feedInvite(t, g, room, name, "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if got := h.push(t, g, room,
		memberEv(1, h.addr("x"), "read", "status_update"),
		memberEv(2, h.addr("y"), "read"),
		msg(3, 13, room, h.id.Domain, h.addr("x"), "post", nil, "", nil),
		msg(4, 14, room, h.id.Domain, h.addr("y"), "not the feed's", nil, "", nil),
		roomEv(5, "renamed", false),
		roomEv(6, fedrooms.Suffixed(name, room), false),
	); got != 6 {
		t.Fatalf("applied %d", got)
	}
	if got := members(t, g, room); got != "b1 "+h.addr("x") {
		t.Errorf("members %q", got)
	}
	if got := storedMessages(t, g, room); len(got) != 1 || got[0].Content != "post" {
		t.Errorf("stored %+v, want the feed's post only", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = ?`, room, name); n != 1 {
		t.Error("the feed was renamed")
	}
	if n := refusedCount(t, g); n != 3 {
		t.Errorf("%d events refused, want 3", n)
	}
}
