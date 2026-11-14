package follow

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

// env is a server with the follow, messages and rooms routes, an admin
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
	msgs.FedDomain = domain
	svc := &Service{DB: db, FedDomain: domain, MaxMessageBytes: msgs.MaxMessageBytes, Send: msgs.Send, Delete: msgs.Delete}
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
	rs := rooms.NewService(db)
	rs.FedDomain = domain
	rs.Routes(r)
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

// bot adds a bot with a profile row of follow policy, or none for "".
func (e *env) bot(handle, policy string) int64 {
	e.t.Helper()
	id := e.key(handle, handle, false)
	if policy != "" {
		e.exec(`INSERT INTO bot_profiles(key_id, updated_at, follow_policy) VALUES (?, '2026-01-01T00:00:00Z', ?)`, id, policy)
	}
	return id
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) do(key int64, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Key", strconv.FormatInt(key, 10))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

type follow struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Status    string `json:"status"`
	DecidedAt string `json:"decided_at"`
}

// follow has from follow to and returns the 201 answer.
func (e *env) follow(from int64, to string) follow {
	e.t.Helper()
	rec := e.do(from, http.MethodPost, "/follows", `{"to":"`+to+`"}`)
	var f follow
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &f) != nil || f.ID == 0 {
		e.t.Fatalf("follow %s: %d %s", to, rec.Code, rec.Body)
	}
	return f
}

// list answers GET path as key, the follows under name.
func (e *env) list(key int64, path, name string) []follow {
	e.t.Helper()
	rec := e.do(key, http.MethodGet, path, "")
	var out map[string][]follow
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out[name] == nil {
		e.t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	return out[name]
}

// post posts content as key with visibility ("" for the default) and
// returns the message id.
func (e *env) post(key int64, content, visibility string) int64 {
	e.t.Helper()
	body := `{"content":"` + content + `"}`
	if visibility != "" {
		body = `{"content":"` + content + `","visibility":"` + visibility + `"}`
	}
	rec := e.do(key, http.MethodPost, "/posts", body)
	var m struct {
		ID int64 `json:"id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m.ID == 0 {
		e.t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	return m.ID
}

// feed returns the active feed of key.
func (e *env) feed(key int64) string {
	e.t.Helper()
	var id string
	if err := e.db.QueryRow(`SELECT id FROM rooms WHERE owner_key_id = ? AND feed IS NOT NULL AND archived_at IS NULL`, key).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) member(room string, key int64) string {
	e.t.Helper()
	var r string
	if err := e.db.QueryRow(`SELECT rights FROM room_members WHERE room_id = ? AND key_id = ?`, room, key).Scan(&r); err != nil && err != sql.ErrNoRows {
		e.t.Fatal(err)
	}
	return r
}

func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Errorf("got %d %s; want %d %s", rec.Code, rec.Body, status, code)
	}
}

// The followed bot's policy decides: none refuses, approve waits, open
// accepts and adds the follower to the feed with read; a bot without a
// profile row has approve.
func TestFollow_PolicyMatrix(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	e.bot("none", "none")
	e.bot("approve", "approve")
	open := e.bot("open", "open")
	e.bot("norow", "")
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{"to":"none"}`), http.StatusForbidden, "follow_refused")
	for to, status := range map[string]string{"approve": "pending", "open": "accepted", "norow": "pending"} {
		if f := e.follow(alice, to); f.Status != status || f.To != to || f.From != "alice" {
			t.Errorf("%s: %+v, want %s", to, f, status)
		}
	}
	if got := e.member(e.feed(open), alice); got != "read" {
		t.Errorf("follower of open: rights %q, want read", got)
	}
	if got := e.member(e.feed(open), open); got != "read,status_update" {
		t.Errorf("owner: rights %q", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms WHERE feed IS NOT NULL`); n != 1 {
		t.Errorf("%d feeds, want 1 (only an accepted follow makes one)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'follow.refused' AND detail LIKE '%"reason":"policy"%'`); n != 1 {
		t.Errorf("%d policy refusals audited, want 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'follow.request' AND actor_key_id = ?`, alice); n != 3 {
		t.Errorf("%d requests audited, want 3", n)
	}
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{"to":"nobody"}`), http.StatusNotFound, "not_found")
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{"to":"alice"}`), http.StatusBadRequest, "bad_request")
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{"to":"bad name"}`), http.StatusBadRequest, "bad_request")
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{"to":"open@peer.example"}`), http.StatusNotFound, "federation_disabled")
	wantErr(t, e.do(alice, http.MethodPost, "/follows", `{`), http.StatusBadRequest, "bad_request")
}

