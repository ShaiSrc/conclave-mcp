package messages

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	db, closeFn, err := storage.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if _, err := storage.Migrate(context.Background(), db, migrationsDir(), slog.Default()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func migrationsDir() fs.FS {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return os.DirFS(filepath.Join(repoRoot, "migrations"))
}

func seedKey(t *testing.T, db *sql.DB, label string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES (?, 'phc:fake', ?, ?)`,
		label, label+"-prefix", storage.FormatTS(time.Now()),
	)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedRoom(t *testing.T, db *sql.DB, ownerID int64, name string) string {
	t.Helper()
	id := "01J" + name + "ROOMTEST"
	if _, err := db.Exec(
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES (?, ?, ?, ?)`,
		id, name, ownerID, storage.FormatTS(time.Now()),
	); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'owner', ?, ?)`,
		id, ownerID, storage.FormatTS(time.Now()), rights.All().Storage(),
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func newServer(t *testing.T, db *sql.DB, callerID int64, scopes ...string) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: callerID, Scopes: scopes})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	NewService(db, 262144, nil).Routes(r)
	return r
}

func setupAs(t *testing.T) (*chi.Mux, *sql.DB, int64, string) {
	t.Helper()
	db := setupDB(t)
	id := seedKey(t, db, "k-"+t.Name())
	roomID := seedRoom(t, db, id, "r-"+t.Name())
	return newServer(t, db, id), db, id, roomID
}

func TestMessages_SendReturnsID(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	body := bytes.NewBufferString(`{"content":"hello","content_type":"text/plain"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var mv messageView
	_ = json.Unmarshal(rec.Body.Bytes(), &mv)
	if mv.RoomID != roomID {
		t.Errorf("room: %q", mv.RoomID)
	}
	if mv.ID == 0 {
		t.Error("id not set")
	}
}

func TestMessages_SendRejectsTooLarge(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	// 300 KiB content > 256 KiB limit
	big := strings.Repeat("x", 300*1024)
	body := bytes.NewBufferString(`{"content":"` + big + `","content_type":"text/plain"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("big content: got %d, want 413", rec.Code)
	}
}

func TestMessages_SendRejectsUnsupportedContentType(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	body := bytes.NewBufferString(`{"content":"x","content_type":"image/png"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad content_type: %d, want 400", rec.Code)
	}
}

func TestMessages_SendDefaultContentTypeIsTextPlain(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	body := bytes.NewBufferString(`{"content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var mv messageView
	_ = json.Unmarshal(rec.Body.Bytes(), &mv)
	if mv.ContentType != "text/plain" {
		t.Errorf("default content_type: %q", mv.ContentType)
	}
}

func TestMessages_ListReturnsMessagesInOrder(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	// Insert 3 messages directly (so we know the ids).
	for _, content := range []string{"a", "b", "c"} {
		if _, err := db.Exec(
			`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, ?, ?, ?, 'text/plain')`,
			roomID, 1, storage.FormatTS(time.Now()), content,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var resp struct {
		Messages  []messageView `json:"messages"`
		NextSince string        `json:"next_since"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Messages) != 3 {
		t.Errorf("count: got %d, want 3", len(resp.Messages))
	}
	for i := 1; i < len(resp.Messages); i++ {
		if resp.Messages[i-1].ID >= resp.Messages[i].ID {
			t.Errorf("not ascending: %d >= %d", resp.Messages[i-1].ID, resp.Messages[i].ID)
		}
	}
	if resp.NextSince != resp.Messages[2].TS {
		t.Errorf("next_since: got %q, want %q", resp.NextSince, resp.Messages[2].TS)
	}
}

func TestMessages_ListRespectsSince(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	// SPEC-0003 §4.2: `since` is an ISO-8601 timestamp. We
	// insert three messages with monotonically increasing
	// timestamps; passing the first ts as `since` should
	// return only the second and third.
	var tss []string
	for _, content := range []string{"a", "b", "c"} {
		ts := storage.FormatTS(time.Now())
		tss = append(tss, ts)
		_, err := db.Exec(
			`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, ?, ?, ?, 'text/plain')`,
			roomID, 1, ts, content,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // ensure monotonic ts
	}
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages?since="+tss[0], nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Messages) != 2 {
		t.Errorf("since filter: got %d, want 2", len(resp.Messages))
	}
}

