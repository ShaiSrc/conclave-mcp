package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// fakeHost hosts rooms that the rig replicates: an allowed peer of the
// rig on 127.0.0.1 that answers pulls from its log. Its pushes are
// signed requests served by the rig.
type fakeHost struct {
	id    *Identity
	mu    sync.Mutex
	log   map[string][]fedrooms.Event // per room
	pulls []string                    // "room since" of each pull
}

func newFakeHost(t *testing.T, g *roomRig) *fakeHost {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{id: testIdentity(t, ln.Addr().String()), log: map[string][]fedrooms.Event{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, _ := strings.CutPrefix(r.URL.Path, "/fed/v1/rooms/")
		room, ok := strings.CutSuffix(rest, "/events")
		since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if r.Method != http.MethodGet || !ok || err != nil {
			http.NotFound(w, r)
			return
		}
		h.mu.Lock()
		h.pulls = append(h.pulls, room+" "+strconv.FormatInt(since, 10))
		out := []fedrooms.Event{}
		for _, e := range h.log[room] {
			if e.Seq > since && len(out) < maxRoomEvents {
				out = append(out, e)
			}
		}
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"events": out})
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	allow(t, g.svc, h.id)
	return h
}

func (h *fakeHost) addr(name string) string { return name + "@" + h.id.Domain }

func (h *fakeHost) setLog(room string, evs ...fedrooms.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log[room] = evs
}

func (h *fakeHost) pulled() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.pulls...)
}

func (h *fakeHost) invite(t *testing.T, g *roomRig, room, name, handle string, rights ...string) *httptest.ResponseRecorder {
	t.Helper()
	if rights == nil {
		rights = []string{}
	}
	return g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/invite",
		map[string]any{"name": name, "handle": handle, "rights": rights}))
}

// push sends evs to the rig as a push of room and returns the seq it
// answers it applied, or -1 on an error status.
func (h *fakeHost) push(t *testing.T, g *roomRig, room string, evs ...fedrooms.Event) int64 {
	t.Helper()
	rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/events", map[string]any{"events": evs}))
	if rec.Code != http.StatusOK {
		return -1
	}
	var out struct{ Applied int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("push answer: %s", rec.Body)
	}
	return out.Applied
}

func event(seq int64, typ string, payload any) fedrooms.Event {
	b, _ := json.Marshal(payload)
	return fedrooms.Event{Seq: seq, Type: typ, Payload: b}
}

func roomEv(seq int64, name string, archived bool) fedrooms.Event {
	return event(seq, fedrooms.EventRoom, fedrooms.RoomPayload{Name: name, Archived: archived})
}

func memberEv(seq int64, address string, rights ...string) fedrooms.Event {
	if rights == nil {
		rights = []string{}
	}
	return event(seq, fedrooms.EventMember, fedrooms.MemberPayload{Address: address, Rights: rights})
}

func removeEv(seq int64, address string) fedrooms.Event {
	return event(seq, fedrooms.EventMemberRemove, fedrooms.RemovePayload{Address: address})
}

// msg is a message event of room hosted by host, from author, with the
// host id id; signer, when set, signs it as the author's server.
func msg(seq, id int64, room, host, author, content string, replyTo *int64, metadata string, signer *Identity) fedrooms.Event {
	p := fedrooms.MessagePayload{ID: id, Author: author, TS: "2026-11-13T00:00:00.000000000Z", ContentType: "text/plain",
		MessageType: "answer", Content: content, ReplyTo: replyTo}
	if metadata != "" {
		p.Metadata = json.RawMessage(metadata)
	}
	if signer != nil {
		p.OriginSig = signer.SignMessage(MessageSig{Room: room + "@" + host, Author: author, ContentType: p.ContentType,
			MessageType: p.MessageType, Content: content, Metadata: p.Metadata, ReplyTo: replyTo})
	}
	return event(seq, fedrooms.EventMessage, p)
}

// replica is a rig with bot b1 invited by a fake host into room proj.
func replica(t *testing.T, rights ...string) (*roomRig, *fakeHost, string, int64) {
	t.Helper()
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.bot(t, "b1")
	room := ulid.Make().String()
	if rec := h.invite(t, g, room, "proj", "b1", rights...); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	return g, h, room, b1
}

