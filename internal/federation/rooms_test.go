package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/claims"
	"github.com/hermes/conclave-mcp/internal/direct"
	"github.com/hermes/conclave-mcp/internal/events"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/follow"
	"github.com/hermes/conclave-mcp/internal/messages"
	"github.com/hermes/conclave-mcp/internal/rooms"
)

func TestMessageSig_SigningString(t *testing.T) {
	hexOf := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	reply := int64(41)
	m := MessageSig{Room: "01ROOM@a.example", Author: "b1@b.example", ContentType: "text/plain", MessageType: "answer",
		Content: "hi", Metadata: []byte(`{"k":1}`), ReplyTo: &reply, Nonce: "AAAAAAAAAAAAAAAAAAAAAA"}
	want := "conclave-msg-v1\n01ROOM@a.example\nb1@b.example\ntext/plain\nanswer\n" + hexOf("hi") + "\n" + hexOf(`{"k":1}`) +
		"\n41\nAAAAAAAAAAAAAAAAAAAAAA"
	if got := string(m.SigningString()); got != want {
		t.Errorf("signing string:\n got %q\nwant %q", got, want)
	}
	m.ReplyTo, m.Metadata = nil, nil
	want = "conclave-msg-v1\n01ROOM@a.example\nb1@b.example\ntext/plain\nanswer\n" + hexOf("hi") + "\n" + hexOf("") + "\n\nAAAAAAAAAAAAAAAAAAAAAA"
	if got := string(m.SigningString()); got != want {
		t.Errorf("without reply_to and metadata:\n got %q\nwant %q", got, want)
	}
}

func TestMessageSig_EachFieldIsSigned(t *testing.T) {
	id := testIdentity(t, "b.example")
	other := testIdentity(t, "b.example")
	reply, reply2 := int64(41), int64(42)
	base := MessageSig{Room: "01ROOM@a.example", Author: "b1@b.example", ContentType: "text/plain", MessageType: "answer",
		Content: "hi", Metadata: []byte(`{"k":1}`), ReplyTo: &reply, Nonce: newNonce()}
	sig := id.SignMessage(base)
	if !VerifyMessage(id.Public, base, sig) {
		t.Fatal("round trip does not verify")
	}
	if VerifyMessage(other.Public, base, sig) {
		t.Error("verifies with another key")
	}
	// The decoder skips CR and LF: a signature has one spelling, or a
	// copy with a line break would pass the once-per-room check.
	for _, bad := range []string{"", "x", sig[:len(sig)-2], sig + "AA", sig + "\n", sig + "\r", sig[:5] + "\r\n" + sig[5:]} {
		if VerifyMessage(id.Public, base, bad) {
			t.Errorf("malformed signature %q verifies", bad)
		}
	}
	for name, change := range map[string]func(*MessageSig){
		"room":         func(m *MessageSig) { m.Room = "01ROOM@c.example" },
		"author":       func(m *MessageSig) { m.Author = "b2@b.example" },
		"content type": func(m *MessageSig) { m.ContentType = "text/markdown" },
		"message type": func(m *MessageSig) { m.MessageType = "decision" },
		"content":      func(m *MessageSig) { m.Content = "hi!" },
		"metadata":     func(m *MessageSig) { m.Metadata = []byte(`{"k":2}`) },
		"no metadata":  func(m *MessageSig) { m.Metadata = nil },
		"reply_to":     func(m *MessageSig) { m.ReplyTo = &reply2 },
		"no reply_to":  func(m *MessageSig) { m.ReplyTo = nil },
		"nonce":        func(m *MessageSig) { m.Nonce = newNonce() },
	} {
		m := base
		change(&m)
		if VerifyMessage(id.Public, m, sig) {
			t.Errorf("%s changed, signature still verifies", name)
		}
	}
	// A nonce that is not 16 bytes of base64url is refused, signed or not.
	for _, bad := range []string{"", "AAAA", "AAAAAAAAAAAAAAAAAAAAAA==", base.Nonce + "\n", base.Nonce + "\r"} {
		m := base
		m.Nonce = bad
		if VerifyMessage(id.Public, m, id.SignMessage(m)) {
			t.Errorf("nonce %q verifies", bad)
		}
	}
}

// roomRig is a server, b.example, with the rooms, messages and claims
// services wired to its federation service as server.go wires them.
type roomRig struct {
	svc    *Service
	db     *sql.DB
	admin  auth.Claims
	routes func(chi.Router)
	fed    chi.Router
	broker *events.Broker
	direct *direct.Service
	follow *follow.Service
}

func newRoomRig(t *testing.T) *roomRig {
	t.Helper()
	return wireRig(t, newLocal(t, "allowlist"))
}

