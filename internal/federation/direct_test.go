package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
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
	RoomID     string `json:"room_id"`
	Address    string `json:"address"`
	Unread     int    `json:"unread"`
	LastReadAt string `json:"last_read_at"`
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
	if n := count(t, a.db, `SELECT COUNT(*) FROM fed_delivery WHERE room_id = ? AND peer = ? AND max_body = ?`,
		room, b.svc.Domain(), b.svc.eventsLimit()); n != 1 {
		t.Error("a's cursor to b does not keep what b reads a push up to")
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

// dmName is the name of the conversation of the bots at addresses a and
// b: dm:<a>~<b>, sorted.
func dmName(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return "dm:" + a + "~" + b
}

// pairOf is the name of the conversation of g's bot handle with h's bot x.
func (h *fakeHost) pairOf(g *roomRig, handle string) string {
	return dmName(handle+"@"+g.svc.Domain(), h.addr("x"))
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
		if rec := h.directInvite(t, g, ulid.Make().String(), dmName("b1@"+local, h.addr(fmt.Sprintf("x%d", i))), "b1"); rec.Code != http.StatusCreated {
			t.Fatalf("invite %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := h.directInvite(t, g, ulid.Make().String(), dmName("b1@"+local, h.addr("x100")), "b1")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if n := audited(t, g, "direct.refused", "limit"); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	now = now.Add(time.Minute)
	if rec := h.directInvite(t, g, ulid.Make().String(), dmName("b1@"+local, h.addr("x101")), "b1"); rec.Code != http.StatusCreated {
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
	room, name := ulid.Make().String(), h.pairOf(g, "b1")
	if rec := h.directInvite(t, g, room, name, "b1"); rec.Code != http.StatusCreated {
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
	wantErr(t, h.invite(t, g, room, "pair", "b2", "read"), http.StatusConflict, "conflict")
	wantErr(t, h.directInvite(t, g, room, h.pairOf(g, "b2"), "b2"), http.StatusForbidden, "dm_refused")
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, b2.KeyID); n != 0 {
		t.Error("an invite added b2 to the conversation")
	}
	// A room event cannot turn the conversation into a room.
	h.push(t, g, room, roomEv(3, "renamed", false))
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = ? AND kind = 'direct'`, room, name); n != 1 {
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

// A host cannot put two bots of ours into one direct replica: the
// second invite is a conflict, and two bots of ours talk in a room of
// this server even if such a replica exists, so b1's message to b2
// never leaves the server.
func TestDirectRemote_NoForeignRoomForLocalPair(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	b2 := g.dmBot(t, "b2", "peers")
	room := ulid.Make().String()
	if rec := h.directInvite(t, g, room, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite b1: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, h.directInvite(t, g, room, h.pairOf(g, "b2"), "b2"), http.StatusConflict, "conflict")
	// Nor a second bot of its own, by an invite of the same bot.
	wantErr(t, h.directInvite(t, g, room, dmName("b1@"+local, h.addr("y")), "b1"), http.StatusConflict, "conflict")
	// A replica holding both, as an older server could have one.
	if _, err := g.db.Exec(`INSERT OR IGNORE INTO room_members(room_id, key_id, role, joined_at, rights)
		VALUES (?, ?, 'reader', '2026-01-01T00:00:00Z', 'read,question')`, room, b2.KeyID); err != nil {
		t.Fatal(err)
	}
	local := dmRoom(t, g.dm(b1, "b2", "secret"))
	if local == room || count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND home_domain IS NULL`, local) != 1 {
		t.Errorf("b1 wrote to b2 in room %s, want a room of this server", local)
	}
	for _, r := range h.received() {
		if strings.Contains(r, "secret") {
			t.Errorf("the host got the local message: %s", r)
		}
	}
}

// A first message the send path would refuse is refused before any
// room or invite: nothing is left on either server.
func TestDirectRemote_InvalidFirstMessage(t *testing.T) {
	a, b := dmPair(t)
	a1 := a.dmBot(t, "a1", "")
	b.dmBot(t, "b1", "peers")
	to := "b1@" + b.svc.Domain()
	for _, c := range []struct {
		body   string
		status int
	}{
		{`{"to":"` + to + `","content":"hi","message_type":"system"}`, http.StatusBadRequest},
		{`{"to":"` + to + `","content":"hi","message_type":"read"}`, http.StatusBadRequest},
		{`{"to":"` + to + `","content":"","message_type":"question"}`, http.StatusBadRequest},
		{`{"to":"` + to + `","message_type":"question"}`, http.StatusBadRequest},
		{`{"to":"` + to + `","message_type":"question","content":"` + strings.Repeat("x", a.direct.MaxMessageBytes+1) + `"}`,
			http.StatusRequestEntityTooLarge},
	} {
		if rec := a.do(a1, http.MethodPost, "/direct", c.body); rec.Code != c.status {
			t.Errorf("%.80s: %d %s, want %d", c.body, rec.Code, rec.Body, c.status)
		}
	}
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
			t.Errorf("%s has %d rooms", g.svc.Domain(), n)
		}
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite'`); n != 0 {
		t.Errorf("b got %d invites", n)
	}
}

// When the first message fails after the peer took the conversation,
// the host archives the room through its log, so the peer's replica is
// archived too; the next start makes a new conversation.
func TestDirectRemote_AcceptedThenFailed(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "")
	b1 := b.dmBot(t, "b1", "peers")
	send := a.direct.Send
	a.direct.Send = func(w http.ResponseWriter, r *http.Request, roomID string) {
		http.Error(w, `{"error":{"code":"internal_error"}}`, http.StatusInternalServerError)
	}
	if rec := a.dm(a1, "b1@"+b.svc.Domain(), "hi"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed send: %d %s", rec.Code, rec.Body)
	}
	a.direct.Send = send
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND archived_at IS NOT NULL`); n != 1 {
		t.Errorf("a has %d archived conversations, want 1", n)
	}
	a.svc.deliverDue(ctx)
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND archived_at IS NULL`); n != 0 {
		t.Errorf("b has %d active replicas, want 0", n)
	}
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "again"))
	a.svc.deliverDue(ctx)
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE archived_at IS NULL`); n != 1 {
			t.Errorf("%s has %d active rooms, want 1", g.svc.Domain(), n)
		}
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].RoomID != room || got[0].Unread != 1 {
		t.Errorf("b1's list: %+v", got)
	}
}

// A failure of the first message after the peer took the conversation
// does not archive it when another start used it meanwhile.
func TestDirectRemote_FailureKeepsUsedConversation(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "")
	b1 := b.dmBot(t, "b1", "peers")
	send := a.direct.Send
	a.direct.Send = func(w http.ResponseWriter, r *http.Request, roomID string) {
		a.direct.Send = send
		dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "second")) // another start posts
		http.Error(w, `{"error":{"code":"bad_request","message":"no"}}`, http.StatusBadRequest)
	}
	if rec := a.dm(a1, "b1@"+b.svc.Domain(), "hi"); rec.Code != http.StatusBadRequest {
		t.Fatalf("failed send: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND archived_at IS NULL`); n != 1 {
			t.Errorf("%s has %d active conversations, want 1", g.svc.Domain(), n)
		}
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].Unread != 1 {
		t.Errorf("b1's list: %+v", got)
	}
}

