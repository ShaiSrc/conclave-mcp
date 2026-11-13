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

	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// deliverDue makes one delivery pass and waits for the runs it started;
// it returns how long the worker would sleep after them: 0 while
// cursors are due.
func (s *Service) deliverDue(ctx context.Context) time.Duration {
	s.startDue(ctx)
	s.runs.Wait()
	_, peers, wait, err := s.plan(ctx, s.now())
	if err != nil {
		return deliveryIdle
	}
	if len(peers) > 0 {
		return 0
	}
	return wait
}

// pushes returns the events of each push m received for room.
func (m *memberServer) pushes(t *testing.T, room string) [][]fedrooms.Event {
	t.Helper()
	var out [][]fedrooms.Event
	prefix := "POST /fed/v1/rooms/" + room + "/events "
	for _, r := range m.received() {
		if body, ok := strings.CutPrefix(r, prefix); ok {
			var in struct{ Events []fedrooms.Event }
			if err := json.Unmarshal([]byte(body), &in); err != nil {
				t.Fatalf("push body: %v", err)
			}
			out = append(out, in.Events)
		}
	}
	return out
}

// cursorOf reads room's delivery row for peer.
func cursorOf(t *testing.T, g *roomRig, room, peer string) (acked int64, attempts int, next, failing string, paused bool) {
	t.Helper()
	if err := g.db.QueryRow(`SELECT acked_seq, attempts, COALESCE(next_try_at, ''), COALESCE(failing_since, ''), paused
		FROM fed_delivery WHERE room_id = ? AND peer = ?`, room, peer).Scan(&acked, &attempts, &next, &failing, &paused); err != nil {
		t.Fatal(err)
	}
	return
}

// fixClock makes g's federation clock return *now and its jitter j.
func fixClock(g *roomRig, now *time.Time, j float64) {
	g.svc.now = func() time.Time { return *now }
	g.svc.jitter = func() float64 { return j }
}

func TestDelivery_PushesAfterCursor(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	m.setReply(applyAll)
	for i := range 120 {
		if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"m`+fmt.Sprint(i)+` <&>"}`); rec.Code != http.StatusCreated {
			t.Fatalf("send: %d %s", rec.Code, rec.Body)
		}
	}
	total := int64(len(g.events(t, room)))
	ctx := context.Background()
	if wait := g.svc.deliverDue(ctx); wait != 0 {
		t.Errorf("more to send, but the worker would sleep %v", wait)
	}
	g.svc.deliverDue(ctx)
	got := m.pushes(t, room)
	if len(got) != 2 || len(got[0]) != maxRoomEvents || int64(len(got[0])+len(got[1])) != total {
		t.Fatalf("pushes: %d", len(got))
	}
	logged := g.events(t, room)
	for i, e := range append(got[0], got[1]...) {
		if e.Seq != int64(i+1) || string(e.Payload) != string(logged[i].Payload) {
			t.Fatalf("event %d: seq %d %s", i, e.Seq, e.Payload)
		}
	}
	// Payloads go as stored, <, > and & unescaped.
	if !strings.Contains(m.received()[len(m.received())-1], ` <&>"`) {
		t.Error("push body escapes HTML characters")
	}
	if acked, attempts, next, _, _ := cursorOf(t, g, room, m.id.Domain); acked != total || attempts != 0 || next != "" {
		t.Errorf("cursor: acked %d attempts %d next %q", acked, attempts, next)
	}
	// Nothing due: no request, and the worker sleeps a minute.
	if wait := g.svc.deliverDue(ctx); wait != deliveryIdle || len(m.pushes(t, room)) != 2 {
		t.Errorf("idle pass: wait %v, %d pushes", wait, len(m.pushes(t, room)))
	}
}

// The worker is woken right after each change of a federated room.
func TestDelivery_WokenByChanges(t *testing.T) {
	g, _, room, _ := fedRoom(t, "read,answer")
	drain := func() int {
		n := 0
		for {
			select {
			case <-g.svc.deliver:
				n++
			default:
				return n
			}
		}
	}
	if drain() != 1 {
		t.Error("invite did not wake the worker")
	}
	if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"x"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if drain() != 1 {
		t.Error("message did not wake the worker")
	}
}