// newLiveRig is a rig whose federation routes are served on 127.0.0.1;
// its domain is the listener's address.
func newLiveRig(t *testing.T) *roomRig {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(testDB(t), Options{Domain: ln.Addr().String(), Mode: "allowlist", InsecureHTTP: true,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	g := wireRig(t, svc)
	srv := httptest.NewUnstartedServer(g.fed)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return g
}

func wireRig(t *testing.T, svc *Service) *roomRig {
	t.Helper()
	db := svc.db
	broker := events.NewBroker()
	rs := rooms.NewService(db)
	rs.FedDomain, rs.InviteRemote, rs.FedNotify, rs.Leave = svc.Domain(), svc.InviteRemote, svc.DeliverSoon, svc.LeaveRemote
	ms := messages.NewService(db, 0, broker)
	ms.FedDomain, ms.FedNotify, ms.Forward = svc.Domain(), svc.DeliverSoon, svc.ForwardMessage
	rs.MaxMessageBytes = ms.MaxMessageBytes
	cs := claims.NewService(db)
	cs.Forward = svc.ForwardClaims
	cl := chi.NewRouter()
	cs.Routes(cl)
	ds := &direct.Service{DB: db, FedDomain: svc.Domain(), MaxMessageBytes: ms.MaxMessageBytes, Send: ms.Send,
		InviteRemote: svc.InviteRemote, FedNotify: svc.DeliverSoon}
	fs := &follow.Service{DB: db, FedDomain: svc.Domain(), MaxMessageBytes: ms.MaxMessageBytes, Send: ms.SendPost, Delete: ms.Delete,
		InviteRemote: svc.InviteRemote, Request: svc.FollowRemote, Leave: svc.LeaveRemote, Withdraw: svc.WithdrawFollow, FedNotify: svc.DeliverSoon}
	svc.Rooms = RoomHooks{Send: ms.SendFromPeer, Claims: cl, MaxMessageBytes: ms.MaxMessageBytes, Replicate: ms.StoreReplica,
		Follow: fs.Remote, Withdraw: fs.RemoteWithdraw}
	fed := chi.NewRouter()
	svc.PublicRoutes(fed)
	res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('root', 'x', 'p-root', '2026-01-01T00:00:00Z', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	adminID, _ := res.LastInsertId()
	return &roomRig{svc: svc, db: db, admin: auth.Claims{KeyID: adminID, Scopes: admin}, fed: fed, broker: broker, direct: ds,
		follow: fs, routes: func(r chi.Router) { rs.Routes(r); ms.Routes(r); cs.Routes(r); ds.Routes(r); fs.Routes(r) }}
}

// bot adds a local bot with handle and returns its key.
func (g *roomRig) bot(t *testing.T, handle string) int64 {
	t.Helper()
	res, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, handle) VALUES (?, 'x', ?, '2026-01-01T00:00:00Z', ?)`,
		handle, "p-"+handle, handle)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// do serves a request of the /v1 API (path without /v1) as c.
func (g *roomRig) do(c auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, c)))
		})
	})
	g.routes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func (g *roomRig) fedDo(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.fed.ServeHTTP(rec, req)
	return rec
}

func (g *roomRig) room(t *testing.T, name string) string {
	t.Helper()
	rec := g.do(g.admin, http.MethodPost, "/rooms", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create room: %d %s", rec.Code, rec.Body)
	}
	var out struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.ID
}

// localBot adds a local key that is a member of room with rights.
func (g *roomRig) localBot(t *testing.T, room, label, rights string) auth.Claims {
	t.Helper()
	res, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES (?, 'x', ?, '2026-01-01T00:00:00Z')`, label, "p-"+label)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["%s"]}`, id, strings.ReplaceAll(rights, ",", `","`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add %s: %d %s", label, rec.Code, rec.Body)
	}
	return auth.Claims{KeyID: id}
}

func (g *roomRig) invite(t *testing.T, room, address, rights string) int64 {
	t.Helper()
	rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+address+`","rights":["`+strings.ReplaceAll(rights, ",", `","`)+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite %s: %d %s", address, rec.Code, rec.Body)
	}
	var out struct {
		KeyID   int64    `json:"key_id"`
		Address string   `json:"address"`
		Rights  []string `json:"rights"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.KeyID == 0 || out.Address != strings.ToLower(address) {
		t.Fatalf("invite answer: %s", rec.Body)
	}
	return out.KeyID
}

// state counts the rows a refused request must not touch.
func (g *roomRig) state(t *testing.T) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"messages", "fed_events", "claims", "room_members", "api_keys", "fed_delivery"} {
		parts = append(parts, fmt.Sprintf("%s=%d", table, count(t, g.db, `SELECT COUNT(*) FROM `+table)))
	}
	return strings.Join(parts, " ")
}

