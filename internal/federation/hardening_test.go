package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// A message deleted on the host is retracted: its event in the log
// keeps no content, a message.delete event follows, member servers
// delete their copy, and one invited later never gets the content.
func TestMessageDelete_Retracted(t *testing.T) {
	a, b, room, b1 := pair(t, "read,answer")
	ctx := context.Background()
	a1 := a.localBot(t, room, "a1-key", "read,answer")
	gone := post(t, a, a1, room, `{"content":"secret plan","message_type":"answer","metadata":{"k":"secret"}}`)
	post(t, a, a1, room, `{"content":"kept","message_type":"answer"}`)
	a.svc.deliverDue(ctx)
	if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'secret plan'`, room); n != 1 {
		t.Fatalf("b has %d copies before the delete", n)
	}
	wantStatus(t, a.do(a.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/messages/%d", room, gone), ""), http.StatusNoContent)
	if n := count(t, a.db, `SELECT COUNT(*) FROM fed_events WHERE room_id = ? AND payload LIKE '%secret%'`, room); n != 0 {
		t.Errorf("%d events still carry the content", n)
	}
	evs := a.events(t, room)
	if last := evs[len(evs)-1]; last.Type != fedrooms.EventMessageDelete || string(last.Payload) != fmt.Sprintf(`{"id":%d}`, gone) {
		t.Errorf("last event: %s %s", last.Type, last.Payload)
	}
	a.svc.deliverDue(ctx)
	rec := b.do(b1, http.MethodGet, "/rooms/"+room+"/messages", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret plan") || !strings.Contains(rec.Body.String(), "kept") {
		t.Errorf("list on b: %d %s", rec.Code, rec.Body)
	}
	if applied, total := appliedSeq(t, b, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("b applied %d of %d", applied, total)
	}

	c := newLiveRig(t)
	allow(t, a.svc, c.svc.id)
	allow(t, c.svc, a.svc.id)
	c.bot(t, "c1")
	a.invite(t, room, "c1@"+c.svc.Domain(), "read")
	a.svc.deliverDue(ctx)
	if n := count(t, c.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'kept'`, room); n != 1 {
		t.Errorf("c has %d copies of the kept message", n)
	}
	if n := count(t, c.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND (content LIKE '%secret%' OR metadata LIKE '%secret%')`, room); n != 0 {
		t.Errorf("c got the deleted content")
	}
	if applied, total := appliedSeq(t, c, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("c applied %d of %d", applied, total)
	}
	if n := count(t, c.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
		t.Errorf("c refused %d batches: a deleted message is no bad event", n)
	}
}

// Message events go only to peers with a reader in the room; others get
// skips, so seqs stay contiguous. A message.delete goes to every member
// server, so one that lost read still deletes its copy. Read granted
// later brings the messages logged after it.
func TestDelivery_MessagesOnlyToReaders(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "claim")
	m.setReply(applyAll)
	ctx := context.Background()
	bot := g.localBot(t, room, "local-bot", "read,answer")
	id := post(t, g, bot, room, `{"content":"before read","message_type":"answer"}`)
	wantStatus(t, g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/messages/%d", room, id), ""), http.StatusNoContent)
	post(t, g, bot, room, `{"content":"also before","message_type":"answer"}`)
	g.svc.deliverDue(ctx)
	var skips, deletes int
	for _, push := range m.pushes(t, room) {
		for _, e := range push {
			switch e.Type {
			case fedrooms.EventMessage:
				t.Errorf("event %d %s sent to a peer without read", e.Seq, e.Type)
			case fedrooms.EventMessageDelete:
				deletes++
				if string(e.Payload) != fmt.Sprintf(`{"id":%d}`, id) {
					t.Errorf("message.delete %d: %s", e.Seq, e.Payload)
				}
			case fedrooms.EventSkip:
				skips++
				if len(e.Payload) != 0 {
					t.Errorf("skip %d has a payload", e.Seq)
				}
			}
		}
	}
	if skips != 2 || deletes != 1 {
		t.Errorf("%d skips and %d deletes, want 2 and 1", skips, deletes)
	}
	if acked, _, _, _, _ := cursorOf(t, g, room, m.id.Domain); acked != int64(len(g.events(t, room))) {
		t.Errorf("acked %d of %d", acked, len(g.events(t, room)))
	}
	pull := g.fedDo(m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events?since=0", nil))
	last := g.events(t, room)[len(g.events(t, room))-1]
	if pull.Code != http.StatusOK || strings.Contains(pull.Body.String(), "before") ||
		!strings.Contains(pull.Body.String(), fmt.Sprintf(`{"seq":%d,"type":"skip"}`, last.Seq)) ||
		!strings.Contains(pull.Body.String(), fmt.Sprintf(`"type":"message.delete","payload":{"id":%d}`, id)) {
		t.Errorf("pull: %d %s", pull.Code, pull.Body)
	}
	for _, r := range m.received() {
		if strings.Contains(r, "before") {
			t.Errorf("content reached the peer: %s", r)
		}
	}

	if rec := g.do(g.admin, http.MethodPatch, fmt.Sprintf("/rooms/%s/members/%d", room, c1), `{"rights":["read","claim"]}`); rec.Code != http.StatusOK {
		t.Fatalf("rights: %d %s", rec.Code, rec.Body)
	}
	post(t, g, bot, room, `{"content":"after read","message_type":"answer"}`)
	g.svc.deliverDue(ctx)
	all := strings.Join(m.received(), "\n")
	if !strings.Contains(all, "after read") || strings.Contains(all, "before") {
		t.Errorf("after read granted: %s", all)
	}
}

// Stand-ins are made only from member events and message authors, at
// most 2000 per address domain; beyond that the event is skipped.
func TestReplica_StandInCap(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	if _, err := g.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain)
		SELECT 's' || i || '@x.example', '', '', '2026-01-01T00:00:00Z', 'external', 'x.example' FROM n`); err != nil {
		t.Fatal(err)
	}
	keys := count(t, g.db, `SELECT COUNT(*) FROM api_keys`)
	host := h.id.Domain
	if got := h.push(t, g, room, memberEv(1, "new@x.example", "read"), msg(2, 100, room, host, "new2@x.example", "hi", nil, "", nil),
		memberEv(3, "s1@x.example", "read"), memberEv(4, "y1@y.example", "read")); got != 4 {
		t.Fatalf("applied %d", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys`); n != keys+1 {
		t.Errorf("%d keys made, want 1 (y1)", n-keys)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE m.room_id = ? AND k.label = 's1@x.example'`, room); n != 1 {
		t.Error("an existing stand-in is refused at the cap")
	}
	if len(storedMessages(t, g, room)) != 0 {
		t.Error("a message of an author over the cap was stored")
	}

	// Claim answers make none: an unknown holder is dropped.
	g2, h2, room2, b1 := replica(t, "read", "claim")
	h2.setAnswer(answerWith(http.StatusOK, string(rawJSON(map[string]any{"claims": []map[string]any{
		{"id": 1, "path": "a.go", "address": h2.addr("unknown"), "expires_at": "2026-11-07T12:00:00Z"},
		{"id": 2, "path": "b.go", "address": "z@z.example", "expires_at": "2026-11-07T12:00:00Z"},
	}}))))
	keys = count(t, g2.db, `SELECT COUNT(*) FROM api_keys`)
	rec := g2.do(auth.Claims{KeyID: b1}, http.MethodGet, "/rooms/"+room2+"/claims", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"claims":[]}` {
		t.Errorf("claims: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g2.db, `SELECT COUNT(*) FROM api_keys`); n != keys {
		t.Errorf("a claim answer made %d keys", n-keys)
	}
}

// A host has at most 200 replicas here; an invite into a new one beyond
// that is 409 limit, while its existing ones can still be invited into.
func TestReplica_RoomCap(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.bot(t, "b1")
	var first string
	for i := range 200 {
		id := ulid.Make().String()
		if i == 0 {
			first = id
		}
		if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, home_domain, archived_at)
			VALUES (?, ?, ?, '2026-01-01T00:00:00Z', 'private', ?, '2026-01-01T00:00:00Z')`, id, fmt.Sprint("r", i), g.admin.KeyID, h.id.Domain); err != nil {
			t.Fatal(err)
		}
	}
	// Refused inside the invite's transaction: its audit row waits for
	// no lock and is written.
	start := time.Now()
	wantErr(t, h.invite(t, g, ulid.Make().String(), "new", "b1", "read"), http.StatusConflict, "limit")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the refusal took %v", d)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND result = 'error:limit'`); n != 1 {
		t.Errorf("federation.room.invite error:limit audit rows: %d, want 1", n)
	}
	if rec := h.invite(t, g, first, "again", "b1", "read"); rec.Code != http.StatusCreated {
		t.Errorf("invite into an existing replica: %d %s", rec.Code, rec.Body)
	}
	other := newFakeHost(t, g)
	if rec := other.invite(t, g, ulid.Make().String(), "other", "b1", "read"); rec.Code != http.StatusCreated {
		t.Errorf("another host's invite: %d %s", rec.Code, rec.Body)
	}
}