// A message that reaches a replica after its bot read the conversation,
// though the host wrote it before that read, is stamped on arrival: it
// counts as unread, and a page since the last read returns it.
func TestDirectRemote_LateMessageUnread(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "")
	b1 := b.dmBot(t, "b1", "peers")
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "first"))
	a.svc.deliverDue(ctx)
	dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "late")) // written on the host, not delivered yet
	if rec := b.do(b1, http.MethodGet, "/rooms/"+room+"/messages", ""); rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].Unread != 0 {
		t.Fatalf("after the read: %+v", got)
	}
	a.svc.deliverDue(ctx)
	got := b.dmList(t, b1)
	if len(got) != 1 || got[0].Unread != 1 || got[0].LastReadAt == "" {
		t.Fatalf("after the late message: %+v", got)
	}
	rec := b.do(b1, http.MethodGet, "/rooms/"+room+"/messages?since="+url.QueryEscape(got[0].LastReadAt), "")
	var page struct{ Messages []struct{ Content string } }
	if json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Messages) != 1 || page.Messages[0].Content != "late" {
		t.Fatalf("page since the last read: %d %s", rec.Code, rec.Body)
	}
	if got := b.dmList(t, b1); got[0].Unread != 0 {
		t.Errorf("after reading the late message: %+v", got)
	}
}