// Only bots follow and are followed: admin keys and keys without a
// handle are refused as followers, admin keys as targets.
func TestFollow_OnlyBots(t *testing.T) {
	e := newEnv(t, "")
	bob := e.bot("bob", "open")
	plain := e.key("plain", "", false)
	wantErr(t, e.do(e.admin, http.MethodPost, "/follows", `{"to":"bob"}`), http.StatusForbidden, "forbidden")
	wantErr(t, e.do(plain, http.MethodPost, "/follows", `{"to":"bob"}`), http.StatusForbidden, "forbidden")
	e.exec(`UPDATE api_keys SET handle = 'boss' WHERE id = ?`, e.admin)
	wantErr(t, e.do(bob, http.MethodPost, "/follows", `{"to":"boss"}`), http.StatusForbidden, "follow_refused")
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'follow.refused' AND detail LIKE '%"reason":"not a bot"%'`); n != 3 {
		t.Errorf("%d not-a-bot refusals audited, want 3", n)
	}
	for _, path := range []string{"/follows", "/followers", "/follows/requests", "/timeline"} {
		wantErr(t, e.do(plain, http.MethodGet, path, ""), http.StatusForbidden, "forbidden")
	}
	wantErr(t, e.do(e.admin, http.MethodPost, "/posts", `{"content":"x"}`), http.StatusForbidden, "forbidden")
	if n := e.count(`SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("%d follows", n)
	}
}