// A push of 100 bad events writes one audit row.
func TestReplica_RefusedEventsAuditedOnce(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	var evs []fedrooms.Event
	for i := range 100 {
		evs = append(evs, fedrooms.Event{Seq: int64(i + 1), Type: "bogus", Payload: json.RawMessage(`{}`)})
	}
	if got := h.push(t, g, room, evs...); got != 100 {
		t.Fatalf("applied %d", got)
	}
	var detail string
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 1 {
		t.Fatalf("event_refused rows: %d, want 1", n)
	}
	if err := g.db.QueryRow(`SELECT detail FROM audit WHERE action = 'federation.room.event_refused'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Count   int
		Reasons []string
	}
	if json.Unmarshal([]byte(detail), &d) != nil || d.Count != 100 || len(d.Reasons) == 0 || len(d.Reasons) > 5 ||
		!strings.Contains(d.Reasons[0], `unknown event type "bogus"`) {
		t.Errorf("detail: %s", detail)
	}
}

// A message over this server's cap, which a host with a larger cap
// sent, is skipped and counted in the push's one audit row.
func TestReplica_OverOurCap(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	host := h.id.Domain
	big := strings.Repeat("x", 257<<10)
	if got := h.push(t, g, room, memberEv(1, h.addr("h1"), "read"), msg(2, 100, room, host, h.addr("h1"), big, nil, "", nil),
		msg(3, 101, room, host, h.addr("h1"), big, nil, "", nil), msg(4, 102, room, host, h.addr("h1"), "small", nil, "", nil)); got != 4 {
		t.Fatalf("applied %d", got)
	}
	if s := storedMessages(t, g, room); len(s) != 1 || s[0].Content != "small" {
		t.Errorf("stored %d messages", len(s))
	}
	var detail string
	if err := g.db.QueryRow(`SELECT detail FROM audit WHERE action = 'federation.room.event_refused'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 1 ||
		!strings.Contains(detail, `"count":2`) || !strings.Contains(detail, "message cap") {
		t.Errorf("%d rows, detail %s", n, detail)
	}
}

// A slow peer does not hold up the pushes to a fast one.
func TestDelivery_SlowPeerDoesNotDelayOthers(t *testing.T) {
	g := newRoomRig(t)
	slow, fast := newMemberServer(t, g), newMemberServer(t, g)
	r1, r2 := g.room(t, "one"), g.room(t, "two")
	if r2 < r1 {
		r1, r2 = r2, r1
	}
	// The slow peer's room sorts first.
	g.invite(t, r1, slow.addr("s1"), "read")
	g.invite(t, r2, fast.addr("f1"), "read")
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	slow.setReply(func(path string, body []byte) (int, string) {
		<-release
		return applyAll(path, body)
	})
	got := make(chan struct{}, 1)
	fast.setReply(func(path string, body []byte) (int, string) {
		got <- struct{}{}
		return applyAll(path, body)
	})
	done := make(chan struct{})
	go func() { g.svc.deliverDue(context.Background()); close(done) }()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Error("the fast peer waited for the slow one")
	}
	unblock()
	<-done
	for _, c := range []struct {
		room string
		m    *memberServer
	}{{r1, slow}, {r2, fast}} {
		if acked, _, _, _, _ := cursorOf(t, g, c.room, c.m.id.Domain); acked != int64(len(g.events(t, c.room))) {
			t.Errorf("room %s: acked %d", c.room, acked)
		}
	}
}