// An invite whose answer is lost leaves the room pending; the next
// start invites into it again, which the peer takes as the same, so
// the pair has one room on each side.
func TestDirectRemote_TimeoutRetry(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "")
	b1 := b.dmBot(t, "b1", "peers")
	invite := a.direct.InviteRemote
	a.direct.InviteRemote = func(ctx context.Context, roomID, name, address string, rights []string) (int, error) {
		if _, err := invite(ctx, roomID, name, address, rights); err != nil {
			return 0, err
		}
		return 0, &fedrooms.Error{Status: http.StatusBadGateway, Code: "peer_unreachable", Message: "peer did not answer"}
	}
	wantErr(t, a.dm(a1, "b1@"+b.svc.Domain(), "hi"), http.StatusBadGateway, "peer_unreachable")
	a.direct.InviteRemote = invite
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "again"))
	a.svc.deliverDue(ctx)
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 1 {
			t.Errorf("%s has %d rooms, want 1", g.svc.Domain(), n)
		}
	}
	if got := b.dmList(t, b1); len(got) != 1 || got[0].RoomID != room || got[0].Unread != 1 {
		t.Errorf("b1's list: %+v", got)
	}
}

// A second direct invite of the origin for the same pair, into another
// room, archives the older replica; a late retry of the older invite
// then changes nothing.
func TestDirectRemote_NewInviteArchivesOlder(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.dmBot(t, "b1", "peers")
	older, newer := ulid.Make().String(), ulid.Make().String()
	for _, room := range []string{older, older, newer} { // the second is a retry, which changes nothing
		if rec := h.directInvite(t, g, room, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
			t.Fatalf("invite %s: %d %s", room, rec.Code, rec.Body)
		}
	}
	wantErr(t, h.directInvite(t, g, older, h.pairOf(g, "b1"), "b1"), http.StatusConflict, "conflict")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, older); n != 1 {
		t.Error("the older replica is still active")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NULL`, newer); n != 1 {
		t.Error("the newer replica is not active")
	}
	// b1 leaving the newer one (membership gone, replica archived) does
	// not let the older come back.
	if _, err := g.db.Exec(`DELETE FROM room_members WHERE room_id = ?`, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`UPDATE rooms SET archived_at = '2026-11-14T00:00:00Z' WHERE id = ?`, newer); err != nil {
		t.Fatal(err)
	}
	wantErr(t, h.directInvite(t, g, older, h.pairOf(g, "b1"), "b1"), http.StatusConflict, "conflict")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, older); n != 1 {
		t.Error("the older replica came back")
	}
}

// When both bots start at once, the room with the lower id is the
// pair's: a server that hosts a lower one of the pair answers an invite
// into a higher one with 409, and takes an invite into a lower one.
func TestDirectRemote_LowerRoomWins(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	lower, mine, higher := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	// g's own start toward the host's bot, its invite not answered yet;
	// another room held the pair's name, so its name is cut and suffixed.
	if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind, pair)
		VALUES (?, ?, ?, '2026-11-14T00:00:00Z', 'private', 'direct', ?)`, mine, "dm:cut#"+mine, b1.KeyID, h.pairOf(g, "b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights)
		VALUES (?, ?, 'reader', '2026-11-14T00:00:00Z', 'read')`, mine, b1.KeyID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, h.directInvite(t, g, higher, h.pairOf(g, "b1"), "b1"), http.StatusConflict, "conflict")
	if rec := h.directInvite(t, g, lower, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("invite into the lower room: %d %s", rec.Code, rec.Body)
	}
}

// Both bots start at once across two servers: one conversation is left.
func TestDirectRemote_SimultaneousStarts(t *testing.T) {
	a, b := dmPair(t)
	ctx := context.Background()
	a1 := a.dmBot(t, "a1", "peers")
	b1 := b.dmBot(t, "b1", "peers")
	invite := a.direct.InviteRemote
	a.direct.InviteRemote = func(ctx context.Context, roomID, name, address string, rights []string) (int, error) {
		a.direct.InviteRemote = invite
		// b1 starts before a's invite arrives: b's room has the higher id.
		wantErr(t, b.dm(b1, "a1@"+a.svc.Domain(), "hello a1"), http.StatusConflict, "conflict")
		return invite(ctx, roomID, name, address, rights)
	}
	room := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "hello b1"))
	a.svc.deliverDue(ctx)
	if again := dmRoom(t, b.dm(b1, "a1@"+a.svc.Domain(), "hello a1")); again != room {
		t.Errorf("b1's next start went to %s, want %s", again, room)
	}
	for _, g := range []*roomRig{a, b} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND archived_at IS NULL`); n != 1 {
			t.Errorf("%s has %d active conversations, want 1", g.svc.Domain(), n)
		}
	}
}

