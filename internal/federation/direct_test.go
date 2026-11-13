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
)

// Direct conversations across servers (docs/superpowers/specs/2026-11-13-direct-messages-design.md).

// dmPair is servers a and b, each an allowed peer of the other.
func dmPair(t *testing.T) (a, b *roomRig) {
	t.Helper()
	a, b = newLiveRig(t), newLiveRig(t)
	allow(t, a.svc, b.svc.id)
	allow(t, b.svc, a.svc.id)
	return a, b
}

// dmBot adds a bot whose DM policy is policy; "" leaves it without a
// profile row.
func (g *roomRig) dmBot(t *testing.T, handle, policy string) auth.Claims {
	t.Helper()
	id := g.bot(t, handle)
	if policy != "" {
		if _, err := g.db.Exec(`INSERT INTO bot_profiles(key_id, updated_at, dm_policy) VALUES (?, '2026-01-01T00:00:00Z', ?)`,
			id, policy); err != nil {
			t.Fatal(err)
		}
	}
	return auth.Claims{KeyID: id}
}

// dm starts or continues c's conversation with to.
func (g *roomRig) dm(c auth.Claims, to, content string) *httptest.ResponseRecorder {
	return g.do(c, http.MethodPost, "/direct", fmt.Sprintf(`{"to":%q,"content":%q,"message_type":"question"}`, to, content))
}

// dmRoom returns the room of a 201 answer to POST /direct.
func dmRoom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		RoomID  string `json:"room_id"`
		Message struct {
			ID int64 `json:"id"`
		} `json:"message"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.RoomID == "" || out.Message.ID == 0 {
		t.Fatalf("direct: %d %s", rec.Code, rec.Body)
	}
	return out.RoomID
}

type conversation struct {
	RoomID  string `json:"room_id"`
	Address string `json:"address"`
	Unread  int    `json:"unread"`
}

func (g *roomRig) dmList(t *testing.T, c auth.Claims) []conversation {
	t.Helper()
	var out struct{ Conversations []conversation }
	rec := g.do(c, http.MethodGet, "/direct", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	return out.Conversations
}

// audited counts g's audit rows of action whose detail has the reason.
func audited(t *testing.T, g *roomRig, action, reason string) int {
	t.Helper()
	return count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = ? AND detail LIKE ?`, action, `%"reason":"`+reason+`"%`)
}

// The recipient's server takes a conversation only for a bot whose
// policy is peers; a refusal reaches the sender with its status and
// code, and the host keeps nothing of the room it made.
func TestDirectRemote_Policy(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.dmBot(t, "a1", "")
	b.dmBot(t, "none", "none")
	b.dmBot(t, "local", "local")
	b.dmBot(t, "norow", "")
	b.dmBot(t, "b1", "peers")
	b.bot(t, "boss")
	if _, err := b.db.Exec(`UPDATE api_keys SET is_admin = 1 WHERE handle = 'boss'`); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"none", "local", "norow"} {
		wantErr(t, a.dm(a1, h+"@"+b.svc.Domain(), "hi"), http.StatusForbidden, "dm_refused")
	}
	wantErr(t, a.dm(a1, "nobody@"+b.svc.Domain(), "hi"), http.StatusNotFound, "not_found")
	wantErr(t, a.dm(a1, "boss@"+b.svc.Domain(), "hi"), http.StatusNotFound, "not_found")
	for _, table := range []string{"rooms", "room_members", "messages", "fed_events", "fed_delivery"} {
		if n := count(t, a.db, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("a keeps %d rows in %s after the refusals", n, table)
		}
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM api_keys WHERE remote_domain IS NOT NULL`); n != 0 {
		t.Errorf("a made %d stand-ins for refused conversations", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("b made %d replicas for refused invites", n)
	}
	for _, g := range []*roomRig{a, b} {
		if n := audited(t, g, "direct.refused", "policy"); n != 3 {
			t.Errorf("%s audited %d policy refusals, want 3", g.svc.Domain(), n)
		}
		if n := audited(t, g, "direct.refused", "not a bot"); n != 2 {
			t.Errorf("%s audited %d not-a-bot refusals, want 2", g.svc.Domain(), n)
		}
	}

	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "hello"))
	var kind, name string
	if err := a.db.QueryRow(`SELECT kind, name FROM rooms WHERE id = ? AND home_domain IS NULL`, room).Scan(&kind, &name); err != nil ||
		kind != "direct" || name != "dm:a1@"+a.svc.Domain()+"~b1@"+b.svc.Domain() {
		t.Fatalf("room on a: %q %q %v", kind, name, err)
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE m.room_id = ?
		AND (m.key_id = ? OR k.label = ?)`, room, a1.KeyID, "b1@"+b.svc.Domain()); n != 2 {
		t.Errorf("a has %d of the two members", n)
	}
	if evs := a.events(t, room); len(evs) == 0 || !strings.Contains(string(evs[0].Payload), `"kind":"direct"`) {
		t.Errorf("first event: %+v", evs)
	}
	if err := b.db.QueryRow(`SELECT kind FROM rooms WHERE id = ? AND home_domain = ?`, room, a.svc.Domain()).Scan(&kind); err != nil || kind != "direct" {
		t.Errorf("replica on b: %q %v", kind, err)
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM audit WHERE action = 'direct.start' AND room_id = ?`, room); n != 1 {
		t.Errorf("a audited %d starts, want 1", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'direct.start' AND room_id = ?`, room); n != 1 {
		t.Errorf("b audited %d starts, want 1", n)
	}
}