func TestMessages_ListRespectsLimit(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	for i := 0; i < 10; i++ {
		_, _ = db.Exec(
			`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, 1, '2026-01-01T00:00:00.000Z', 'x', 'text/plain')`,
			roomID,
		)
	}
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages?limit=3", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":1`) {
		t.Errorf("expected first 3 messages, got: %s", rec.Body.String())
	}
}

func TestMessages_SendToArchivedRoomIs410(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	// Archive the room.
	_, _ = db.Exec(`UPDATE rooms SET archived_at = '2026-06-27T00:00:00.000Z' WHERE id = ?`, roomID)
	body := bytes.NewBufferString(`{"content":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Errorf("archived send: %d, want 410", rec.Code)
	}
}

func TestMessages_NonMemberSendIs404(t *testing.T) {
	r, db, owner, roomID := setupAs(t)
	// Seed a second key in the SAME db, but build a server
	// authenticated as that second key.
	other := seedKey(t, db, "other")
	rOther := newServer(t, db, other)
	_ = owner

	// The other user is not a member of roomID.
	body := bytes.NewBufferString(`{"content":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec := httptest.NewRecorder()
	rOther.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-member send: %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// And the owner still can.
	body = bytes.NewBufferString(`{"content":"hi"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("owner send: %d, want 201", rec.Code)
	}
}

func TestMessages_NoAuthClaimsIs401(t *testing.T) {
	db := setupDB(t)
	r := chi.NewRouter()
	NewService(db, 262144, nil).Routes(r)
	req := httptest.NewRequest(http.MethodGet, "/rooms/abc/messages", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: %d, want 401", rec.Code)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestMessages_SendTSIsFixedWidth(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", strings.NewReader(`{"content":"hi"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp struct {
		TS string `json:"ts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.TS) != len("2006-01-02T15:04:05.000000000Z") {
		t.Errorf("ts %q is not fixed width (status %d)", resp.TS, rec.Code)
	}
}

func TestMessages_ListSinceAcceptsAnyOffset(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	for _, ts := range []string{"2026-11-07T10:00:00.100000000Z", "2026-11-07T10:00:00.120000000Z"} {
		if _, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, 1, ?, 'x', 'text/plain')`, roomID, ts); err != nil {
			t.Fatal(err)
		}
	}
	since := url.QueryEscape("2026-11-07T11:00:00.1+01:00")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages?since="+since, nil))
	var resp struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Messages) != 1 || resp.Messages[0].TS != "2026-11-07T10:00:00.120000000Z" {
		t.Errorf("status %d, messages %+v", rec.Code, resp.Messages)
	}
}

func TestMessages_ListBadSinceIs400(t *testing.T) {
	r, _, _, roomID := setupAs(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages?since=m0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

func TestMessages_ReplyToIsCheckedAndKeptInMetadata(t *testing.T) {
	r, db, owner, roomID := setupAs(t)
	send := func(body string) (int, messageView) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", strings.NewReader(body)))
		var mv messageView
		_ = json.Unmarshal(rec.Body.Bytes(), &mv)
		return rec.Code, mv
	}
	code, q := send(`{"content":"review please","message_type":"task_assignment"}`)
	if code != http.StatusCreated {
		t.Fatalf("question: status %d", code)
	}
	code, a := send(`{"content":"done","message_type":"answer","reply_to":` + itoa(q.ID) + `,"metadata":{"mr":"!6"}}`)
	if code != http.StatusCreated {
		t.Fatalf("reply: status %d", code)
	}
	var meta map[string]any
	_ = json.Unmarshal(a.Metadata, &meta)
	if meta["reply_to"] != float64(q.ID) || meta["mr"] != "!6" {
		t.Errorf("reply metadata = %s, want reply_to %d merged with mr", a.Metadata, q.ID)
	}
	// list returns the stored metadata too
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages", nil))
	if !strings.Contains(rec.Body.String(), `"reply_to":`+itoa(q.ID)) {
		t.Errorf("list lost reply_to: %s", rec.Body.String())
	}

	other := seedRoom(t, db, owner, "other")
	_, _ = db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, ?, '2026-01-01T00:00:00.000000000Z', 'x', 'text/plain')`, other, owner)
	var foreign int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM messages WHERE room_id = ?`, other).Scan(&foreign)
	// metadata null with reply_to used to panic (nil map); big numbers
	// in metadata must survive the merge unrounded.
	if code, mv := send(`{"content":"x","reply_to":` + itoa(q.ID) + `,"metadata":null}`); code != http.StatusCreated || string(mv.Metadata) != `{"reply_to":`+itoa(q.ID)+`}` {
		t.Errorf("metadata null: status %d, metadata %s", code, mv.Metadata)
	}
	if code, mv := send(`{"content":"x","reply_to":` + itoa(q.ID) + `,"metadata":{"big":9007199254740993}}`); code != http.StatusCreated || !strings.Contains(string(mv.Metadata), "9007199254740993") {
		t.Errorf("big number: status %d, metadata %s", code, mv.Metadata)
	}
	_, gone := send(`{"content":"to be deleted"}`)
	if _, err := db.Exec(`UPDATE messages SET deleted_at = '2026-01-01T00:00:00.000000000Z' WHERE id = ?`, gone.ID); err != nil {
		t.Fatal(err)
	}
	// reply_to inside metadata gets the same check as the field.
	if code, _ := send(`{"content":"x","metadata":{"reply_to":999999}}`); code != http.StatusBadRequest {
		t.Errorf("metadata.reply_to 999999: status %d, want 400", code)
	}
	if code, _ := send(`{"content":"x","metadata":{"reply_to":"abc"}}`); code != http.StatusBadRequest {
		t.Errorf(`metadata.reply_to "abc": status %d, want 400`, code)
	}
	if code, mv := send(`{"content":"x","metadata":{"reply_to":` + itoa(q.ID) + `,"k":1}}`); code != http.StatusCreated || !strings.Contains(string(mv.Metadata), `"k":1`) {
		t.Errorf("valid metadata.reply_to: status %d, metadata %s", code, mv.Metadata)
	}
	for _, bad := range []string{"0", "-1", "999999", itoa(foreign), itoa(gone.ID)} {
		if code, _ := send(`{"content":"x","reply_to":` + bad + `}`); code != http.StatusBadRequest {
			t.Errorf("reply_to %s: status %d, want 400", bad, code)
		}
	}
}

func TestMessages_DeleteIsAdminOnly(t *testing.T) {
	r, db, owner, roomID := setupAs(t)
	if _, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, ?, ?, 'x', 'text/plain')`,
		roomID, owner, storage.FormatTS(time.Now())); err != nil {
		t.Fatal(err)
	}
	path := "/rooms/" + roomID + "/messages/1"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin key required") {
		t.Errorf("room owner (not admin): %d %s, want 403", rec.Code, rec.Body.String())
	}
	admin := newServer(t, db, seedKey(t, db, "admin"), auth.ScopeAdmin) // not a member
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("admin delete: %d %s, want 204", rec.Code, rec.Body.String())
	}
}