func TestDelivery_Backoff(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	m.setReply(func(string, []byte) (int, string) { return http.StatusInternalServerError, "" })
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fixClock(g, &now, 1)
	ctx := context.Background()
	start := now
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 3600, 3600} {
		want *= time.Second
		wait := g.svc.deliverDue(ctx)
		_, attempts, next, failing, paused := cursorOf(t, g, room, m.id.Domain)
		if attempts != i+1 || next != storage.FormatTS(now.Add(want)) || failing != storage.FormatTS(start) || paused {
			t.Fatalf("attempt %d: attempts %d next %s failing %s, want next %s", i+1, attempts, next, failing, storage.FormatTS(now.Add(want)))
		}
		if wait != min(want, deliveryIdle) {
			t.Errorf("attempt %d: worker sleeps %v, want %v", i+1, wait, min(want, deliveryIdle))
		}
		// Not due before next_try_at.
		now = now.Add(want - time.Millisecond)
		g.svc.deliverDue(ctx)
		if _, a, _, _, _ := cursorOf(t, g, room, m.id.Domain); a != i+1 {
			t.Fatalf("attempt %d: retried before it was due", i+1)
		}
		now = now.Add(time.Millisecond)
	}
	if !strings.Contains(lastError(t, g, room), "500") {
		t.Errorf("last error: %q", lastError(t, g, room))
	}
	// Jitter: the wait is between half and all of the backoff.
	fixClock(g, &now, 0)
	g.svc.deliverDue(ctx)
	if _, _, next, _, _ := cursorOf(t, g, room, m.id.Domain); next != storage.FormatTS(now.Add(30*time.Minute)) {
		t.Errorf("jitter 0: next %s, want half an hour", next)
	}
	// A success resets the backoff.
	m.setReply(applyAll)
	now = now.Add(time.Hour)
	g.svc.deliverDue(ctx)
	if acked, attempts, next, failing, _ := cursorOf(t, g, room, m.id.Domain); acked == 0 || attempts != 0 || next != "" || failing != "" ||
		lastError(t, g, room) != "" {
		t.Errorf("after success: acked %d attempts %d next %q failing %q", acked, attempts, next, failing)
	}
}