// A pair has one follow: following again returns it, also after the
// target's policy narrowed, and a rejected request still reads as
// pending to the follower, while an admin sees it rejected.
func TestFollow_OnePerPair(t *testing.T) {
	e := newEnv(t, "x.example")
	alice, bob := e.bot("alice", ""), e.bot("bob", "")
	f := e.follow(alice, "bob")
	if f.To != "bob@x.example" || f.From != "alice@x.example" || f.Status != "pending" {
		t.Errorf("follow: %+v", f)
	}
	e.exec(`INSERT INTO bot_profiles(key_id, updated_at, follow_policy) VALUES (?, '2026-01-01T00:00:00Z', 'none')`, bob)
	if again := e.follow(alice, "BOB@X.Example"); again.ID != f.ID || again.Status != "pending" {
		t.Errorf("again: %+v, want %+v", again, f)
	}
	if rec := e.do(bob, http.MethodPost, fmt.Sprintf("/follows/requests/%d/reject", f.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	if again := e.follow(alice, "bob"); again.ID != f.ID || again.Status != "pending" || again.DecidedAt != "" {
		t.Errorf("after reject: %+v", again)
	}
	if got := e.list(alice, "/follows", "follows"); len(got) != 1 || got[0].Status != "pending" || got[0].DecidedAt != "" {
		t.Errorf("alice's follows: %+v", got)
	}
	if got := e.list(e.admin, "/follows?bot=alice", "follows"); len(got) != 1 || got[0].Status != "rejected" {
		t.Errorf("admin's view: %+v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM follows`); n != 1 {
		t.Errorf("%d follows", n)
	}
}

// The followed bot or an admin accepts or rejects a pending request;
// the follower unfollows; the followed bot or an admin removes a
// follower. Each takes the follower in or out of the feed.
func TestFollow_AcceptRejectUnfollowRemove(t *testing.T) {
	e := newEnv(t, "")
	alice, bob, carol, dave := e.bot("alice", ""), e.bot("bob", ""), e.bot("carol", ""), e.bot("dave", "")
	fa, fc, fd := e.follow(alice, "bob"), e.follow(carol, "bob"), e.follow(dave, "bob")
	e.follow(alice, "carol")

	if got := e.list(bob, "/follows/requests", "requests"); len(got) != 3 || got[0].From != "alice" || got[0].To != "bob" {
		t.Errorf("bob's requests: %+v", got)
	}
	if got := e.list(e.admin, "/follows/requests", "requests"); len(got) != 4 {
		t.Errorf("all requests: %+v", got)
	}
	if got := e.list(e.admin, "/follows/requests?bot=carol", "requests"); len(got) != 1 || got[0].From != "alice" {
		t.Errorf("carol's requests: %+v", got)
	}
	wantErr(t, e.do(e.admin, http.MethodGet, "/follows?bot=nobody", ""), http.StatusNotFound, "not_found")
	accept := func(key, id int64) *httptest.ResponseRecorder {
		return e.do(key, http.MethodPost, fmt.Sprintf("/follows/requests/%d/accept", id), "")
	}
	wantErr(t, accept(carol, fa.ID), http.StatusNotFound, "not_found") // not carol's
	wantErr(t, accept(alice, fa.ID), http.StatusNotFound, "not_found") // the follower cannot
	if rec := accept(bob, fa.ID); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"accepted"`) {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, accept(bob, fa.ID), http.StatusNotFound, "not_found") // no longer pending
	if rec := accept(e.admin, fc.ID); rec.Code != http.StatusOK {
		t.Fatalf("admin accept: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(bob, http.MethodPost, fmt.Sprintf("/follows/requests/%d/reject", fd.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	feed := e.feed(bob)
	if e.member(feed, alice) != "read" || e.member(feed, carol) != "read" || e.member(feed, dave) != "" {
		t.Errorf("feed members: alice %q carol %q dave %q", e.member(feed, alice), e.member(feed, carol), e.member(feed, dave))
	}
	if got := e.list(bob, "/followers", "followers"); len(got) != 2 || got[0].From != "alice" || got[1].From != "carol" {
		t.Errorf("bob's followers: %+v", got)
	}
	if got := e.list(e.admin, "/followers", "followers"); len(got) != 2 {
		t.Errorf("all followers: %+v", got)
	}
	if got := e.list(alice, "/follows", "follows"); len(got) != 2 || got[0].Status != "accepted" || got[1].Status != "pending" {
		t.Errorf("alice's follows: %+v", got)
	}

	// Unfollowing leaves the feed; a second time there is nothing.
	if rec := e.do(alice, http.MethodDelete, "/follows/bob", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unfollow: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, e.do(alice, http.MethodDelete, "/follows/bob", ""), http.StatusNotFound, "not_found")
	if e.member(feed, alice) != "" {
		t.Error("alice is still in bob's feed")
	}
	// A pending or rejected follow goes too.
	if rec := e.do(dave, http.MethodDelete, "/follows/bob", ""); rec.Code != http.StatusNoContent {
		t.Errorf("unfollow after reject: %d %s", rec.Code, rec.Body)
	}
	if f := e.follow(dave, "bob"); f.ID == fd.ID || f.Status != "pending" {
		t.Errorf("new request after unfollow: %+v", f)
	}

	// The followed bot removes a follower; an admin names the bot.
	wantErr(t, e.do(alice, http.MethodDelete, "/followers/carol", ""), http.StatusNotFound, "not_found")
	if rec := e.do(bob, http.MethodDelete, "/followers/carol", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if e.member(feed, carol) != "" {
		t.Error("carol is still in bob's feed")
	}
	e.follow(carol, "bob")
	wantErr(t, e.do(e.admin, http.MethodDelete, "/followers/carol", ""), http.StatusBadRequest, "bad_request")
	if rec := e.do(e.admin, http.MethodDelete, "/followers/carol?bot=bob", ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin remove: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(`SELECT COUNT(*) FROM follows WHERE follower_key_id = ? AND target_key_id = ?`, carol, bob); n != 0 {
		t.Errorf("carol's follow of bob is left")
	}
	for action, want := range map[string]int{"follow.accept": 2, "follow.reject": 1, "follow.remove": 4} {
		if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = ?`, action); n != want {
			t.Errorf("%d %s audited, want %d", n, action, want)
		}
	}
}

// Posts go to the bot's feed, made on its first post: a status_update
// with the visibility in its metadata, followers by default. The
// message is checked before anything is made.
func TestPost(t *testing.T) {
	e := newEnv(t, "x.example")
	alice := e.bot("alice", "")
	for body, status := range map[string]int{
		`{"content":""}`: http.StatusBadRequest,
		`{"content":"x","visibility":"everyone"}`:         http.StatusBadRequest,
		`{"content":"` + strings.Repeat("x", 1025) + `"}`: http.StatusRequestEntityTooLarge,
		`{"content":"` + strings.Repeat("x", 3000) + `"}`: http.StatusRequestEntityTooLarge,
		`{"content":`: http.StatusBadRequest,
		`{"content":"x","visibility":"followers","x":"y"}}`: http.StatusBadRequest,
	} {
		if rec := e.do(alice, http.MethodPost, "/posts", body); rec.Code != status {
			t.Errorf("%.40s: %d %s, want %d", body, rec.Code, rec.Body, status)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms after refused posts", n)
	}
	// A room squatting the feed's name does not stop it.
	e.exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('squat', 'feed:alice@x.example', ?, '2026-01-01T00:00:00Z')`, e.admin)
	first := e.post(alice, "hello", "")
	second := e.post(alice, "world", "public")
	feed := e.feed(alice)
	var name, owner, f string
	if err := e.db.QueryRow(`SELECT name, owner_key_id, feed FROM rooms WHERE id = ?`, feed).Scan(&name, &owner, &f); err != nil ||
		name != "feed:alice@x.example#"+feed || owner != strconv.FormatInt(alice, 10) || f != "alice@x.example" {
		t.Errorf("feed: %q %s %q %v", name, owner, f, err)
	}
	for id, vis := range map[int64]string{first: "followers", second: "public"} {
		var typ, meta string
		if err := e.db.QueryRow(`SELECT message_type, metadata FROM messages WHERE id = ? AND room_id = ?`, id, feed).Scan(&typ, &meta); err != nil ||
			typ != "status_update" || meta != `{"visibility":"`+vis+`"}` {
			t.Errorf("post %d: %s %s %v", id, typ, meta, err)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM rooms WHERE feed IS NOT NULL`); n != 1 {
		t.Errorf("%d feeds, want 1", n)
	}
}

// Followers hold read only: the send path refuses their posts in the
// feed.
func TestPost_FollowersCannotPost(t *testing.T) {
	e := newEnv(t, "")
	alice, bob := e.bot("alice", ""), e.bot("bob", "open")
	e.follow(alice, "bob")
	feed := e.feed(bob)
	wantErr(t, e.do(alice, http.MethodPost, "/rooms/"+feed+"/messages", `{"content":"x","message_type":"status_update"}`),
		http.StatusForbidden, "missing_right")
	wantErr(t, e.do(bob, http.MethodPost, "/rooms/"+feed+"/messages", `{"content":"x","message_type":"question"}`),
		http.StatusForbidden, "missing_right")
	// A follower's own post goes to its own feed.
	id := e.post(alice, "mine", "")
	if n := e.count(`SELECT COUNT(*) FROM messages WHERE id = ? AND room_id = ?`, id, e.feed(alice)); n != 1 {
		t.Error("alice's post is not in its own feed")
	}
}

// The author deletes its post through the message delete; another
// bot, or a message outside the author's feed, is 404.
func TestPost_Delete(t *testing.T) {
	e := newEnv(t, "")
	alice, bob := e.bot("alice", ""), e.bot("bob", "open")
	e.follow(alice, "bob")
	id := e.post(bob, "oops", "")
	wantErr(t, e.do(alice, http.MethodDelete, fmt.Sprintf("/posts/%d", id), ""), http.StatusNotFound, "not_found")
	wantErr(t, e.do(bob, http.MethodDelete, "/posts/x", ""), http.StatusBadRequest, "bad_request")
	if rec := e.do(bob, http.MethodDelete, fmt.Sprintf("/posts/%d", id), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, e.do(bob, http.MethodDelete, fmt.Sprintf("/posts/%d", id), ""), http.StatusNotFound, "not_found")
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'message.delete' AND actor_key_id = ?`, bob); n != 1 {
		t.Errorf("%d deletes audited, want 1", n)
	}
	// A message in a room that is not a feed is no post.
	rec := e.do(e.admin, http.MethodPost, "/rooms", `{"name":"proj"}`)
	var room struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &room)
	e.do(e.admin, http.MethodPost, "/rooms/"+room.ID+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["all"]}`, bob))
	var msg struct{ ID int64 }
	_ = json.Unmarshal(e.do(bob, http.MethodPost, "/rooms/"+room.ID+"/messages", `{"content":"x","message_type":"question"}`).Body.Bytes(), &msg)
	wantErr(t, e.do(bob, http.MethodDelete, fmt.Sprintf("/posts/%d", msg.ID), ""), http.StatusNotFound, "not_found")
}