func (g *roomRig) events(t *testing.T, room string) []fedrooms.Event {
	t.Helper()
	rows, err := g.db.Query(`SELECT seq, type, payload FROM fed_events WHERE room_id = ? ORDER BY seq`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []fedrooms.Event
	for rows.Next() {
		var e fedrooms.Event
		var p string
		if err := rows.Scan(&e.Seq, &e.Type, &p); err != nil {
			t.Fatal(err)
		}
		e.Payload = json.RawMessage(p)
		if e.Seq != int64(len(out)+1) {
			t.Fatalf("seq %d after %d events: a gap", e.Seq, len(out))
		}
		out = append(out, e)
	}
	return out
}

// memberServer is a member server of the rig: an allowed peer on
// 127.0.0.1 that checks the host's signature on what it receives and
// answers with reply, when set, else with answer (201 when 0) and no
// body. A reply of status 0 drops the connection.
type memberServer struct {
	id      *Identity
	answer  atomic.Int32
	mu      sync.Mutex
	invites []string // "method path body" of each signed request
	reply   func(path string, body []byte) (int, string)
}

// setReply sets m's reply.
func (m *memberServer) setReply(f func(path string, body []byte) (int, string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reply = f
}

// applyAll is a reply that applies every pushed event and accepts
// everything else.
func applyAll(path string, body []byte) (int, string) {
	var in struct{ Events []fedrooms.Event }
	if !strings.HasSuffix(path, "/events") || json.Unmarshal(body, &in) != nil || len(in.Events) == 0 {
		return http.StatusCreated, ""
	}
	return http.StatusOK, fmt.Sprintf(`{"applied":%d}`, in.Events[len(in.Events)-1].Seq)
}

func newMemberServer(t *testing.T, g *roomRig) *memberServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &memberServer{id: testIdentity(t, ln.Addr().String())}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p, err := ParseSigHeader(r.Header.Get("Authorization"))
		if err != nil || p.Origin != local ||
			!ed25519.Verify(g.svc.id.Public, SigningString(r.Method, r.RequestURI, m.id.Domain, local, p.TS, p.Nonce, body), p.Sig) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		m.invites = append(m.invites, r.Method+" "+r.URL.Path+" "+string(body))
		reply := m.reply
		m.mu.Unlock()
		if reply != nil {
			status, out := reply(r.URL.Path, body)
			if status == 0 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
			return
		}
		status := int(m.answer.Load())
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	allow(t, g.svc, m.id)
	return m
}

func (m *memberServer) addr(name string) string { return name + "@" + m.id.Domain }

func (m *memberServer) received() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.invites...)
}

// req is a request to the host signed by m; body is marshalled unless
// it is nil.
func (m *memberServer) req(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	return signedReq(t, m.id, local, method, path, body)
}

// signedReq is a request to dest signed by id; body is marshalled
// unless it is nil.
func signedReq(t *testing.T, id *Identity, dest, method, path string, body any) *http.Request {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Host = dest
	if err := id.SignRequest(req, dest, b); err != nil {
		t.Fatal(err)
	}
	return req
}

// proposal is a message proposal by author, signed by m.
func (m *memberServer) proposal(room, author, msgType, content string, metadata json.RawMessage, replyTo *int64) map[string]any {
	wire, _, _ := fedrooms.SplitMetadata(metadata)
	nonce := newNonce()
	sig := m.id.SignMessage(MessageSig{Room: room + "@" + local, Author: author, ContentType: "text/plain",
		MessageType: msgType, Content: content, Metadata: wire, ReplyTo: replyTo, Nonce: nonce})
	p := map[string]any{"author": author, "content": content, "content_type": "text/plain", "message_type": msgType, "sig": sig,
		"nonce": nonce}
	if metadata != nil {
		p["metadata"] = metadata
	}
	if replyTo != nil {
		p["reply_to"] = *replyTo
	}
	return p
}