// stored is one message of a replica as the rig stored it.
type stored struct {
	ID, RemoteID     int64
	Author, Trust    string
	Content, Meta    string
	Unverified       bool
	OriginSig, Label string
}

func storedMessages(t *testing.T, g *roomRig, room string) []stored {
	t.Helper()
	rows, err := g.db.Query(`SELECT m.id, COALESCE(m.remote_id, 0), m.author_key_id, m.trust_origin, m.content, COALESCE(m.metadata, ''),
		m.unverified, COALESCE(m.origin_sig, ''), k.label FROM messages m JOIN api_keys k ON k.id = m.author_key_id
		WHERE m.room_id = ? ORDER BY m.id`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []stored
	for rows.Next() {
		var s stored
		var author int64
		if err := rows.Scan(&s.ID, &s.RemoteID, &author, &s.Trust, &s.Content, &s.Meta, &s.Unverified, &s.OriginSig, &s.Label); err != nil {
			t.Fatal(err)
		}
		s.Author = strconv.FormatInt(author, 10)
		out = append(out, s)
	}
	return out
}

func appliedSeq(t *testing.T, g *roomRig, room string) int64 {
	t.Helper()
	return int64(count(t, g.db, `SELECT applied_seq FROM rooms WHERE id = ?`, room))
}

func TestReplicaInvite(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.bot(t, "b1")
	revoked := g.bot(t, "gone")
	if _, err := g.db.Exec(`UPDATE api_keys SET revoked_at = '2026-01-02T00:00:00Z' WHERE id = ?`, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin, handle) VALUES ('adm', 'x', 'p-adm', '2026-01-01T00:00:00Z', 1, 'adm')`); err != nil {
		t.Fatal(err)
	}
	mine := g.room(t, "mine")
	other := newFakeHost(t, g)
	theirs := ulid.Make().String()
	if rec := other.invite(t, g, theirs, "theirs", "b1", "read"); rec.Code != http.StatusCreated {
		t.Fatalf("other host's invite: %d %s", rec.Code, rec.Body)
	}
	room := ulid.Make().String()
	rooms := func() int { return count(t, g.db, `SELECT COUNT(*) FROM rooms`) }
	before, nRooms := g.state(t), rooms()
	for name, c := range map[string]struct {
		room, name, handle string
		rights             []string
		status             int
		code               string
	}{
		"unknown bot":            {room, "proj", "nobody", []string{"read"}, 404, "not_found"},
		"revoked bot":            {room, "proj", "gone", []string{"read"}, 404, "not_found"},
		"admin key":              {room, "proj", "adm", []string{"read"}, 404, "not_found"},
		"bad handle":             {room, "proj", "B 1", []string{"read"}, 404, "not_found"},
		"bad rights":             {room, "proj", "b1", []string{"fly"}, 400, "bad_request"},
		"no name":                {room, "", "b1", []string{"read"}, 400, "bad_request"},
		"long name":              {room, strings.Repeat("n", 129), "b1", []string{"read"}, 400, "bad_request"},
		"room id not a ULID":     {"not-a-room", "proj", "b1", []string{"read"}, 400, "bad_request"},
		"a local room":           {mine, "proj", "b1", []string{"read"}, 409, "conflict"},
		"another host's replica": {theirs, "proj", "b1", []string{"read"}, 409, "conflict"},
		"room id not canonical":  {strings.ToLower(room), "proj", "b1", []string{"read"}, 400, "bad_request"},
	} {
		wantErr(t, h.invite(t, g, c.room, c.name, c.handle, c.rights...), c.status, c.code)
		if g.state(t) != before || rooms() != nRooms {
			t.Errorf("%s: stored something", name)
		}
	}

	if rec := h.invite(t, g, room, "proj", "b1", "read", "answer"); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	var (
		name, home, owner string
		archived          *string
	)
	if err := g.db.QueryRow(`SELECT r.name, r.home_domain, r.archived_at, k.label FROM rooms r JOIN api_keys k ON k.id = r.owner_key_id WHERE r.id = ?`, room).
		Scan(&name, &home, &archived, &owner); err != nil {
		t.Fatal(err)
	}
	if name != "proj" || home != h.id.Domain || archived != nil || owner != h.addr("operator") {
		t.Errorf("replica: name %q home %q archived %v owner %q", name, home, archived, owner)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read,answer'`, room, b1); n != 1 {
		t.Error("b1 is not a member with its rights")
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND target_id = ? AND room_id = ? AND result = 'ok'`,
		h.id.Domain, room); n != 1 {
		t.Errorf("federation.room.invite audit rows: %d", n)
	}
	// The bot reads the replica like any room; writes to it stay refused.
	if rec := g.do(auth.Claims{KeyID: b1}, http.MethodGet, "/rooms/"+room, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"home":"`+h.id.Domain+`"`) {
		t.Errorf("get replica: %d %s", rec.Code, rec.Body)
	}
	// Invited again: rights updated, nothing duplicated.
	if rec := h.invite(t, g, room, "proj", "b1", "read"); rec.Code != http.StatusCreated {
		t.Fatalf("invite again: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ? AND rights = 'read'`, room, b1); n != 1 {
		t.Error("rights not updated")
	}

	// A name a local room holds: the replica is name@host.
	g.room(t, "clash")
	clash := ulid.Make().String()
	if rec := h.invite(t, g, clash, "clash", "b1", "read"); rec.Code != http.StatusCreated {
		t.Fatalf("invite clash: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = ?`, clash, "clash@"+h.id.Domain); n != 1 {
		t.Error("clashing replica is not named name@host")
	}
}

func TestReplicaInvite_OnlyAllowedPeers(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.bot(t, "b1")
	g.svc.mode = modeOpen
	if _, err := g.db.Exec(`UPDATE federation_peers SET status = 'seen' WHERE domain = ?`, h.id.Domain); err != nil {
		t.Fatal(err)
	}
	room := ulid.Make().String()
	wantErr(t, h.invite(t, g, room, "proj", "b1", "read"), 403, "peer_not_allowed")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Error("a seen peer created a replica")
	}
}

func TestReplicaEvents_OrderAndIdempotence(t *testing.T) {
	g, h, room, b1 := replica(t, "read", "answer")
	sub, cancel := g.broker.Subscribe(room)
	defer cancel()
	evs := []fedrooms.Event{
		roomEv(1, "proj", false),
		memberEv(2, h.addr("h1"), "read", "answer"),
		memberEv(3, "b1@"+local, "read", "answer"),
		msg(4, 100, room, h.id.Domain, h.addr("h1"), "one", nil, "", nil),
		msg(5, 101, room, h.id.Domain, h.addr("h1"), "two", nil, "", nil),
	}
	if got := h.push(t, g, room, evs[2], evs[0], evs[1]); got != 3 {
		t.Errorf("unordered push: applied %d, want 3", got)
	}
	if got := h.push(t, g, room, evs...); got != 5 {
		t.Errorf("push: applied %d, want 5", got)
	}
	if got := h.push(t, g, room, evs[3], evs[4]); got != 5 {
		t.Errorf("again: applied %d, want 5", got)
	}
	ms := storedMessages(t, g, room)
	if len(ms) != 2 || ms[0].Content != "one" || ms[1].Content != "two" || ms[0].RemoteID != 100 || ms[1].RemoteID != 101 {
		t.Fatalf("messages: %+v", ms)
	}
	if ms[0].Label != h.addr("h1") || ms[0].Trust != "external" || ms[0].Unverified {
		t.Errorf("author: %+v", ms[0])
	}
	if appliedSeq(t, g, room) != 5 {
		t.Errorf("applied_seq %d", appliedSeq(t, g, room))
	}
	// Each message reached the live stream once, as a local message does.
	for _, want := range []string{"one", "two"} {
		select {
		case e := <-sub:
			if !strings.Contains(string(e.Payload), `"content":"`+want+`"`) || !strings.Contains(string(e.Payload), `"trust_origin":"external"`) {
				t.Errorf("live event: %s", e.Payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("no live event for %s", want)
		}
	}
	select {
	case e := <-sub:
		t.Errorf("extra live event: %s", e.Payload)
	default:
	}
	// The bot reads them through the API.
	rec := g.do(auth.Claims{KeyID: b1}, http.MethodGet, "/rooms/"+room+"/messages", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"two"`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	// A replica writes no event log of its own.
	if n := count(t, g.db, `SELECT COUNT(*) FROM fed_events`); n != 0 {
		t.Errorf("replica logged %d events", n)
	}
}

func TestReplicaEvents_GapThenPull(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	var log []fedrooms.Event
	log = append(log, roomEv(1, "proj", false))
	for seq := int64(2); seq <= 150; seq++ {
		log = append(log, msg(seq, 1000+seq, room, h.id.Domain, h.addr("h1"), "m"+strconv.FormatInt(seq, 10), nil, "", nil))
	}
	h.setLog(room, log...)
	if got := h.push(t, g, room, log[0], log[1]); got != 2 {
		t.Fatalf("first push: applied %d", got)
	}
	// 3 to 148 never came: the replica applies nothing past the gap and
	// pulls since 2, page by page.
	if got := h.push(t, g, room, log[148], log[149]); got != 150 {
		t.Errorf("push after a gap: applied %d, want 150", got)
	}
	want := []string{room + " 2", room + " 102", room + " 150"}
	if got := h.pulled(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pulls %v, want %v", got, want)
	}
	if ms := storedMessages(t, g, room); len(ms) != 149 || ms[0].Content != "m2" || ms[148].Content != "m150" {
		t.Errorf("%d messages", len(ms))
	}
}

func TestReplicaEvents_OnlyFromHost(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	other := newFakeHost(t, g)
	mine := g.room(t, "mine")
	before := g.state(t)
	for name, c := range map[string]struct {
		from   *fakeHost
		room   string
		status int
		code   string
	}{
		"another peer":     {other, room, 404, "not_found"},
		"unknown room":     {h, ulid.Make().String(), 404, "not_found"},
		"room hosted here": {h, mine, 404, "not_found"},
	} {
		rec := g.fedDo(signedReq(t, c.from.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+c.room+"/events",
			map[string]any{"events": []fedrooms.Event{memberEv(1, c.from.addr("x"), "read")}}))
		wantErr(t, rec, c.status, c.code)
		if g.state(t) != before || appliedSeq(t, g, room) != 0 {
			t.Errorf("%s: applied something", name)
		}
	}
	// A host that is no longer an allowed peer (open mode lets seen
	// peers through the signature check) is refused too.
	g.svc.mode = modeOpen
	if _, err := g.db.Exec(`UPDATE federation_peers SET status = 'seen' WHERE domain = ?`, h.id.Domain); err != nil {
		t.Fatal(err)
	}
	wantErr(t, g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/events",
		map[string]any{"events": []fedrooms.Event{roomEv(1, "x", false)}})), 403, "peer_not_allowed")
	if appliedSeq(t, g, room) != 0 {
		t.Error("a seen peer's events applied")
	}
	// More than 100 events in one push.
	g.svc.mode = "allowlist"
	if _, err := g.db.Exec(`UPDATE federation_peers SET status = 'allowed' WHERE domain = ?`, h.id.Domain); err != nil {
		t.Fatal(err)
	}
	var many []fedrooms.Event
	for seq := int64(1); seq <= 101; seq++ {
		many = append(many, roomEv(seq, "x", false))
	}
	wantErr(t, g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/events",
		map[string]any{"events": many})), 400, "bad_request")
}

func TestReplicaEvents_AuthorSignatures(t *testing.T) {
	g, h, room, b1 := replica(t, "read", "answer")
	c := testIdentity(t, "c.example")
	allow(t, g.svc, c)
	d := testIdentity(t, "d.example") // unknown here
	opKey := g.bot(t, "opbot")
	if _, err := g.db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, fedrooms.OperatorMetaKey, strconv.FormatInt(opKey, 10)); err != nil {
		t.Fatal(err)
	}
	plain := g.bot(t, "plainbot")
	host := h.id.Domain
	cases := []struct {
		name, author string
		signer       *Identity
		stored       bool
		unverified   bool
		label        string
	}{
		{"host's bot, no signature", h.addr("h1"), nil, true, false, h.addr("h1")},
		{"third server, verified", "c1@c.example", c, true, false, "c1@c.example"},
		{"third server, key unknown", "d1@d.example", d, true, true, "d1@d.example"},
		{"third server, wrong key", "c1@c.example", d, false, false, ""},
		{"third server, no signature", "c1@c.example", nil, false, false, ""},
		{"our bot, our signature", "b1@" + local, g.svc.id, true, false, "b1"},
		{"our bot, forged", "b1@" + local, c, false, false, ""},
		{"our bot, unsigned", "b1@" + local, nil, false, false, ""},
		{"our operator", "operator@" + local, g.svc.id, true, false, "opbot"},
		{"our key without handle", fmt.Sprintf("key-%d@%s", plain, local), g.svc.id, true, false, "plainbot"},
		{"no such bot of ours", "zz@" + local, g.svc.id, false, false, ""},
		{"invalid address", "Not An Address", nil, false, false, ""},
	}
	for i, tc := range cases {
		seq, id := int64(i+1), int64(100+i)
		if got := h.push(t, g, room, msg(seq, id, room, host, tc.author, tc.name, nil, "", tc.signer)); got != seq {
			t.Fatalf("%s: applied %d, want %d (a refused event still counts)", tc.name, got, seq)
		}
		var found *stored
		for _, s := range storedMessages(t, g, room) {
			if s.RemoteID == id {
				found = &s
			}
		}
		switch {
		case !tc.stored && found != nil:
			t.Errorf("%s: stored", tc.name)
		case !tc.stored:
			if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused' AND target_id = ? AND room_id = ?
				AND detail LIKE ?`, host, room, fmt.Sprintf(`%%"seq":%d%%`, seq)); n != 1 {
				t.Errorf("%s: event_refused audit rows %d", tc.name, n)
			}
		case found == nil:
			t.Errorf("%s: not stored", tc.name)
		case found.Unverified != tc.unverified || found.Label != tc.label:
			t.Errorf("%s: unverified %v label %s", tc.name, found.Unverified, found.Label)
		}
	}
	// Our own bot's message is ours, internal, outside the markers.
	for _, s := range storedMessages(t, g, room) {
		if s.Label == "b1" && (s.Author != strconv.FormatInt(b1, 10) || s.Trust != "internal") {
			t.Errorf("our bot's message: %+v", s)
		}
	}
}

func TestReplicaEvents_ReplyTo(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	c := testIdentity(t, "c.example")
	allow(t, g.svc, c)
	host := h.id.Domain
	first, missing := int64(100), int64(999)
	if got := h.push(t, g, room,
		msg(1, 100, room, host, h.addr("h1"), "first", nil, "", nil),
		// The host's reply_to and wire metadata are what c signed.
		msg(2, 101, room, host, "c1@c.example", "reply", &first, `{"x":1}`, c),
		msg(3, 102, room, host, h.addr("h1"), "to the unknown", &missing, `{"y":2}`, nil),
		msg(4, 103, room, host, h.addr("h1"), "plain reply", &first, "", nil),
	); got != 4 {
		t.Fatalf("applied %d", got)
	}
	ms := storedMessages(t, g, room)
	if len(ms) != 4 {
		t.Fatalf("%d messages", len(ms))
	}
	local1 := ms[0].ID
	for i, want := range []string{"", fmt.Sprintf(`{"reply_to":%d,"x":1}`, local1), `{"y":2}`, fmt.Sprintf(`{"reply_to":%d}`, local1)} {
		if ms[i].Meta != want {
			t.Errorf("message %d metadata %s, want %s", i, ms[i].Meta, want)
		}
	}
	if ms[1].Unverified {
		t.Error("signature over the host's reply_to did not verify")
	}
}

func TestReplicaEvents_MembersAndRoom(t *testing.T) {
	g, h, room, _ := replica(t, "read", "answer")
	host := h.id.Domain
	rightsOf := func(address string) string {
		var r string
		if err := g.db.QueryRow(`SELECT m.rights FROM room_members m JOIN api_keys k ON k.id = m.key_id
			WHERE m.room_id = ? AND (k.label = ? OR k.handle = ?)`, room, address, strings.TrimSuffix(address, "@"+local)).Scan(&r); err != nil {
			return "-"
		}
		return r
	}
	roomState := func() (name string, archived bool) {
		if err := g.db.QueryRow(`SELECT name, archived_at IS NOT NULL FROM rooms WHERE id = ?`, room).Scan(&name, &archived); err != nil {
			t.Fatal(err)
		}
		return
	}
	g.room(t, "taken")
	steps := []struct {
		ev    fedrooms.Event
		check func() string
	}{
		{memberEv(1, h.addr("h2"), "read"), func() string { return rightsOf(h.addr("h2")) }},
		{memberEv(2, h.addr("h2"), "read", "answer"), func() string { return rightsOf(h.addr("h2")) }},
		{memberEv(3, "c1@c.example", "read"), func() string { return rightsOf("c1@c.example") }},
		{memberEv(4, "b1@"+local, "read"), func() string { return rightsOf("b1@" + local) }},
		{memberEv(5, "zz@"+local, "read"), func() string { return rightsOf("zz@" + local) }},
		{removeEv(6, h.addr("h2")), func() string { return rightsOf(h.addr("h2")) }},
		{removeEv(7, h.addr("never")), func() string { return rightsOf(h.addr("never")) }},
		{roomEv(8, "renamed", false), func() string { n, a := roomState(); return fmt.Sprintf("%s %v", n, a) }},
		{roomEv(9, "taken", false), func() string { n, a := roomState(); return fmt.Sprintf("%s %v", n, a) }},
		// The last bot of ours leaves: the replica is archived.
		{removeEv(10, "b1@"+local), func() string { n, a := roomState(); return fmt.Sprintf("%s %s %v", rightsOf("b1@"+local), n, a) }},
		// Invited again (the invite came first): active again.
		{memberEv(11, "b1@"+local, "read"), func() string { n, a := roomState(); return fmt.Sprintf("%s %s %v", rightsOf("b1@"+local), n, a) }},
		{roomEv(12, "taken", true), func() string { n, a := roomState(); return fmt.Sprintf("%s %v", n, a) }},
	}
	want := []string{"read", "read,answer", "read", "read", "-", "-", "-", "renamed false", "taken@" + host + " false",
		"- taken@" + host + " true", "read taken@" + host + " false", "taken true"}
	for i, st := range steps {
		if got := h.push(t, g, room, st.ev); got != st.ev.Seq {
			t.Fatalf("step %d: applied %d", i+1, got)
		}
		if got := st.check(); got != want[i] {
			t.Errorf("step %d (%s): %q, want %q", i+1, st.ev.Payload, got, want[i])
		}
	}
}

func TestReplicaCatchUp(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	h.setLog(room, roomEv(1, "proj", false), memberEv(2, h.addr("h1"), "read"), msg(3, 100, room, h.id.Domain, h.addr("h1"), "hi", nil, "", nil))
	// An archived replica is not pulled.
	gone := ulid.Make().String()
	if rec := h.invite(t, g, gone, "gone", "b1", "read"); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if _, err := g.db.Exec(`UPDATE rooms SET archived_at = '2026-01-01T00:00:00Z' WHERE id = ?`, gone); err != nil {
		t.Fatal(err)
	}
	g.svc.catchUp(context.Background())
	if appliedSeq(t, g, room) != 3 || len(storedMessages(t, g, room)) != 1 {
		t.Errorf("after catch-up: applied %d", appliedSeq(t, g, room))
	}
	for _, p := range h.pulled() {
		if strings.HasPrefix(p, gone) {
			t.Error("archived replica pulled")
		}
	}
}

// Blocking or removing a host archives its replicas here.
func TestPeerDrop_Member(t *testing.T) {
	for _, op := range []string{"block", "remove"} {
		t.Run(op, func(t *testing.T) {
			g, h, room, _ := replica(t, "read")
			if op == "block" {
				wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+h.id.Domain+"/block", ""), http.StatusOK)
			} else {
				wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, "/v1/federation/peers/"+h.id.Domain, ""), http.StatusNoContent)
			}
			if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, room); n != 1 {
				t.Error("replica not archived")
			}
			rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/events",
				map[string]any{"events": []fedrooms.Event{roomEv(1, "x", false)}}))
			if rec.Code != http.StatusForbidden || appliedSeq(t, g, room) != 0 {
				t.Errorf("push after %s: %d %s", op, rec.Code, rec.Body)
			}
		})
	}
}