func TestMessages_RightsPerMessageType(t *testing.T) {
	_, db, _, roomID := setupAs(t)
	agent := seedKey(t, db, "agent")
	if _, err := db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, 'read,handoff')`,
		roomID, agent, storage.FormatTS(time.Now())); err != nil {
		t.Fatal(err)
	}
	r := newServer(t, db, agent)
	for _, c := range []struct {
		method, body string
		want         int
		missing      string
	}{
		{http.MethodPost, `{"content":"x","message_type":"handoff"}`, http.StatusCreated, ""},
		{http.MethodPost, `{"content":"x","message_type":"question"}`, http.StatusForbidden, "question"},
		{http.MethodPost, `{"content":"x"}`, http.StatusForbidden, "system"}, // untyped = system
		{http.MethodGet, ``, http.StatusOK, ""},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, "/rooms/"+roomID+"/messages", strings.NewReader(c.body)))
		if rec.Code != c.want || (c.missing != "" && !strings.Contains(rec.Body.String(), `"message":"missing right: `+c.missing+`"`)) {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.body, rec.Code, rec.Body.String(), c.want)
		}
	}
	if _, err := db.Exec(`UPDATE room_members SET rights = '' WHERE key_id = ?`, agent); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"missing_right"`) {
		t.Errorf("list without read: %d %s, want 403 missing_right", rec.Code, rec.Body.String())
	}
}