// A slow host does not hold up the catch-up of another host's rooms.
func TestCatchUp_SlowHostDoesNotDelayOthers(t *testing.T) {
	g := newRoomRig(t)
	g.bot(t, "b1")
	slow, fast := newFakeHost(t, g), newFakeHost(t, g)
	rooms := map[*fakeHost]string{}
	for _, h := range []*fakeHost{slow, fast} {
		rooms[h] = ulid.Make().String()
		if rec := h.invite(t, g, rooms[h], "r"+h.id.Domain, "b1", "read"); rec.Code != http.StatusCreated {
			t.Fatalf("invite: %d %s", rec.Code, rec.Body)
		}
		h.setLog(rooms[h], roomEv(1, "r"+h.id.Domain, false))
	}
	gate := make(chan struct{})
	slow.mu.Lock()
	slow.gate = gate
	slow.mu.Unlock()
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	defer open()
	done := make(chan struct{})
	go func() { g.svc.catchUp(context.Background()); close(done) }()
	for deadline := time.Now().Add(3 * time.Second); appliedSeq(t, g, rooms[fast]) != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Error("the fast host's room waited for the slow one")
			break
		}
	}
	open()
	<-done
	if appliedSeq(t, g, rooms[slow]) != 1 {
		t.Error("the slow host's room was not caught up")
	}
}