// The timeline holds the posts of the feeds the caller is a member of,
// its own and those it follows, in (ts, id) order after since, with
// next_since; deleted posts and feeds it does not follow are left out.
func TestTimeline(t *testing.T) {
	e := newEnv(t, "")
	alice, bob, carol := e.bot("alice", ""), e.bot("bob", "open"), e.bot("carol", "")
	e.follow(alice, "bob")
	e.follow(alice, "carol") // pending: carol's posts stay out
	ids := []int64{e.post(alice, "a1", ""), e.post(bob, "b1", "public"), e.post(carol, "c1", "")}
	ids = append(ids, e.post(bob, "b2", ""), e.post(alice, "a2", ""))
	gone := e.post(bob, "b3", "")
	if rec := e.do(bob, http.MethodDelete, fmt.Sprintf("/posts/%d", gone), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	type item struct {
		ID         int64  `json:"id"`
		RoomID     string `json:"room_id"`
		Author     string `json:"author_address"`
		Content    string `json:"content"`
		Visibility string `json:"visibility"`
		TS         string `json:"ts"`
	}
	page := func(query string) ([]item, string) {
		t.Helper()
		rec := e.do(alice, http.MethodGet, "/timeline"+query, "")
		var out struct {
			Posts     []item `json:"posts"`
			NextSince string `json:"next_since"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Posts == nil {
			t.Fatalf("timeline%s: %d %s", query, rec.Code, rec.Body)
		}
		return out.Posts, out.NextSince
	}
	all, next := page("")
	var got []string
	for _, p := range all {
		got = append(got, p.Author+":"+p.Content+":"+p.Visibility)
	}
	if want := "alice:a1:followers bob:b1:public bob:b2:followers alice:a2:followers"; strings.Join(got, " ") != want {
		t.Errorf("timeline: %s, want %s", strings.Join(got, " "), want)
	}
	if all[1].RoomID != e.feed(bob) || all[0].RoomID != e.feed(alice) || next != all[3].TS {
		t.Errorf("room ids or next_since: %+v %s", all, next)
	}
	first, next := page("?limit=2")
	rest, last := page("?limit=2&since=" + next)
	if len(first) != 2 || len(rest) != 2 || first[1].ID != ids[1] || rest[0].ID != ids[3] || last != all[3].TS {
		t.Errorf("pages: %+v %+v", first, rest)
	}
	if empty, next := page("?since=" + last); len(empty) != 0 || next != "" {
		t.Errorf("after the last: %+v %q", empty, next)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?since=yesterday"} {
		wantErr(t, e.do(alice, http.MethodGet, "/timeline"+q, ""), http.StatusBadRequest, "bad_request")
	}
	if posts, _ := page("?limit=100"); len(posts) != 4 {
		t.Errorf("limit=100: %d posts", len(posts))
	}
	// After unfollowing, bob's posts leave alice's timeline.
	e.do(alice, http.MethodDelete, "/follows/bob", "")
	if posts, _ := page(""); len(posts) != 2 {
		t.Errorf("after unfollow: %+v", posts)
	}
}

// An admin key reads the posts of every active feed hosted here, or a
// bot's timeline with ?bot=; ?order=desc puts the newest first, as one
// page without next_since.
func TestTimeline_AdminAndOrder(t *testing.T) {
	e := newEnv(t, "")
	alice, bob, carol := e.bot("alice", ""), e.bot("bob", "open"), e.bot("carol", "")
	e.follow(alice, "bob")
	e.post(alice, "a1", "")
	e.post(bob, "b1", "public")
	e.post(carol, "c1", "")
	e.post(alice, "a2", "")
	page := func(key int64, query string) (string, string) {
		t.Helper()
		rec := e.do(key, http.MethodGet, "/timeline"+query, "")
		var out struct {
			Posts []struct {
				Author  string `json:"author_address"`
				Content string `json:"content"`
			} `json:"posts"`
			NextSince string `json:"next_since"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Posts == nil {
			t.Fatalf("timeline%s: %d %s", query, rec.Code, rec.Body)
		}
		var got []string
		for _, p := range out.Posts {
			got = append(got, p.Author+":"+p.Content)
		}
		return strings.Join(got, " "), out.NextSince
	}
	for _, c := range []struct {
		key          int64
		query, want  string
		wantNextNone bool
	}{
		{e.admin, "", "alice:a1 bob:b1 carol:c1 alice:a2", false},
		{e.admin, "?order=desc", "alice:a2 carol:c1 bob:b1 alice:a1", true},
		{e.admin, "?order=desc&limit=2", "alice:a2 carol:c1", true},
		{e.admin, "?order=asc&limit=2", "alice:a1 bob:b1", false},
		{e.admin, "?bot=alice&order=desc", "alice:a2 bob:b1 alice:a1", true},
		{alice, "?order=desc", "alice:a2 bob:b1 alice:a1", true},
		{alice, "?bot=carol", "alice:a1 bob:b1 alice:a2", false}, // a bot's ?bot= counts for nothing
	} {
		got, next := page(c.key, c.query)
		if got != c.want || (next == "") != c.wantNextNone {
			t.Errorf("%d %s: %s next %q, want %s", c.key, c.query, got, next, c.want)
		}
	}
	wantErr(t, e.do(e.admin, http.MethodGet, "/timeline?bot=nobody", ""), http.StatusNotFound, "not_found")
	wantErr(t, e.do(e.admin, http.MethodGet, "/timeline?order=up", ""), http.StatusBadRequest, "bad_request")
	// A replica of a feed hosted elsewhere is not this server's to moderate.
	e.exec(`UPDATE rooms SET home_domain = 'peer.example' WHERE id = ?`, e.feed(carol))
	if got, _ := page(e.admin, ""); got != "alice:a1 bob:b1 alice:a2" {
		t.Errorf("admin with a replica: %s", got)
	}
}

// Feeds take no invites, rights changes or new members through the room
// API; an admin may remove a follower, which ends its follow, but not
// the owner. GET /rooms lists them as kind feed, which ?kind= filters.
func TestRooms_FeedRefusals(t *testing.T) {
	e := newEnv(t, "")
	alice, bob, carol := e.bot("alice", ""), e.bot("bob", "open"), e.bot("carol", "")
	e.follow(alice, "bob")
	feed := e.feed(bob)
	base := "/rooms/" + feed
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, carol)},
		{http.MethodPost, base + "/invite", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, carol)},
		{http.MethodPost, base + "/members", `{"address":"c@peer.example","rights":["read"]}`},
		{http.MethodPatch, fmt.Sprintf("%s/members/%d", base, alice), `{"rights":["read","status_update"]}`},
	} {
		wantErr(t, e.do(e.admin, c.method, c.path, c.body), http.StatusForbidden, "feed_room")
	}
	body := e.do(alice, http.MethodGet, "/rooms?kind=feed", "").Body.String()
	if !strings.Contains(body, `"id":"`+feed+`"`) || !strings.Contains(body, `"kind":"feed"`) || !strings.Contains(body, `"feed":"bob"`) {
		t.Errorf("rooms?kind=feed: %s", body)
	}
	if body := e.do(alice, http.MethodGet, "/rooms?kind=room", "").Body.String(); strings.Contains(body, feed) {
		t.Errorf("rooms?kind=room: %s", body)
	}
	if body := e.do(bob, http.MethodGet, base, "").Body.String(); !strings.Contains(body, `"kind":"feed"`) || !strings.Contains(body, `"feed":"bob"`) {
		t.Errorf("rooms/{id}: %s", body)
	}
	// A bot with a feed is not "in another room" for an invite.
	rec := e.do(e.admin, http.MethodPost, "/rooms", `{"name":"proj"}`)
	var nr struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &nr)
	if rec := e.do(e.admin, http.MethodPost, "/rooms/"+nr.ID+"/members", fmt.Sprintf(`{"key_id":%d,"rights":["read"]}`, bob)); rec.Code != http.StatusCreated {
		t.Errorf("invite of a bot with a feed: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, e.do(e.admin, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, bob), ""), http.StatusConflict, "conflict")
	if rec := e.do(e.admin, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, alice), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(`SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("%d follows left after the member removal", n)
	}
	// Archiving a feed ends its accepted follows; the next post makes a
	// new feed.
	e.follow(alice, "bob")
	if rec := e.do(e.admin, http.MethodPost, base+"/archive", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}
	if n := e.count(`SELECT COUNT(*) FROM follows`); n != 0 {
		t.Errorf("%d follows left after the archive", n)
	}
	e.post(bob, "again", "")
	if e.feed(bob) == feed {
		t.Error("a post went to the archived feed")
	}
}

// The feed: prefix belongs to feeds: no room is made with it.
func TestRooms_FeedPrefixReserved(t *testing.T) {
	e := newEnv(t, "")
	for _, name := range []string{"feed:alice", "feed:"} {
		wantErr(t, e.do(e.admin, http.MethodPost, "/rooms", `{"name":"`+name+`"}`), http.StatusBadRequest, "bad_request")
	}
	if rec := e.do(e.admin, http.MethodPost, "/rooms", `{"name":"feeds"}`); rec.Code != http.StatusCreated {
		t.Errorf("feeds: %d %s", rec.Code, rec.Body)
	}
}

// 30 new follows an hour per sender; the 31st gets 429, while an
// existing follow is still answered.
func TestFollow_RateLimit(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	for i := range 31 {
		e.bot(fmt.Sprintf("b%d", i), "")
	}
	for i := range 30 {
		e.follow(alice, fmt.Sprintf("b%d", i))
	}
	rec := e.do(alice, http.MethodPost, "/follows", `{"to":"b30"}`)
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	e.follow(alice, "b0")
	if n := e.count(`SELECT COUNT(*) FROM audit WHERE action = 'follow.refused' AND detail LIKE '%"reason":"limit"%'`); n != 1 {
		t.Errorf("%d limit refusals audited, want 1", n)
	}
	e.follow(e.bot("other", ""), "b30") // the limit is per sender
}

// 60 posts an hour per bot; the 61st gets 429.
func TestPost_RateLimit(t *testing.T) {
	e := newEnv(t, "")
	alice := e.bot("alice", "")
	for range 60 {
		e.post(alice, "x", "")
	}
	rec := e.do(alice, http.MethodPost, "/posts", `{"content":"x"}`)
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	e.post(e.bot("bob", ""), "x", "") // per bot
}
