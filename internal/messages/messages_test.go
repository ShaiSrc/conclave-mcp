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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
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
		label, label+"-prefix", time.Now().UTC().Format(time.RFC3339Nano),
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
		id, name, ownerID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES (?, ?, 'owner', ?)`,
		id, ownerID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func newServer(t *testing.T, db *sql.DB, callerID int64) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: callerID})
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
			roomID, 1, time.Now().UTC().Format(time.RFC3339Nano), content,
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
		ts := time.Now().UTC().Format(time.RFC3339Nano)
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