func lastError(t *testing.T, g *roomRig, room string) string {
	t.Helper()
	var s string
	if err := g.db.QueryRow(`SELECT last_error FROM fed_delivery WHERE room_id = ?`, room).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDelivery_PausesAfter7Days(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	m.setReply(func(string, []byte) (int, string) { return http.StatusBadGateway, "" })
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fixClock(g, &now, 1)
	ctx := context.Background()
	start := now
	for now.Sub(start) < pauseAfter {
		g.svc.deliverDue(ctx)
		_, _, next, _, paused := cursorOf(t, g, room, m.id.Domain)
		if paused {
			t.Fatalf("paused after %v", now.Sub(start))
		}
		now, _ = time.Parse(time.RFC3339Nano, next)
	}
	g.svc.deliverDue(ctx)
	if _, _, _, _, paused := cursorOf(t, g, room, m.id.Domain); !paused {
		t.Fatal("not paused after 7 days")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.delivery.paused' AND target_id = ? AND room_id = ?`,
		m.id.Domain, room); n != 1 {
		t.Errorf("federation.delivery.paused rows: %d", n)
	}
	// A paused peer is not tried again.
	before := len(m.pushes(t, room))
	now = now.Add(30 * 24 * time.Hour)
	if wait := g.svc.deliverDue(ctx); len(m.pushes(t, room)) != before || wait != deliveryIdle {
		t.Errorf("paused peer was tried, or the worker waits %v for it", wait)
	}
	// Its next contact resumes delivery.
	m.setReply(applyAll)
	wantStatus(t, g.fedDo(m.req(t, http.MethodPost, "/fed/v1/ping", nil)), http.StatusOK)
	if _, attempts, _, failing, paused := cursorOf(t, g, room, m.id.Domain); paused || attempts != 0 || failing != "" {
		t.Errorf("after contact: paused %v attempts %d failing %q", paused, attempts, failing)
	}
	g.svc.deliverDue(ctx)
	if acked, _, _, _, _ := cursorOf(t, g, room, m.id.Domain); acked != int64(len(g.events(t, room))) {
		t.Errorf("not delivered after resuming: acked %d", acked)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status %d %s, want %d", rec.Code, rec.Body, status)
	}
}

// A peer that does not answer costs one request per pass, not one per
// room.
func TestDelivery_DeadPeerOncePerPass(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	other := g.room(t, "other")
	g.invite(t, other, m.addr("c1"), "read")
	m.setReply(func(string, []byte) (int, string) { return 0, "" })
	before := len(m.received())
	g.svc.deliverDue(context.Background())
	if n := len(m.received()) - before; n != 1 {
		t.Errorf("%d requests to a dead peer in one pass", n)
	}
	for _, r := range []string{room, other} {
		if _, attempts, _, _, _ := cursorOf(t, g, r, m.id.Domain); attempts != 1 {
			t.Errorf("room %s: attempts %d", r, attempts)
		}
	}
}

// A peer's rooms go in room id order, each pass starting after the
// rooms the last one attempted. With every room due on every pass and
// a budget that allows one push a pass, each pass attempts a new room:
// all N rooms within N passes.
func TestDelivery_FairOrder(t *testing.T) {
	g, m, first, _ := fedRoom(t, "read")
	rooms := []string{first}
	for i := range 3 {
		r := g.room(t, fmt.Sprint("r", i))
		g.invite(t, r, m.addr("c1"), "read")
		rooms = append(rooms, r)
	}
	// Every push outlasts the budget and fails: a pass attempts one
	// room, and every room stays due.
	g.svc.budget = 300 * time.Millisecond
	m.setReply(func(string, []byte) (int, string) {
		time.Sleep(time.Second)
		return http.StatusInternalServerError, ""
	})
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fixClock(g, &now, 0)
	ctx := context.Background()
	for pass := 1; pass <= len(rooms); pass++ {
		g.svc.startDue(ctx)
		g.svc.runs.Wait()
		now = now.Add(2 * time.Hour) // past any backoff
		if n := count(t, g.db, `SELECT COUNT(*) FROM fed_delivery WHERE peer = ? AND attempts > 0`, m.id.Domain); n != pass {
			t.Fatalf("after pass %d: %d of %d rooms attempted, want %d", pass, n, len(rooms), pass)
		}
	}
}

// A member server gets the events up to its last bot's removal, then
// nothing more of the room.
func TestDelivery_StopsAfterLastMemberLeft(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read")
	m.setReply(applyAll)
	// Another member server keeps the room logged.
	m2 := newMemberServer(t, g)
	m2.setReply(applyAll)
	g.invite(t, room, m2.addr("d1"), "read")
	ctx := context.Background()
	g.svc.deliverDue(ctx)
	if rec := g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, c1), ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	removal := int64(len(g.events(t, room)))
	if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"after"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	g.svc.deliverDue(ctx)
	g.svc.deliverDue(ctx)
	got := m.pushes(t, room)
	last := got[len(got)-1]
	if e := last[len(last)-1]; e.Seq != removal || e.Type != fedrooms.EventMemberRemove {
		t.Errorf("last event pushed: %d %s, want the removal %d", e.Seq, e.Type, removal)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM fed_delivery WHERE room_id = ? AND peer = ?`, room, m.id.Domain); n != 0 {
		t.Errorf("delivery row kept: %d", n)
	}
	got2 := m2.pushes(t, room)
	if last := got2[len(got2)-1]; last[len(last)-1].Seq != removal+1 {
		t.Errorf("the other member server got up to %d, want %d", last[len(last)-1].Seq, removal+1)
	}
	// Invited again, it gets the log again; its replica skips what it
	// applied.
	g.invite(t, room, m.addr("c1"), "read")
	g.svc.deliverDue(ctx)
	got = m.pushes(t, room)
	if last := got[len(got)-1]; last[len(last)-1].Seq != int64(len(g.events(t, room))) {
		t.Errorf("after the new invite: last pushed %d of %d", last[len(last)-1].Seq, len(g.events(t, room)))
	}
}

// A member that answers less than it was sent is retried later, from
// what it applied.
func TestDelivery_PartialApply(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	m.setReply(func(string, []byte) (int, string) { return http.StatusOK, `{"applied":1}` })
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fixClock(g, &now, 1)
	g.svc.deliverDue(context.Background())
	if acked, attempts, next, _, _ := cursorOf(t, g, room, m.id.Domain); acked != 1 || attempts != 1 || next == "" {
		t.Errorf("cursor: acked %d attempts %d next %q", acked, attempts, next)
	}
}

// Blocking or removing a member server's domain takes its bots out of
// the rooms hosted here and pauses delivery to it.
func TestPeerDrop_Host(t *testing.T) {
	for _, op := range []string{"block", "remove"} {
		t.Run(op, func(t *testing.T) {
			g, m, room, c1 := fedRoom(t, "read")
			other := g.room(t, "other")
			c2 := g.invite(t, other, m.addr("c2"), "read")
			var rec *httptest.ResponseRecorder
			if op == "block" {
				rec = adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+m.id.Domain+"/block", "")
				wantStatus(t, rec, http.StatusOK)
			} else {
				rec = adminDo(g.svc, admin, http.MethodDelete, "/v1/federation/peers/"+m.id.Domain, "")
				wantStatus(t, rec, http.StatusNoContent)
			}
			for r, key := range map[string]int64{room: c1, other: c2} {
				if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, r, key); n != 0 {
					t.Errorf("room %s: the stand-in is still a member", r)
				}
				evs := g.events(t, r)
				if last := evs[len(evs)-1]; last.Type != fedrooms.EventMemberRemove || !strings.Contains(string(last.Payload), m.id.Domain) {
					t.Errorf("room %s: last event %s %s", r, last.Type, last.Payload)
				}
				if _, _, _, _, paused := cursorOf(t, g, r, m.id.Domain); !paused {
					t.Errorf("room %s: delivery not paused", r)
				}
			}
		})
	}
}