func TestInviteRemote(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "proj")
	bot := g.localBot(t, room, "local-bot", "read,answer")
	if rec := g.do(bot, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"before","message_type":"answer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("local message: %d %s", rec.Code, rec.Body)
	}
	before := g.state(t)
	inv := func(c auth.Claims, body string) *httptest.ResponseRecorder {
		return g.do(c, http.MethodPost, "/rooms/"+room+"/members", body)
	}
	seen, err := LoadOrCreateIdentity(context.Background(), testDB(t), "seen.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.peers.RecordSeen(context.Background(), seen.Domain, seen.Public); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		caller       auth.Claims
		body         string
		status       int
		code, answer string
	}{
		{bot, `{"address":"` + m.addr("c1") + `","rights":["read"]}`, 403, "forbidden", ""},
		{g.admin, `{"address":"c1@unknown.example","rights":["read"]}`, 403, "peer_not_allowed", ""},
		{g.admin, `{"address":"c1@seen.example","rights":["read"]}`, 403, "peer_not_allowed", ""},
		{g.admin, `{"address":"c1@b.example","rights":["read"]}`, 400, "own_domain", ""},
		{g.admin, `{"address":"operator@` + m.id.Domain + `","rights":["read"]}`, 400, "invalid_address", ""},
		{g.admin, `{"address":"c1","rights":["read"]}`, 400, "invalid_address", ""},
		{g.admin, `{"address":"c 1@` + m.id.Domain + `","rights":["read"]}`, 400, "invalid_address", ""},
		{g.admin, `{"address":"` + m.addr("c1") + `","key_id":5}`, 400, "bad_request", ""},
		{g.admin, `{"address":"` + m.addr("c1") + `","rights":["fly"]}`, 400, "bad_request", ""},
		{g.admin, `{"address":"` + m.addr("nobody") + `","rights":["read"]}`, 404, "not_found", "404"},
		{g.admin, `{"address":"` + m.addr("c1") + `","rights":["read"]}`, 403, "invite_refused", "403"},
		{g.admin, `{"address":"` + m.addr("c1") + `","rights":["read"]}`, 502, "peer_unreachable", "500"},
	} {
		n, _ := strconv.Atoi(c.answer)
		m.answer.Store(int32(n))
		wantErr(t, inv(c.caller, c.body), c.status, c.code)
		if got := g.state(t); got != before {
			t.Errorf("%s: stored something: %s, want %s", c.body, got, before)
		}
	}
	if got := len(m.received()); got != 3 {
		t.Errorf("member server got %d invites, want 3 (only the ones it answered)", got)
	}

	m.answer.Store(0)
	c1 := g.invite(t, room, m.addr("C1"), "read,answer,claim") // addresses are lowercased
	got := m.received()
	want := fmt.Sprintf(`POST /fed/v1/rooms/%s/invite {"name":"proj","handle":"c1","rights":["read","claim","answer"],"kind":"room"}`, room)
	if len(got) != 4 || got[3] != want {
		t.Errorf("invite sent: %q, want %q", got[len(got)-1], want)
	}
	var label, trust, remote, rights string
	if err := g.db.QueryRow(`SELECT k.label, k.trust_origin, k.remote_domain, m.rights FROM api_keys k JOIN room_members m ON m.key_id = k.id
		WHERE k.id = ? AND m.room_id = ?`, c1, room).Scan(&label, &trust, &remote, &rights); err != nil {
		t.Fatal(err)
	}
	if label != m.addr("c1") || trust != "external" || remote != m.id.Domain || rights != "read,claim,answer" {
		t.Errorf("stand-in: %s %s %s %s", label, trust, remote, rights)
	}
	// The log starts with the snapshot: room, members (not the admin
	// key), the message so far.
	evs := g.events(t, room)
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	if strings.Join(types, " ") != "room member member message" {
		t.Errorf("snapshot: %v", types)
	}
	if string(evs[2].Payload) != `{"address":"`+m.addr("c1")+`","rights":["read","claim","answer"]}` {
		t.Errorf("member event: %s", evs[2].Payload)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM fed_delivery WHERE room_id = ? AND peer = ? AND acked_seq = 0`, room, m.id.Domain); n != 1 {
		t.Errorf("delivery rows: %d", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND target_id = ? AND result = 'ok'`, m.id.Domain); n != 1 {
		t.Errorf("federation.room.invite audit rows: %d", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'room.invite' AND detail LIKE ?`, `%"address":"`+m.addr("c1")+`"%`); n != 1 {
		t.Errorf("room.invite audit rows with the address: %d", n)
	}
	// Inviting a member again is refused before anything is sent.
	wantErr(t, inv(g.admin, `{"address":"`+m.addr("c1")+`","rights":["read"]}`), 409, "conflict")
	if len(m.received()) != 4 {
		t.Error("an invite was sent for a member")
	}
	// A stand-in is never invited by key id; a remote bot comes in by address.
	wantErr(t, inv(g.admin, fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, c1)), 400, "bad_request")
	// Rights changes of the stand-in are logged.
	if rec := g.do(g.admin, http.MethodPatch, fmt.Sprintf("/rooms/%s/members/%d", room, c1), `{"rights":["read"]}`); rec.Code != 200 {
		t.Fatalf("set rights: %d %s", rec.Code, rec.Body)
	}
	if evs := g.events(t, room); evs[len(evs)-1].Type != fedrooms.EventMember ||
		string(evs[len(evs)-1].Payload) != `{"address":"`+m.addr("c1")+`","rights":["read"]}` {
		t.Errorf("rights event: %s", evs[len(evs)-1].Payload)
	}
}

func TestInviteRemote_ArchivedRoom(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "old")
	if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/archive", ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	wantErr(t, g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+m.addr("c1")+`","rights":["read"]}`), 410, "not_found")
	if len(m.received()) != 0 {
		t.Error("invite sent for an archived room")
	}
}

// fedRoom is a rig with a room in which m's bot c1 holds rights.
func fedRoom(t *testing.T, rights string) (*roomRig, *memberServer, string, int64) {
	t.Helper()
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "proj")
	return g, m, room, g.invite(t, room, m.addr("c1"), rights)
}