// The dm: prefix is the conversations': a room invite with such a name
// is refused, and a direct invite's name must be the pair's, with the
// other bot on the origin's server.
func TestDirectRemote_ReservedNames(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.dmBot(t, "b1", "peers")
	g.dmBot(t, "b2", "peers")
	wantErr(t, h.invite(t, g, ulid.Make().String(), "dm:b1@"+local+"~b2@"+local, "b1", "read"), http.StatusBadRequest, "bad_request")
	for _, name := range []string{
		"dm:pair",
		dmName("b1@"+local, "b2@"+local),             // a local pair's name
		dmName("b1@"+local, "x@c.example"),           // the other bot is not the origin's
		dmName("b2@"+local, h.addr("x")),             // not the invited bot
		"dm:" + h.addr("x") + "~b1@" + local,         // not sorted
		dmName("b1@"+local, h.addr("x")) + "#1",      // more after the pair
		dmName("b1@"+local, "Bad name@"+h.id.Domain), // not a handle
	} {
		wantErr(t, h.directInvite(t, g, ulid.Make().String(), name, "b1"), http.StatusBadRequest, "bad_request")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms", n)
	}
}

// A room that holds a pair's name does not stop the pair: their
// conversation gets the name with a suffix, and is found again by its
// members. This holds for a local pair and for a remote one.
func TestDirect_SquattedNames(t *testing.T) {
	a, b := dmPair(t)
	a1, a2 := a.dmBot(t, "a1", ""), a.dmBot(t, "a2", "")
	b.dmBot(t, "b1", "peers")
	squat := func(name string) {
		t.Helper()
		if _, err := a.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z')`,
			ulid.Make().String(), name, a.admin.KeyID); err != nil {
			t.Fatal(err)
		}
	}
	squat(dmName("a1@"+a.svc.Domain(), "a2@"+a.svc.Domain()))
	squat(dmName("a1@"+a.svc.Domain(), "b1@"+b.svc.Domain()))
	localRoom := dmRoom(t, a.dm(a1, "a2", "hi"))
	if got := dmRoom(t, a.dm(a2, "a1", "hi back")); got != localRoom {
		t.Errorf("a2 answers in %s, want %s", got, localRoom)
	}
	remoteRoom := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "hi"))
	if got := dmRoom(t, a.dm(a1, "b1@"+b.svc.Domain(), "again")); got != remoteRoom {
		t.Errorf("a1 again in %s, want %s", got, remoteRoom)
	}
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct'`); n != 2 {
		t.Errorf("a has %d conversations, want 2", n)
	}
	// Each keeps its pair, whatever name it got: that is how it is found.
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND pair IN (?, ?)`,
		dmName("a1@"+a.svc.Domain(), "a2@"+a.svc.Domain()), dmName("a1@"+a.svc.Domain(), "b1@"+b.svc.Domain())); n != 2 {
		t.Errorf("%d conversations keep their pair, want 2", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM rooms WHERE kind = 'direct' AND pair = ?`,
		dmName("a1@"+a.svc.Domain(), "b1@"+b.svc.Domain())); n != 1 {
		t.Errorf("b's replica does not keep its pair (%d)", n)
	}
}