// The host refuses to invite a bot of a domain that has 2000 stand-ins
// here, before asking its server.
func TestInviteRemote_StandInCap(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "proj")
	if _, err := g.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain)
		SELECT 's' || i || '@' || ?, '', '', '2026-01-01T00:00:00Z', 'external', ? FROM n`, m.id.Domain, m.id.Domain); err != nil {
		t.Fatal(err)
	}
	wantErr(t, g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+m.addr("c1")+`","rights":["read"]}`),
		http.StatusConflict, "limit")
	if len(m.received()) != 0 {
		t.Error("the invite was sent")
	}
	g.invite(t, room, m.addr("s1"), "read") // an existing stand-in is no new one
}

// Room requests wait 30 s for the host, not the 5 s of document fetches.
func TestRoomRequests_Timeout(t *testing.T) {
	g, h, room, b1 := replica(t, "read", "answer")
	g.svc.fetch.client.Timeout = 50 * time.Millisecond // stands in for the 5 s
	h.setAnswer(func(body []byte) (int, string) {
		time.Sleep(200 * time.Millisecond)
		return http.StatusCreated, string(rawJSON(echo(1, 500, body)))
	})
	if rec := g.do(auth.Claims{KeyID: b1}, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"hi","message_type":"answer"}`); rec.Code != http.StatusCreated {
		t.Errorf("post to a slow host: %d %s", rec.Code, rec.Body)
	}
	if roomTimeout != 30*time.Second || g.svc.fetch.roomClient.Timeout != roomTimeout {
		t.Errorf("room timeout %v", g.svc.fetch.roomClient.Timeout)
	}
}

// A blocked and allowed again host's invite brings its archived replica
// back: name, rights and the bot's membership.
func TestReplicaInvite_Reactivates(t *testing.T) {
	g, h, room, b1 := replica(t, "read")
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+h.id.Domain+"/block", ""), http.StatusOK)
	if _, err := g.db.Exec(`DELETE FROM room_members WHERE room_id = ? AND key_id = ?`, room, b1); err != nil {
		t.Fatal(err)
	}
	allow(t, g.svc, h.id)
	if rec := h.invite(t, g, room, "renamed", "b1", "read", "answer"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	var name, rights string
	if err := g.db.QueryRow(`SELECT r.name, m.rights FROM rooms r JOIN room_members m ON m.room_id = r.id AND m.key_id = ?
		WHERE r.id = ? AND r.archived_at IS NULL`, b1, room).Scan(&name, &rights); err != nil || name != "renamed" || rights != "read,answer" {
		t.Errorf("replica: %q %q %v", name, rights, err)
	}
}

// A peer blocked and allowed again can be invited again, and delivery
// to it resumes with the invite.
func TestInviteRemote_AfterUnblock(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	m.setReply(applyAll)
	ctx := context.Background()
	g.svc.deliverDue(ctx)
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+m.id.Domain+"/block", ""), http.StatusOK)
	allow(t, g.svc, m.id)
	g.invite(t, room, m.addr("c1"), "read")
	g.svc.deliverDue(ctx)
	if acked, _, _, _, paused := cursorOf(t, g, room, m.id.Domain); paused || acked != int64(len(g.events(t, room))) {
		t.Errorf("after the new invite: acked %d of %d, paused %v", acked, len(g.events(t, room)), paused)
	}
}

// Leaving a room whose host is blocked or no peer removes the bot here
// only; the host is not contacted.
func TestReplicaLeave_HostBlocked(t *testing.T) {
	for _, op := range []string{"block", "remove"} {
		t.Run(op, func(t *testing.T) {
			g, h, room, b1 := replica(t, "read")
			h.setAnswer(answerWith(http.StatusNoContent, ""))
			if op == "block" {
				wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+h.id.Domain+"/block", ""), http.StatusOK)
			} else {
				wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, "/v1/federation/peers/"+h.id.Domain, ""), http.StatusNoContent)
			}
			wantStatus(t, g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, b1), ""), http.StatusNoContent)
			if n := len(h.received()); n != 0 {
				t.Errorf("host contacted %d times", n)
			}
			if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b1); n != 0 {
				t.Error("b1 still a member")
			}
			if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.leave' AND result = 'ok'
				AND detail LIKE '%"local_only":true%'`); n != 1 {
				t.Errorf("local-only leave audit rows: %d", n)
			}
		})
	}
}