func TestRoomMessage_FromMember(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read,answer")
	sub, cancel := g.broker.Subscribe(room)
	defer cancel()
	path := "/fed/v1/rooms/" + room + "/messages"
	rec := g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "hello", nil, nil)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("proposal: %d %s", rec.Code, rec.Body)
	}
	// Local readers get it as external, with the author's address, live
	// and listed.
	want := `"trust_origin":"external","author_address":"` + m.addr("c1") + `"`
	if live := string((<-sub).Payload); !strings.Contains(live, want) || strings.Contains(live, "operator") {
		t.Errorf("live: %s", live)
	}
	if rec := g.do(g.admin, http.MethodGet, "/rooms/"+room+"/messages", ""); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("list: %s", rec.Body)
	}
	var ev fedrooms.Event
	var p fedrooms.MessagePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil || json.Unmarshal(ev.Payload, &p) != nil {
		t.Fatalf("answer is not an event: %s", rec.Body)
	}
	evs := g.events(t, room)
	if last := evs[len(evs)-1]; ev.Seq != last.Seq || ev.Type != fedrooms.EventMessage || string(ev.Payload) != string(last.Payload) {
		t.Errorf("answered %d %s %s, logged %d %s %s", ev.Seq, ev.Type, ev.Payload, last.Seq, last.Type, last.Payload)
	}
	var author int64
	var trust, sig string
	var operator bool
	if err := g.db.QueryRow(`SELECT author_key_id, trust_origin, origin_sig, operator FROM messages WHERE id = ?`, p.ID).
		Scan(&author, &trust, &sig, &operator); err != nil {
		t.Fatal(err)
	}
	if author != c1 || trust != "external" || sig != p.OriginSig || sig == "" || operator {
		t.Errorf("stored: author %d trust %s sig %q operator %v", author, trust, sig, operator)
	}

	// A reply: the host's reply_to is checked and stored in metadata;
	// the event carries metadata without it, and the author signature
	// verifies from the event alone.
	first := p.ID
	rec = g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "re",
		json.RawMessage(`{"x": 1, "reply_to": 999}`), &first)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	p = fedrooms.MessagePayload{}
	_ = json.Unmarshal(ev.Payload, &p)
	var meta string
	if err := g.db.QueryRow(`SELECT metadata FROM messages WHERE id = ?`, p.ID).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != fmt.Sprintf(`{"reply_to":%d,"x":1}`, first) || string(p.Metadata) != `{"x":1}` || p.ReplyTo == nil || *p.ReplyTo != first {
		t.Errorf("stored metadata %s, event metadata %s reply_to %v", meta, p.Metadata, p.ReplyTo)
	}
	ms := MessageSig{Room: room + "@" + local, Author: p.Author, ContentType: p.ContentType, MessageType: p.MessageType,
		Content: p.Content, Metadata: p.Metadata, ReplyTo: p.ReplyTo, Nonce: p.Nonce}
	if !VerifyMessage(m.id.Public, ms, p.OriginSig) {
		t.Error("author signature does not verify from the event")
	}
	// A reply_to that is not in the room is refused like a local one.
	bad := int64(999)
	before := g.state(t)
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "x", nil, &bad))), 400, "bad_request")
	if g.state(t) != before {
		t.Error("bad reply_to stored something")
	}
}

func TestRoomMessage_SizeCap(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	path := "/fed/v1/rooms/" + room + "/messages"
	// Over the 64 KiB body cap of other federation requests, under the message cap.
	big := strings.Repeat("x", 100<<10)
	if rec := g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", big, nil, nil))); rec.Code != http.StatusCreated {
		t.Fatalf("100 KiB message: %d %s", rec.Code, rec.Body)
	}
	before := g.state(t)
	huge := strings.Repeat("x", 256<<10+1)
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", huge, nil, nil))), 413, "payload_too_large")
	if g.state(t) != before {
		t.Error("oversized message stored something")
	}
}