// The receiver gives the invited bot the direct rights, whatever the
// host sent, and says it took a direct invite.
func TestDirectRemote_ReceiverRights(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	room := ulid.Make().String()
	rec := h.directInvite(t, g, room, h.pairOf(g, "b1"), "b1")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"kind":"direct"`) {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?
		AND rights = 'read,question,answer,decision,handoff,status_update,task_assignment,alert'`, room, b1.KeyID); n != 1 {
		t.Error("b1 does not have the direct rights")
	}
	if rec := h.invite(t, g, ulid.Make().String(), "proj", "b1", "read"); strings.Contains(rec.Body.String(), `"kind":"direct"`) {
		t.Errorf("a room invite answered %s", rec.Body)
	}
}

// A direct replica has one bot of the host: a member event that adds a
// second is skipped. The conversation is listed once.
func TestDirectRemote_OneRemoteMember(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	room := ulid.Make().String()
	if rec := h.directInvite(t, g, room, h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if applied := h.push(t, g, room, memberEv(1, h.addr("x"), "read", "answer"), memberEv(2, h.addr("y"), "read")); applied != 2 {
		t.Fatalf("applied %d", applied)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.remote_domain IS NOT NULL`, room); n != 1 {
		t.Errorf("the replica has %d remote members, want 1", n)
	}
	// One held by a replica of an older server is listed once too.
	if _, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, remote_domain) VALUES ('z@c.example', 'x', 'p-z', '2026-01-01T00:00:00Z', 'c.example')`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights)
		SELECT ?, id, 'reader', '2026-01-01T00:00:00Z', 'read' FROM api_keys WHERE label = 'z@c.example'`, room); err != nil {
		t.Fatal(err)
	}
	if got := g.dmList(t, b1); len(got) != 1 {
		t.Errorf("b1's list: %+v", got)
	}
}

// A peer that answers a direct invite without "kind":"direct" does not
// know conversations: the sender gets 403 dm_refused. A 409 of the peer
// reaches the sender as 409. The host keeps no room either way.
func TestDirectRemote_PeerAnswers(t *testing.T) {
	a := newLiveRig(t)
	h := newFakeHost(t, a)
	a1 := a.dmBot(t, "a1", "")
	h.setAnswer(answerWith(http.StatusCreated, `{"room_id":"x","max_events_bytes":100000}`))
	wantErr(t, a.dm(a1, h.addr("x"), "hi"), http.StatusForbidden, "dm_refused")
	h.setAnswer(answerWith(http.StatusConflict, `{"error":{"code":"conflict","message":"a room with this id exists here"}}`))
	wantErr(t, a.dm(a1, h.addr("x"), "hi"), http.StatusConflict, "conflict")
	if n := count(t, a.db, `SELECT COUNT(*) FROM rooms WHERE archived_at IS NULL`); n != 0 {
		t.Errorf("a keeps %d active rooms", n)
	}
}
