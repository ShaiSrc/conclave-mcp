package direct

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/messages"
	"github.com/hermes/conclave-mcp/internal/rooms"
	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

// env is a server with the direct, messages and rooms routes, an admin
// key and the bots a test adds. A request's X-Key header names its key.
type env struct {
	t     *testing.T
	db    *sql.DB
	h     http.Handler
	admin int64
}

func newEnv(t *testing.T, domain string) *env {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db}
	e.admin = e.key("root", "", true)
	msgs := messages.NewService(db, 1024, nil)
	svc := &Service{DB: db, FedDomain: domain, MaxMessageBytes: msgs.MaxMessageBytes, Send: msgs.Send}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, _ := strconv.ParseInt(req.Header.Get("X-Key"), 10, 64)
			c := auth.Claims{KeyID: id}
			if id == e.admin {
				c.Scopes = []string{auth.ScopeAdmin}
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, c)))
		})
	})
	svc.Routes(r)
	msgs.Routes(r)
	rooms.NewService(db).Routes(r)
	e.h = r
	return e
}

// key adds a key labelled label with handle ("" for none).
func (e *env) key(label, handle string, admin bool) int64 {
	e.t.Helper()
	var h any
	if handle != "" {
		h = handle
	}
	res, err := e.db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin, handle)
		VALUES (?, 'x', ?, '2026-01-01T00:00:00Z', ?, ?)`, label, label, admin, h)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// bot adds a bot with a profile row of policy, or none for "".
func (e *env) bot(handle, policy string) int64 {
	e.t.Helper()
	id := e.key(handle, handle, false)
	if policy != "" {
		if _, err := e.db.Exec(`INSERT INTO bot_profiles(key_id, updated_at, dm_policy) VALUES (?, '2026-01-01T00:00:00Z', ?)`,
			id, policy); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func (e *env) do(key int64, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Key", strconv.FormatInt(key, 10))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) start(from int64, to string) *httptest.ResponseRecorder {
	return e.do(from, http.MethodPost, "/direct", `{"to":"`+to+`","content":"hi","message_type":"question"}`)
}

// started returns the room id of a 201 answer to POST /direct.
func (e *env) started(rec *httptest.ResponseRecorder) string {
	e.t.Helper()
	var out struct {
		RoomID  string `json:"room_id"`
		Message struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.RoomID == "" || out.Message.ID == 0 {
		e.t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	return out.RoomID
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Errorf("got %d %s; want %d %s", rec.Code, rec.Body, status, code)
	}
}

// The recipient's policy decides whether a local bot may start a
// conversation: none refuses, local and peers accept, and a bot without
// a profile row has local.
func TestStart_PolicyMatrix(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	e.bot("none", "none")
	e.bot("local", "local")
	e.bot("peers", "peers")
	e.bot("norow", "")
	for to, ok := range map[string]bool{"none": false, "local": true, "peers": true, "norow": true} {
		rec := e.start(alice, to)
		if ok {
			e.started(rec)
		} else {
			wantErr(t, rec, http.StatusForbidden, "dm_refused")
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms WHERE kind = 'direct'`); n != 3 {
		t.Errorf("%d direct rooms, want 3", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'direct.refused' AND detail LIKE '%"reason":"policy"%'`); n != 1 {
		t.Errorf("%d policy refusals audited, want 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'direct.start' AND actor_key_id = ?`, alice); n != 3 {
		t.Errorf("%d starts audited, want 3", n)
	}
	wantErr(t, e.start(alice, "nobody"), http.StatusNotFound, "not_found")
	wantErr(t, e.start(alice, "alice"), http.StatusBadRequest, "bad_request")
	wantErr(t, e.start(alice, "bad name"), http.StatusBadRequest, "bad_request")
	wantErr(t, e.start(alice, "local@peer.example"), http.StatusNotFound, "federation_disabled")
}