// Server a hosts a room and server b replicates it: the invite, then
// messages flow and appear on b with stand-in authors.
func TestFederatedRoom_TwoServers(t *testing.T) {
	a, b := newLiveRig(t), newLiveRig(t)
	allow(t, a.svc, b.svc.id)
	allow(t, b.svc, a.svc.id)
	ctx := context.Background()
	room := a.room(t, "proj")
	a1 := a.localBot(t, room, "a1-key", "read,answer")
	if _, err := a.db.Exec(`UPDATE api_keys SET handle = 'a1' WHERE id = ?`, a1.KeyID); err != nil {
		t.Fatal(err)
	}
	rec := a.do(a1, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"before the invite","message_type":"answer"}`)
	var first struct{ ID int64 }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &first) != nil {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	b1 := b.bot(t, "b1")
	a.invite(t, room, "b1@"+b.svc.Domain(), "read,answer")
	sub, cancel := b.broker.Subscribe(room)
	defer cancel()
	a.svc.deliverDue(ctx)

	rec = a.do(a1, http.MethodPost, "/rooms/"+room+"/messages",
		fmt.Sprintf(`{"content":"after <it>","message_type":"answer","reply_to":%d}`, first.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	a.svc.deliverDue(ctx)

	var name, home string
	if err := b.db.QueryRow(`SELECT name, home_domain FROM rooms WHERE id = ?`, room).Scan(&name, &home); err != nil || name != "proj" || home != a.svc.Domain() {
		t.Fatalf("replica on b: %q %q %v", name, home, err)
	}
	if applied, total := appliedSeq(t, b, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("b applied %d of %d", applied, total)
	}
	if acked := count(t, a.db, `SELECT acked_seq FROM fed_delivery WHERE room_id = ? AND peer = ?`, room, b.svc.Domain()); int64(acked) != appliedSeq(t, b, room) {
		t.Errorf("a's cursor %d", acked)
	}
	// b1 reads both messages through b's API, authored by a1's stand-in.
	rec = b.do(auth.Claims{KeyID: b1}, http.MethodGet, "/rooms/"+room+"/messages", "")
	var list struct {
		Messages []struct {
			ID          int64           `json:"id"`
			AuthorKeyID int64           `json:"author_key_id"`
			Content     string          `json:"content"`
			TrustOrigin string          `json:"trust_origin"`
			Metadata    json.RawMessage `json:"metadata"`
		}
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Messages) != 2 {
		t.Fatalf("list on b: %d %s", rec.Code, rec.Body)
	}
	var label string
	if err := b.db.QueryRow(`SELECT label FROM api_keys WHERE id = ? AND remote_domain = ?`, list.Messages[0].AuthorKeyID, a.svc.Domain()).Scan(&label); err != nil ||
		label != "a1@"+a.svc.Domain() {
		t.Errorf("author on b: %q %v", label, err)
	}
	m0, m1 := list.Messages[0], list.Messages[1]
	if m0.Content != "before the invite" || m1.Content != "after <it>" || m0.TrustOrigin != "external" ||
		string(m1.Metadata) != fmt.Sprintf(`{"reply_to":%d}`, m0.ID) {
		t.Errorf("messages on b: %+v", list.Messages)
	}
	// Members on b: b1 (local) and a1's stand-in; a's admin key is not sent.
	rec = b.do(auth.Claims{KeyID: b1}, http.MethodGet, "/rooms/"+room+"/members", "")
	if !strings.Contains(rec.Body.String(), `"label":"a1@`+a.svc.Domain()+`"`) || !strings.Contains(rec.Body.String(), `"label":"b1"`) ||
		strings.Contains(rec.Body.String(), `"root"`) {
		t.Errorf("members on b: %s", rec.Body)
	}
	// Both reached b's live stream.
	for range 2 {
		select {
		case <-sub:
		case <-time.After(time.Second):
			t.Fatal("message missing from b's live stream")
		}
	}
	// b catches up with what it missed.
	if _, err := b.db.Exec(`DELETE FROM messages WHERE room_id = ?`, room); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE rooms SET applied_seq = 0 WHERE id = ?`, room); err != nil {
		t.Fatal(err)
	}
	b.svc.catchUp(ctx)
	if n := len(storedMessages(t, b, room)); n != 2 {
		t.Errorf("after catch-up b has %d messages", n)
	}
}
