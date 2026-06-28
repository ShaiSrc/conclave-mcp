package rooms

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

// newServer returns a chi router that mounts the rooms service
// behind an auth-injection middleware. The middleware places the
// given key id into the request context as auth.Claims, so the
// service's auth.FromContext returns it.
func newServer(t *testing.T, db *sql.DB, callerID int64) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: callerID})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	NewService(db).Routes(r)
	return r
}

// setupAs creates a DB, seeds a key, and returns a server that
// authenticates as that key plus the DB handle and the key id.
func setupAs(t *testing.T) (*chi.Mux, *sql.DB, int64) {
	t.Helper()
	db := setupDB(t)
	id := seedKey(t, db, "k-"+t.Name())
	return newServer(t, db, id), db, id
}

func TestRooms_CreateReturnsIDAndAssignsOwner(t *testing.T) {
	r, db, caller := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1","description":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)
	if rv.Name != "r1" {
		t.Errorf("name: %q", rv.Name)
	}
	if rv.OwnerKeyID != caller {
		t.Errorf("owner: got %d, want %d", rv.OwnerKeyID, caller)
	}
	if rv.ID == "" {
		t.Error("id not set")
	}
	var role string
	if err := db.QueryRow("SELECT role FROM room_members WHERE room_id=? AND key_id=?", rv.ID, caller).Scan(&role); err != nil {
		t.Fatalf("owner member: %v", err)
	}
	if role != "owner" {
		t.Errorf("role: got %q, want owner", role)
	}
}

func TestRooms_CreateRejectsBadName(t *testing.T) {
	r, _, _ := setupAs(t)
	for _, name := range []string{"", strings.Repeat("x", 129)} {
		body := bytes.NewBufferString(`{"name":"` + name + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/rooms", body)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q: got %d, want 400", name, rec.Code)
		}
	}
}

func TestRooms_CreateDuplicateNameIs409(t *testing.T) {
	r, _, _ := setupAs(t)
	for i := 0; i < 2; i++ {
		body := bytes.NewBufferString(`{"name":"dup"}`)
		req := httptest.NewRequest(http.MethodPost, "/rooms", body)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first: %d", rec.Code)
		}
		if i == 1 && rec.Code != http.StatusConflict {
			t.Errorf("second: %d, want 409", rec.Code)
		}
	}
}

func TestRooms_ListShowsMemberRooms(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	// Owner sees the room.
	req = httptest.NewRequest(http.MethodGet, "/rooms", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), rv.ID) {
		t.Errorf("list missing room: %s", rec.Body.String())
	}

	// A fresh user with a fresh DB sees nothing.
	r2, _, _ := setupAs(t)
	req = httptest.NewRequest(http.MethodGet, "/rooms", nil)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	body2 := rec.Body.String()
	if !strings.Contains(body2, `"rooms":[]`) && !strings.Contains(body2, `"rooms":null`) {
		t.Errorf("non-member list should be empty, got: %s", body2)
	}
}

func TestRooms_GetReturns404ForNonMember(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	r2, _, _ := setupAs(t)
	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-member get: got %d, want 404", rec.Code)
	}
}

func TestRooms_ArchiveOwnerOnly(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	r2, _, _ := setupAs(t)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/archive", nil)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-owner archive: got %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/archive", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("owner archive: got %d, want 204", rec.Code)
	}
}

func TestRooms_InviteAndRemoveMember(t *testing.T) {
	r, db, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	// Seed a second key (guest) in the same DB; build a guest server
	// that shares the same DB.
	guest := seedKey(t, db, "guest")
	rGuest := newServer(t, db, guest)

	body = bytes.NewBufferString(`{"key_id":` + itoa(guest) + `,"role":"writer"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d; body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	rGuest.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("guest get: %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/rooms/"+rv.ID+"/members/"+itoa(guest), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("remove: %d, want 204", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	rGuest.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("guest get after remove: %d, want 404", rec.Code)
	}
}

func TestRooms_RemoveLastOwnerIs409(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	req = httptest.NewRequest(http.MethodDelete, "/rooms/"+rv.ID+"/members/"+itoa(rv.OwnerKeyID), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("remove last owner: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRooms_InviteRejectsBadRole(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	body = bytes.NewBufferString(`{"key_id":999,"role":"admin"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d, want 400", rec.Code)
	}
}

func TestRooms_InviteRejectsMissingKeyID(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	body = bytes.NewBufferString(`{"role":"reader"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing key_id: %d, want 400", rec.Code)
	}
}

func TestRooms_NoAuthClaimsReturns401(t *testing.T) {
	db := setupDB(t)
	r := chi.NewRouter()
	NewService(db).Routes(r)
	req := httptest.NewRequest(http.MethodPost, "/rooms", bytes.NewBufferString(`{"name":"r1"}`))
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