// Only bots take part: admin keys and keys without a handle cannot
// start one, and an admin key cannot receive one.
func TestStart_OnlyBots(t *testing.T) {
	e := newEnv(t, "")
	bob := e.bot("bob", "")
	plain := e.key("plain", "", false)
	wantErr(t, e.start(e.admin, "bob"), http.StatusForbidden, "forbidden")
	wantErr(t, e.start(plain, "bob"), http.StatusForbidden, "forbidden")
	wantErr(t, e.do(e.admin, http.MethodGet, "/direct", ""), http.StatusForbidden, "forbidden")
	// Admin keys have no handle; one given by hand is still refused.
	if _, err := e.db.Exec(`UPDATE api_keys SET handle = 'boss' WHERE id = ?`, e.admin); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.start(bob, "boss"), http.StatusForbidden, "dm_refused")
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'direct.refused' AND detail LIKE '%"reason":"not a bot"%'`); n != 3 {
		t.Errorf("%d not-a-bot refusals audited, want 3", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms", n)
	}
}

// A pair has one conversation, whichever bot starts it; it stays usable
// when the recipient's policy narrows. Its name is made of the sorted
// addresses, and our own domain in to is a local recipient.
func TestStart_OneRoomPerPair(t *testing.T) {
	e := newEnv(t, "x.example")
	alice, bob := e.bot("alice", ""), e.bot("bob", "")
	room := e.started(e.start(bob, "alice"))
	if got := e.started(e.start(alice, "BOB@X.Example")); got != room {
		t.Errorf("alice starts: room %s, want %s", got, room)
	}
	if _, err := e.db.Exec(`INSERT INTO bot_profiles(key_id, updated_at, dm_policy) VALUES (?, '2026-01-01T00:00:00Z', 'none')`, bob); err != nil {
		t.Fatal(err)
	}
	if got := e.started(e.start(alice, "bob")); got != room {
		t.Errorf("after bob's policy narrowed: room %s, want %s", got, room)
	}
	var name string
	if err := e.db.QueryRow(`SELECT name FROM rooms WHERE id = ?`, room).Scan(&name); err != nil || name != "dm:alice@x.example~bob@x.example" {
		t.Errorf("name %q, %v", name, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM room_members WHERE room_id = ? AND key_id IN (?, ?)
		AND rights = 'read,question,answer,decision,handoff,status_update,task_assignment,alert'`, room, alice, bob); n != 2 {
		t.Errorf("%d members with the direct rights, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages WHERE room_id = ?`, room); n != 3 {
		t.Errorf("%d messages, want 3", n)
	}
	body := e.do(alice, http.MethodGet, "/rooms?kind=direct", "").Body.String()
	if !strings.Contains(body, `"id":"`+room+`"`) || !strings.Contains(body, `"kind":"direct"`) {
		t.Errorf("rooms?kind=direct: %s", body)
	}
	if body := e.do(alice, http.MethodGet, "/rooms?kind=room", "").Body.String(); strings.Contains(body, room) {
		t.Errorf("rooms?kind=room: %s", body)
	}
}

