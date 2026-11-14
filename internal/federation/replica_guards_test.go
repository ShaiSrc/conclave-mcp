package federation

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// Guards of a replica against its host: what it pulls, which events it
// takes, and how our bots get out.

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