// sendAs posts content to roomID through r and fails the test on error.
func sendAs(t *testing.T, r http.Handler, roomID, content string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", bytes.NewBufferString(`{"content":"`+content+`"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("send %s: %d %s", content, rec.Code, rec.Body.String())
	}
}

// tsByID returns the room's timestamps in insert (id) order.
func tsByID(t *testing.T, db *sql.DB, roomID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT ts FROM messages WHERE room_id = ? ORDER BY id`, roomID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts string
		_ = rows.Scan(&ts)
		out = append(out, ts)
	}
	return out
}

func TestMessages_TimestampsFollowInsertOrder(t *testing.T) {
	// The first send stalls right after taking its timestamp while a
	// second send goes through. If stamping and inserting were not one
	// step, the second message would be stored first with the later
	// timestamp, and a reader whose `since` passed it would never see
	// the first.
	r, db, _, roomID := setupAs(t)
	release := make(chan struct{})
	var calls atomic.Int32
	old := clock
	clock = func() time.Time {
		now := time.Now()
		if calls.Add(1) == 1 {
			<-release
		}
		return now
	}
	t.Cleanup(func() { clock = old })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sendAs(t, r, roomID, "first") }()
	time.Sleep(50 * time.Millisecond)
	go func() { defer wg.Done(); sendAs(t, r, roomID, "second") }()
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	wg.Wait()
	ts := tsByID(t, db, roomID)
	if len(ts) != 2 || !(ts[0] < ts[1]) {
		t.Errorf("timestamps in insert order: %v, want strictly increasing", ts)
	}
}

func TestMessages_TimestampsAreUniqueWithinOneInstant(t *testing.T) {
	r, db, _, roomID := setupAs(t)
	fixed := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := clock
	clock = func() time.Time { return fixed }
	t.Cleanup(func() { clock = old })
	for _, c := range []string{"a", "b", "c"} {
		sendAs(t, r, roomID, c)
	}
	ts := tsByID(t, db, roomID)
	if len(ts) != 3 || !(ts[0] < ts[1] && ts[1] < ts[2]) {
		t.Errorf("timestamps: %v, want strictly increasing", ts)
	}
}

func TestMessages_TimestampsStayAboveStoredOnesAfterARestart(t *testing.T) {
	// A fresh Service (a restarted server) whose clock is behind the
	// newest stored message must still stamp after it.
	r, db, keyID, roomID := setupAs(t)
	const stored = "2099-01-01T00:00:00.000000000Z"
	if _, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, trust_origin)
		VALUES (?, ?, ?, 'from before the restart', 'text/plain', 'system', 'internal')`, roomID, keyID, stored); err != nil {
		t.Fatal(err)
	}
	sendAs(t, r, roomID, "after")
	if ts := tsByID(t, db, roomID); len(ts) != 2 || !(ts[1] > stored) {
		t.Errorf("timestamps: %v, want the new one after %s", ts, stored)
	}
}