// Only bots of allowed peers start conversations: a recipient's server
// that has seen the sender's server, without allowing it, refuses; the
// sender's server asks only allowed peers.
func TestDirectRemote_SeenSender(t *testing.T) {
	a, b := newLiveRig(t), newLiveRig(t)
	allow(t, a.svc, b.svc.id)
	b.svc.mode = modeOpen
	if _, err := b.svc.peers.RecordSeen(context.Background(), a.svc.Domain(), a.svc.id.Public); err != nil {
		t.Fatal(err)
	}
	a1 := a.dmBot(t, "a1", "")
	b.dmBot(t, "b1", "peers")
	wantErr(t, a.dm(a1, "b1@"+b.svc.Domain(), "hi"), http.StatusForbidden, "dm_refused")
	wantErr(t, a.dm(a1, "b1@c.example", "hi"), http.StatusForbidden, "peer_not_allowed")
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms`) + count(t, b.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms left", n)
	}
}

// directInvite is a direct invite of handle into a new room, from h.
func (h *fakeHost) directInvite(t *testing.T, g *roomRig, room, name, handle string) *httptest.ResponseRecorder {
	t.Helper()
	return g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/invite",
		map[string]any{"name": name, "handle": handle, "rights": []string{"read", "question"}, "kind": "direct"}))
}

// A server takes at most 100 direct invites per peer an hour.
func TestDirectRemote_IncomingLimit(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.dmBot(t, "b1", "peers")
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.svc.now = func() time.Time { return now }
	g.svc.limiters[h.id.Domain] = rate.NewLimiter(rate.Inf, 0) // the per-peer request limit is not under test
	for i := range 100 {
		if rec := h.directInvite(t, g, ulid.Make().String(), fmt.Sprintf("dm:%d", i), "b1"); rec.Code != http.StatusCreated {
			t.Fatalf("invite %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := h.directInvite(t, g, ulid.Make().String(), "dm:100", "b1")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if n := audited(t, g, "direct.refused", "limit"); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	now = now.Add(time.Minute)
	if rec := h.directInvite(t, g, ulid.Make().String(), "dm:101", "b1"); rec.Code != http.StatusCreated {
		t.Errorf("a minute later: %d %s", rec.Code, rec.Body)
	}
}

// A direct replica takes our bots by direct invite only, which checks
// the policy: not by member events, nor by invites of another kind.
func TestDirectRemote_ReplicaGuards(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	b2 := g.dmBot(t, "b2", "none")
	room := ulid.Make().String()
	if rec := h.directInvite(t, g, room, "dm:pair", "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if applied := h.push(t, g, room, memberEv(1, "b1@"+local, "read"), memberEv(2, "b2@"+local, "read")); applied != 2 {
		t.Fatalf("applied %d", applied)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b2.KeyID); n != 0 {
		t.Error("a member event added b2 to the conversation")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read'`, room, b1.KeyID); n != 1 {
		t.Error("b1's member event was not applied")
	}
	wantErr(t, h.invite(t, g, room, "dm:pair", "b2", "read"), http.StatusConflict, "conflict")
	wantErr(t, h.directInvite(t, g, room, "dm:pair", "b2"), http.StatusForbidden, "dm_refused")
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b2.KeyID); n != 0 {
		t.Error("an invite added b2 to the conversation")
	}
	// A room event cannot turn the conversation into a room.
	h.push(t, g, room, roomEv(3, "renamed", false))
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = 'dm:pair' AND kind = 'direct'`, room); n != 1 {
		t.Error("a room event of another kind was applied")
	}
	wantErr(t, g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, b2.KeyID)),
		http.StatusForbidden, "replica_room")
}

// When a's bot started the conversation, b's bot starting toward it
// finds b's replica, even before a delivered its events, and posts
// through the host: there is one room on each side.
func TestDirectRemote_StartFromOtherSide(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.dmBot(t, "a1", "none") // an existing conversation needs no policy
	b1 := b.dmBot(t, "b1", "peers")
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "hi"))
	if got := dmRoom(t, b.dm(b1, "a1@"+a.svc.Domain(), "hello back")); got != room {
		t.Errorf("b1 starts: room %s, want %s", got, room)
	}
	if got := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "again")); got != room {
		t.Errorf("a1 again: room %s, want %s", got, room)
	}
	a.svc.deliverDue(context.Background())
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 1 {
			t.Errorf("%s has %d rooms", g.svc.Domain(), n)
		}
		if n := count(t, g.db, `SELECT COUNT(*) FROM messages WHERE room_id = ?`, room); n != 3 {
			t.Errorf("%s has %d messages, want 3", g.svc.Domain(), n)
		}
	}
	var label string
	if err := a.db.QueryRow(`SELECT k.label FROM messages m JOIN api_keys k ON k.id = m.author_key_id
		WHERE m.room_id = ? AND m.content = 'hello back'`, room).Scan(&label); err != nil || label != "b1@"+b.svc.Domain() {
		t.Errorf("author on a: %q %v", label, err)
	}
	if applied, total := appliedSeq(t, b, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("b applied %d of %d", applied, total)
	}
}

// a's bot writes, b's bot answers in the room, and both list the
// conversation with the other's address and what they did not read.
func TestDirectRemote_Conversation(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "")
	b1 := b.dmBot(t, "b1", "peers")
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "question"))
	a.svc.deliverDue(ctx)
	post(t, b, b1, room, `{"content":"answer","message_type":"answer"}`)
	a.svc.deliverDue(ctx)
	if got := a.dmList(t, a1); len(got) != 1 || got[0].RoomID != room || got[0].Address != "b1@"+b.svc.Domain() || got[0].Unread != 1 {
		t.Errorf("a1's list: %+v", got)
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].RoomID != room || got[0].Address != "a1@"+a.svc.Domain() || got[0].Unread != 1 {
		t.Errorf("b1's list: %+v", got)
	}
	if rec := b.do(b1, http.MethodGet, "/rooms/"+room+"/messages", ""); rec.Code != http.StatusOK {
		t.Fatalf("read on b: %d %s", rec.Code, rec.Body)
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].Unread != 0 {
		t.Errorf("b1's list after reading: %+v", got)
	}
	if applied, total := appliedSeq(t, b, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("b applied %d of %d", applied, total)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
		t.Errorf("b refused %d batches", n)
	}
	// The host takes no other member, by address either.
	b.bot(t, "c1")
	wantErr(t, a.do(a.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"c1@`+b.svc.Domain()+`","rights":["read"]}`),
		http.StatusForbidden, "direct_room")
}

// A sender's 30 new conversations an hour count remote ones too.
func TestDirectRemote_SenderLimit(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.dmBot(t, "a1", "")
	b.dmBot(t, "b1", "peers")
	b.dmBot(t, "b2", "peers")
	dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "hi"))
	for i := range 29 {
		a.dmBot(t, fmt.Sprintf("l%d", i), "")
		dmRoom(t, a.dm(a1, fmt.Sprintf("l%d", i), "hi"))
	}
	rec := a.dm(a1, "b2@"+b.svc.Domain(), "hi")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms`); n != 1 {
		t.Errorf("b has %d replicas, want 1", n)
	}
}