// A push in flight while its peer is blocked leaves the cursor paused.
func TestDelivery_BlockedDuringPush(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.setReply(func(path string, body []byte) (int, string) {
		if strings.HasSuffix(path, "/events") {
			once.Do(func() { close(inFlight) })
			<-release
		}
		return applyAll(path, body)
	})
	done := make(chan struct{})
	go func() { g.svc.deliverDue(context.Background()); close(done) }()
	<-inFlight
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+m.id.Domain+"/block", ""), http.StatusOK)
	close(release)
	<-done
	if _, _, _, _, paused := cursorOf(t, g, room, m.id.Domain); !paused {
		t.Error("the push's answer resumed delivery to a blocked peer")
	}
}

// A blocked peer's key does not verify author signatures.
func TestReplicaEvents_BlockedAuthorServer(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	c := testIdentity(t, "c.example")
	allow(t, g.svc, c)
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/c.example/block", ""), http.StatusOK)
	if got := h.push(t, g, room, memberEv(1, "c1@c.example", "read"), msg(2, 100, room, h.id.Domain, "c1@c.example", "hi", nil, "", c)); got != 2 {
		t.Fatalf("applied %d", got)
	}
	if s := storedMessages(t, g, room); len(s) != 1 || !s[0].Unverified {
		t.Errorf("stored: %+v", s)
	}
}

// fed_events.message_id is the host message id of message events, and
// the forwarded-post lookup and per-peer delivery updates use indexes.
func TestFedEvents_Indexes(t *testing.T) {
	g, _, room, _ := fedRoom(t, "read")
	bot := g.localBot(t, room, "local-bot", "read,answer")
	id := post(t, g, bot, room, `{"content":"x","message_type":"answer"}`)
	if n := count(t, g.db, `SELECT COUNT(*) FROM fed_events WHERE room_id = ? AND (type = 'message') != (message_id IS NOT NULL)`, room); n != 0 {
		t.Errorf("%d events with a wrong message_id", n)
	}
	if n := count(t, g.db, `SELECT message_id FROM fed_events WHERE room_id = ? AND type = 'message'`, room); int64(n) != id {
		t.Errorf("message_id %d, want %d", n, id)
	}
	for q, want := range map[string]string{
		`SELECT seq FROM fed_events WHERE room_id = '1' AND message_id = 1`:  "fed_events_message",
		`UPDATE fed_delivery SET paused = 0 WHERE peer = 'x' AND paused = 1`: "fed_delivery_peer",
	} {
		rows, err := g.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			_ = rows.Scan(&id, &parent, &unused, &detail)
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if !strings.Contains(strings.Join(plan, "; "), want) {
			t.Errorf("%s: plan %q", q, plan)
		}
	}
}