func TestMessages_SendFailsWhenTheNewestTimestampCannotBeRead(t *testing.T) {
	// Without the stored high-water mark a lagging clock could stamp
	// before it; refuse the send instead of risking a skipped message.
	r, db, keyID, roomID := setupAs(t)
	if _, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, trust_origin)
		VALUES (?, ?, 'not a time', 'broken', 'text/plain', 'system', 'internal')`, roomID, keyID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", bytes.NewBufferString(`{"content":"x"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if n := len(tsByID(t, db, roomID)); n != 1 {
		t.Errorf("%d messages stored, want only the broken one", n)
	}
}

func TestMessages_OperatorMark(t *testing.T) {
	db := setupDB(t)
	id := seedKey(t, db, "op")
	roomID := seedRoom(t, db, id, "opmark")
	pub := &capBroker{}
	svc := NewService(db, 262144, pub)
	withClaims := func(scopes ...string) func(*http.Request) *http.Request {
		return func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: id, Scopes: scopes}))
		}
	}
	operatorCount := func() (n int) {
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE operator = 1`).Scan(&n)
		return
	}

	// SendAsOperator stores, returns and lists the mark, and audits it.
	req := withClaims(auth.ScopeAdmin)(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"content":"hi"}`)))
	rec := httptest.NewRecorder()
	svc.SendAsOperator(rec, req, roomID)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"operator":true`) {
		t.Fatalf("operator send: %d %s", rec.Code, rec.Body.String())
	}
	if operatorCount() != 1 {
		t.Fatalf("operator rows = %d, want 1", operatorCount())
	}
	if len(pub.payloads) != 1 || !strings.Contains(pub.payloads[0], `"operator":true`) {
		t.Errorf("live event lacks the mark: %q", pub.payloads)
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM audit WHERE action = 'message.send'`).Scan(&detail); err != nil || !strings.Contains(detail, `"operator":true`) {
		t.Errorf("audit detail %q, err %v", detail, err)
	}
	r := newServer(t, db, id, auth.ScopeAdmin)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/messages", nil))
	if !strings.Contains(rec.Body.String(), `"operator":true`) {
		t.Errorf("list lacks the mark: %s", rec.Body.String())
	}

	// A normal send cannot claim the mark.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages",
		strings.NewReader(`{"content":"x","operator":true,"metadata":{"operator":true}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("normal send: %d %s", rec.Code, rec.Body.String())
	}
	var mv messageView
	_ = json.Unmarshal(rec.Body.Bytes(), &mv)
	if mv.Operator || operatorCount() != 1 {
		t.Errorf("normal send got the mark: %+v, rows %d", mv, operatorCount())
	}

	// A UI session cannot post through the normal route.
	r = newServer(t, db, id, auth.ScopeAdmin, auth.ScopeUI)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/messages", strings.NewReader(`{"content":"nope"}`)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "operator messages need a passkey confirmation: send them from the web UI") {
		t.Errorf("session post: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 2 {
		t.Errorf("messages stored = %d, want 2", n)
	}
}

// capBroker records published payloads.
type capBroker struct{ payloads []string }

func (b *capBroker) Publish(_ string, payload []byte) int {
	b.payloads = append(b.payloads, string(payload))
	return 1
}

func TestMessages_DeleteOnReplicaIs403(t *testing.T) {
	_, db, owner, roomID := setupAs(t)
	if _, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES (?, ?, ?, 'x', 'text/plain')`,
		roomID, owner, storage.FormatTS(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE rooms SET home_domain = 'a.example' WHERE id = ?`, roomID); err != nil {
		t.Fatal(err)
	}
	admin := newServer(t, db, seedKey(t, db, "admin"), auth.ScopeAdmin)
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/rooms/"+roomID+"/messages/1", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"replica_room"`) {
		t.Errorf("delete on a replica: %d %s, want 403 replica_room", rec.Code, rec.Body.String())
	}
	var deleted sql.NullString
	if err := db.QueryRow(`SELECT deleted_at FROM messages WHERE id = 1`).Scan(&deleted); err != nil || deleted.Valid {
		t.Errorf("message deleted: %v %v", deleted, err)
	}
}