// A send the normal path refuses leaves no new conversation behind.
func TestStart_RefusedSendRemovesNewRoom(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	e.bot("bob", "")
	// Without a type a message is a system message, which is not a right here.
	wantErr(t, e.do(alice, http.MethodPost, "/direct", `{"to":"bob","content":"hi"}`), http.StatusForbidden, "missing_right")
	big := `{"to":"bob","message_type":"question","content":"` + strings.Repeat("x", 1100) + `"}`
	wantErr(t, e.do(alice, http.MethodPost, "/direct", big), http.StatusRequestEntityTooLarge, "payload_too_large")
	if n := e.count(`SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms left", n)
	}
	room := e.started(e.start(alice, "bob"))
	wantErr(t, e.do(alice, http.MethodPost, "/direct", `{"to":"bob","content":"hi"}`), http.StatusForbidden, "missing_right")
	if n := e.count(`SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NULL`, room); n != 1 {
		t.Errorf("an existing conversation was removed")
	}
}

// Direct rooms take no invites, rights changes or new members; an admin
// may remove a member, which archives the room.
func TestRooms_DirectRefusals(t *testing.T) {
	e := newEnv(t, "")
	alice, bob, carol := e.bot("alice", ""), e.bot("bob", ""), e.bot("carol", "")
	room := e.started(e.start(alice, "bob"))
	base := "/rooms/" + room
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, carol)},
		{http.MethodPost, base + "/invite", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, carol)},
		{http.MethodPost, base + "/members", `{"address":"c@peer.example","rights":["read"]}`},
		{http.MethodPatch, fmt.Sprintf("%s/members/%d", base, bob), `{"rights":["read","system"]}`},
	} {
		wantErr(t, e.do(e.admin, c.method, c.path, c.body), http.StatusForbidden, "direct_room")
	}
	// A bot in a conversation is not "in another room" for an invite.
	normal := e.do(e.admin, http.MethodPost, "/rooms", `{"name":"proj"}`)
	var nr struct{ ID string }
	_ = json.Unmarshal(normal.Body.Bytes(), &nr)
	if rec := e.do(e.admin, http.MethodPost, "/rooms/"+nr.ID+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, alice)); rec.Code != http.StatusCreated {
		t.Errorf("invite of a bot with a conversation: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(e.admin, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, bob), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms WHERE id = ? AND archived_at IS NOT NULL`, room); n != 1 {
		t.Errorf("the room is not archived")
	}
	if got := e.started(e.start(alice, "bob")); got == room {
		t.Errorf("a new start reused the archived room")
	}
}

// 30 new conversations an hour per sender; the 31st gets 429, while an
// existing one still takes messages.
func TestStart_RateLimit(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	for i := range 31 {
		e.bot(fmt.Sprintf("b%d", i), "")
	}
	for i := range 30 {
		e.started(e.start(alice, fmt.Sprintf("b%d", i)))
	}
	rec := e.start(alice, "b30")
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	e.started(e.start(alice, "b0"))
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'direct.refused' AND detail LIKE '%"reason":"limit"%'`); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	e.started(e.start(e.bot("other", ""), "b30")) // the limit is per sender
}

// GET /direct lists the caller's conversations with the other address,
// the last message time and the messages of the other bot it has not
// read; reading the room's messages marks them read.
func TestList_Unread(t *testing.T) {
	e := newEnv(t, "")
	alice, bob := e.bot("alice", ""), e.bot("bob", "")
	room := e.started(e.start(alice, "bob"))
	e.started(e.start(alice, "bob"))
	type conv struct {
		RoomID        string `json:"room_id"`
		Address       string `json:"address"`
		LastMessageAt string `json:"last_message_at"`
		Unread        int    `json:"unread"`
	}
	list := func(key int64) []conv {
		t.Helper()
		var out struct{ Conversations []conv }
		rec := e.do(key, http.MethodGet, "/direct", "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out.Conversations
	}
	got := list(bob)
	if len(got) != 1 || got[0].RoomID != room || got[0].Address != "alice" || got[0].Unread != 2 || got[0].LastMessageAt == "" {
		t.Errorf("bob: %+v", got)
	}
	if got := list(alice); len(got) != 1 || got[0].Address != "bob" || got[0].Unread != 0 {
		t.Errorf("alice: %+v", got)
	}
	if rec := e.do(bob, http.MethodGet, "/rooms/"+room+"/messages?limit=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	if got := list(bob); got[0].Unread != 1 {
		t.Errorf("after reading one: %+v", got)
	}
	e.do(bob, http.MethodGet, "/rooms/"+room+"/messages", "")
	e.do(e.admin, http.MethodGet, "/rooms/"+room+"/messages", "") // not a member: no read row
	if got := list(bob); got[0].Unread != 0 {
		t.Errorf("after reading all: %+v", got)
	}
	e.started(e.start(alice, "bob"))
	if got := list(bob); got[0].Unread != 1 {
		t.Errorf("after a new message: %+v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM direct_reads`); n != 1 {
		t.Errorf("%d read rows, want 1", n)
	}
	if got := list(e.bot("carol", "")); len(got) != 0 {
		t.Errorf("carol: %+v", got)
	}
}