// Contact resumes paused cursors only: a cursor backing off keeps its
// next try.
func TestResumeDelivery_OnlyPaused(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read")
	m.setReply(func(string, []byte) (int, string) { return http.StatusInternalServerError, "" })
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fixClock(g, &now, 1)
	g.svc.deliverDue(context.Background())
	wantStatus(t, g.fedDo(m.req(t, http.MethodPost, "/fed/v1/ping", nil)), http.StatusOK)
	if _, attempts, next, _, _ := cursorOf(t, g, room, m.id.Domain); attempts != 1 || next != storage.FormatTS(now.Add(time.Second)) {
		t.Errorf("after contact: attempts %d next %s", attempts, next)
	}
}

// A member server with a smaller message cap reads smaller bodies: it
// tells the host its limit in its invite answer and in each pull, and
// the host packs pushes and pull pages within it. An event over it goes
// as a skip, audited once, so the room does not stall.
func TestDelivery_ReceiverLimit(t *testing.T) {
	for _, via := range []string{"push", "pull"} {
		t.Run(via, func(t *testing.T) {
			a, b := newLiveRig(t), newLiveRig(t)
			b.svc.Rooms.MaxMessageBytes = 8 << 10 // b reads bodies up to 80 KiB; a up to 576 KiB
			allow(t, a.svc, b.svc.id)
			allow(t, b.svc, a.svc.id)
			room := a.room(t, "proj")
			b.bot(t, "b1")
			a.invite(t, room, "b1@"+b.svc.Domain(), "read")
			a1 := a.localBot(t, room, "a1-key", "read,answer")
			for i := range 4 {
				post(t, a, a1, room, fmt.Sprintf(`{"content":"m%d %s","message_type":"answer"}`, i, strings.Repeat("x", 30<<10)))
			}
			big := post(t, a, a1, room, `{"content":"`+strings.Repeat("y", 100<<10)+`","message_type":"answer"}`)
			post(t, a, a1, room, `{"content":"after","message_type":"answer"}`)
			evs := a.events(t, room)
			var bigSeq int64
			for _, e := range evs {
				if e.Type == fedrooms.EventMessage && strings.HasPrefix(string(e.Payload), fmt.Sprintf(`{"id":%d,`, big)) {
					bigSeq = e.Seq
				}
			}
			ctx := context.Background()
			if via == "push" {
				for i := 0; i < 10 && a.svc.deliverDue(ctx) == 0; i++ {
				}
			} else if _, err := b.svc.pull(ctx, room, a.svc.Domain(), &refusals{}); err != nil {
				t.Fatalf("pull: %v", err)
			}
			if applied := appliedSeq(t, b, room); applied != int64(len(evs)) {
				t.Fatalf("b applied %d of %d", applied, len(evs))
			}
			if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content NOT LIKE 'yyy%'`, room); n != 5 {
				t.Errorf("b stored %d messages, want 5", n)
			}
			if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content LIKE 'yyy%'`, room); n != 0 {
				t.Error("b stored the message over its limit")
			}
			var detail string
			if err := a.db.QueryRow(`SELECT detail FROM audit WHERE action = 'federation.delivery.oversize' AND target_id = ? AND room_id = ?`,
				b.svc.Domain(), room).Scan(&detail); err != nil {
				t.Fatalf("oversize audit: %v", err)
			}
			if n := count(t, a.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.delivery.oversize'`); n != 1 ||
				!strings.Contains(detail, fmt.Sprintf(`"seqs":[%d]`, bigSeq)) {
				t.Errorf("%d oversize rows, detail %s", n, detail)
			}
		})
	}
}