func TestRoomMessage_Refusals(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	c2 := g.invite(t, room, m.addr("c2"), "read,answer")
	if rec := g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, c2), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove c2: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'room.remove' AND detail LIKE ?`, `%"address":"`+m.addr("c2")+`"%`); n != 1 {
		t.Errorf("room.remove audit rows with the address: %d", n)
	}
	other := g.room(t, "other")
	path := "/fed/v1/rooms/" + room + "/messages"
	goodSig := m.proposal(room, m.addr("c1"), "answer", "signed text", nil, nil)
	forged := m.proposal(room, m.addr("c1"), "answer", "other text", nil, nil)
	forged["content"] = "signed text"
	forged["sig"] = goodSig["sig"].(string)[:10] + strings.Repeat("A", len(goodSig["sig"].(string))-10)
	tampered := m.proposal(room, m.addr("c1"), "answer", "signed text", nil, nil)
	tampered["content"] = "changed text"
	for name, c := range map[string]struct {
		path   string
		body   map[string]any
		status int
		code   string
	}{
		"never invited":           {path, m.proposal(room, m.addr("c3"), "answer", "x", nil, nil), 404, "not_found"},
		"removed":                 {path, m.proposal(room, m.addr("c2"), "answer", "x", nil, nil), 404, "not_found"},
		"room without its bots":   {"/fed/v1/rooms/" + other + "/messages", m.proposal(other, m.addr("c1"), "answer", "x", nil, nil), 404, "not_found"},
		"unknown room":            {"/fed/v1/rooms/01NOROOM/messages", m.proposal("01NOROOM", m.addr("c1"), "answer", "x", nil, nil), 404, "not_found"},
		"missing right":           {path, m.proposal(room, m.addr("c1"), "decision", "x", nil, nil), 403, "missing_right"},
		"another server's author": {path, m.proposal(room, "c1@c.example", "answer", "x", nil, nil), 403, "wrong_origin"},
		"bad author syntax":       {path, m.proposal(room, "c1", "answer", "x", nil, nil), 400, "bad_request"},
		"bad author signature":    {path, forged, 403, "bad_author_signature"},
		"tampered content":        {path, tampered, 403, "bad_author_signature"},
		"bad metadata":            {path, m.proposal(room, m.addr("c1"), "answer", "x", json.RawMessage(`[1]`), nil), 400, "bad_request"},
	} {
		before := g.state(t)
		wantErr(t, g.fedDo(m.req(t, http.MethodPost, c.path, c.body)), c.status, c.code)
		if got := g.state(t); got != before {
			t.Errorf("%s: stored something: %s, want %s", name, got, before)
		}
	}

	// Archived room.
	if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/archive", ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	before := g.state(t)
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "x", nil, nil))), 410, "not_found")
	if g.state(t) != before {
		t.Error("archived room: stored something")
	}
}

func TestRoomMessage_PeerNotAllowed(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	path := "/fed/v1/rooms/" + room + "/messages"
	// In open mode a peer that is only seen gets past the signature
	// check, but rooms take only allowed peers.
	g.svc.mode = modeOpen
	if _, err := g.db.Exec(`UPDATE federation_peers SET status = 'seen' WHERE domain = ?`, m.id.Domain); err != nil {
		t.Fatal(err)
	}
	before := g.state(t)
	for _, req := range []*http.Request{
		m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "x", nil, nil)),
		m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events", nil),
		m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/claims?author="+m.addr("c1"), nil),
		m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/leave", map[string]string{"author": m.addr("c1")}),
	} {
		wantErr(t, g.fedDo(req), 403, "peer_not_allowed")
	}
	if g.svc.peers.Block(context.Background(), m.id.Domain) != nil {
		t.Fatal("block")
	}
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "x", nil, nil))), 403, "peer_not_allowed")
	if g.state(t) != before {
		t.Error("a peer that is not allowed stored something")
	}
}

func TestRoomClaims_FromMember(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read,claim")
	g.invite(t, room, m.addr("c2"), "read")
	bot := g.localBot(t, room, "local-bot", "read,claim")
	if rec := g.do(bot, http.MethodPost, "/rooms/"+room+"/claims", `{"paths":["a/"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("local claim: %d %s", rec.Code, rec.Body)
	}
	base := "/fed/v1/rooms/" + room + "/claims"
	as := "?author=" + m.addr("c1")
	before := g.state(t)
	rec := g.fedDo(m.req(t, http.MethodPost, base+as, map[string]any{"paths": []string{"a/b.go"}}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"label":"local-bot"`) {
		t.Errorf("overlap: %d %s, want 409 naming local-bot", rec.Code, rec.Body)
	}
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, base+"?author="+m.addr("c2"), map[string]any{"paths": []string{"x/"}})), 403, "missing_right")
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, base+"?author=c1@c.example", map[string]any{"paths": []string{"x/"}})), 403, "wrong_origin")
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, base, map[string]any{"paths": []string{"x/"}})), 400, "bad_request")
	if g.state(t) != before {
		t.Error("refused claims stored something")
	}
	if rec := g.fedDo(m.req(t, http.MethodPost, base+as, map[string]any{"paths": []string{"x/"}})); rec.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM claims WHERE room_id = ? AND key_id = ? AND path = 'x/'`, room, c1); n != 1 {
		t.Errorf("claim of the stand-in: %d rows", n)
	}
	rec = g.fedDo(m.req(t, http.MethodGet, base+as, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"label":"`+m.addr("c1")+`"`) ||
		!strings.Contains(rec.Body.String(), `"trust_origin":"external"`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := g.fedDo(m.req(t, http.MethodPost, base+"/heartbeat"+as, map[string]any{})); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"refreshed":1}` {
		t.Errorf("heartbeat: %d %s", rec.Code, rec.Body)
	}
	// A stand-in is no admin key: it releases only its own claims.
	wantErr(t, g.fedDo(m.req(t, http.MethodPost, base+"/release"+as, map[string]any{"key_id": bot.KeyID})), 403, "forbidden")
	if rec := g.fedDo(m.req(t, http.MethodPost, base+"/release"+as, nil)); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"released":["x/"]}` {
		t.Errorf("release: %d %s", rec.Code, rec.Body)
	}
}

func TestRoomLeave(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read")
	leave := func(author string) *httptest.ResponseRecorder {
		return g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/leave", map[string]string{"author": author}))
	}
	before := g.state(t)
	wantErr(t, leave("c1@c.example"), 403, "wrong_origin")
	wantErr(t, leave(m.addr("c9")), 404, "not_found")
	if g.state(t) != before {
		t.Error("refused leave changed something")
	}
	if rec := leave(m.addr("c1")); rec.Code != http.StatusNoContent {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id = ?`, room, c1); n != 0 {
		t.Error("still a member")
	}
	evs := g.events(t, room)
	if last := evs[len(evs)-1]; last.Type != fedrooms.EventMemberRemove || string(last.Payload) != `{"address":"`+m.addr("c1")+`"}` {
		t.Errorf("last event: %s %s", last.Type, last.Payload)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.leave' AND target_id = ?`, m.id.Domain); n != 1 {
		t.Errorf("federation.room.leave audit rows: %d", n)
	}
	// Its server has no bot left in the room.
	wantErr(t, leave(m.addr("c1")), 404, "not_found")
}

func TestRoomEvents(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	for i := range 120 {
		if rec := g.do(g.admin, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"m`+strconv.Itoa(i)+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("send: %d %s", rec.Code, rec.Body)
		}
	}
	total := len(g.events(t, room))
	get := func(query string) []fedrooms.Event {
		t.Helper()
		rec := g.fedDo(m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("events%s: %d %s", query, rec.Code, rec.Body)
		}
		var out struct{ Events []fedrooms.Event }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Events
	}
	page := get("")
	if len(page) != 100 || page[0].Seq != 1 || page[99].Seq != 100 || page[0].Type != fedrooms.EventRoom {
		t.Errorf("first page: %d events", len(page))
	}
	page = get("?since=100")
	if len(page) != total-100 || page[0].Seq != 101 {
		t.Errorf("second page: %d events from %d, want %d", len(page), page[0].Seq, total-100)
	}
	if page := get(fmt.Sprintf("?since=%d", total)); len(page) != 0 {
		t.Errorf("past the end: %d events", len(page))
	}
	for _, q := range []string{"?since=-1", "?since=x", "?since=1.5"} {
		wantErr(t, g.fedDo(m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events"+q, nil)), 400, "bad_request")
	}
	// Only servers with a bot in the room read its log.
	other := newMemberServer(t, g)
	wantErr(t, g.fedDo(other.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events", nil)), 404, "not_found")
	// A replica here is no room this server hosts.
	if _, err := g.db.Exec(`UPDATE rooms SET home_domain = 'a.example' WHERE id = ?`, room); err != nil {
		t.Fatal(err)
	}
	wantErr(t, g.fedDo(m.req(t, http.MethodGet, "/fed/v1/rooms/"+room+"/events", nil)), 404, "not_found")
}

// Concurrent changes from the member server and from local keys each
// get their own event, numbered without gaps.
func TestRoomEvents_ConcurrentChangesHaveNoGaps(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read,answer")
	bot := g.localBot(t, room, "local-bot", "read,answer")
	start := len(g.events(t, room))
	path := "/fed/v1/rooms/" + room + "/messages"
	reqs := make([]*http.Request, 8)
	for i := range reqs {
		reqs[i] = m.req(t, http.MethodPost, path, m.proposal(room, m.addr("c1"), "answer", "remote "+strconv.Itoa(i), nil, nil))
	}
	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for i := range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if rec := g.fedDo(reqs[i]); rec.Code != http.StatusCreated {
				errs <- fmt.Sprintf("proposal: %d %s", rec.Code, rec.Body)
			}
		}()
		go func() {
			defer wg.Done()
			if rec := g.do(bot, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"local","message_type":"answer"}`); rec.Code != http.StatusCreated {
				errs <- fmt.Sprintf("local send: %d %s", rec.Code, rec.Body)
			}
		}()
		go func() {
			defer wg.Done()
			r := []string{`["read","answer"]`, `["read","claim","answer"]`}[i%2]
			if rec := g.do(g.admin, http.MethodPatch, fmt.Sprintf("/rooms/%s/members/%d", room, c1), `{"rights":`+r+`}`); rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("rights: %d %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	evs := g.events(t, room) // fails on a gap
	if got := len(evs) - start; got != 24 {
		t.Errorf("%d events for 24 changes", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM fed_events WHERE room_id = ? AND type = 'message'`, room); n != count(t, g.db, `SELECT COUNT(*) FROM messages WHERE room_id = ?`, room) {
		t.Errorf("%d message events", n)
	}
}

// The log is written in the transaction of the change: when it cannot
// be written, the change is not made.
func TestRoomEvents_SameTransaction(t *testing.T) {
	g, m, room, c1 := fedRoom(t, "read,answer")
	bot := g.localBot(t, room, "local-bot", "read,answer")
	res, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('joiner', 'x', 'p-joiner', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	joiner, _ := res.LastInsertId()
	if _, err := g.db.Exec(`CREATE TRIGGER fail_log BEFORE INSERT ON fed_events BEGIN SELECT RAISE(ABORT, 'log down'); END`); err != nil {
		t.Fatal(err)
	}
	before := g.state(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"local send":  g.do(bot, http.MethodPost, "/rooms/"+room+"/messages", `{"content":"x","message_type":"answer"}`),
		"rights":      g.do(g.admin, http.MethodPatch, fmt.Sprintf("/rooms/%s/members/%d", room, c1), `{"rights":["read"]}`),
		"remove":      g.do(g.admin, http.MethodDelete, fmt.Sprintf("/rooms/%s/members/%d", room, c1), ""),
		"local join":  g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, joiner)),
		"archive":     g.do(g.admin, http.MethodPost, "/rooms/"+room+"/archive", ""),
		"remote join": g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+m.addr("c2")+`","rights":["read"]}`),
		"proposal":    g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/messages", m.proposal(room, m.addr("c1"), "answer", "x", nil, nil))),
		"leave":       g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/leave", map[string]string{"author": m.addr("c1")})),
	} {
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %s, want 500", name, rec.Code, rec.Body)
		}
	}
	var rights string
	var archived sql.NullString
	if err := g.db.QueryRow(`SELECT m.rights, r.archived_at FROM room_members m JOIN rooms r ON r.id = m.room_id WHERE m.room_id = ? AND m.key_id = ?`, room, c1).
		Scan(&rights, &archived); err != nil || rights != "read,answer" || archived.Valid {
		t.Errorf("c1 rights %q, archived %v, err %v", rights, archived, err)
	}
	if got := g.state(t); got != before {
		t.Errorf("changes made without their events: %s, want %s", got, before)
	}
}

// A member server that sends a proposal again (a retry) gets the
// stored message's event back; nothing is stored twice.
func TestRoomMessage_RetryReturnsOriginal(t *testing.T) {
	g, m, room, _ := fedRoom(t, "read,answer")
	path := "/fed/v1/rooms/" + room + "/messages"
	p := m.proposal(room, m.addr("c1"), "answer", "once", nil, nil)
	first := g.fedDo(m.req(t, http.MethodPost, path, p))
	if first.Code != http.StatusCreated {
		t.Fatalf("proposal: %d %s", first.Code, first.Body)
	}
	before := g.state(t)
	again := g.fedDo(m.req(t, http.MethodPost, path, p))
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Errorf("retry: %d %s, want the first answer %s", again.Code, again.Body, first.Body)
	}
	if g.state(t) != before {
		t.Errorf("retry stored something: %s, want %s", g.state(t), before)
	}
}

// A local key whose label is the address leaves no room for a
// stand-in: the invite is refused before the bot's server is asked.
func TestInviteRemote_LabelOfLocalKey(t *testing.T) {
	g := newRoomRig(t)
	m := newMemberServer(t, g)
	room := g.room(t, "proj")
	if _, err := g.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES (?, 'x', 'p-x', '2026-01-01T00:00:00Z')`, m.addr("c1")); err != nil {
		t.Fatal(err)
	}
	before := g.state(t)
	wantErr(t, g.do(g.admin, http.MethodPost, "/rooms/"+room+"/members", `{"address":"`+m.addr("c1")+`","rights":["read"]}`), 409, "conflict")
	if len(m.received()) != 0 || g.state(t) != before {
		t.Errorf("invite sent (%q) or stored something", m.received())
	}
}
