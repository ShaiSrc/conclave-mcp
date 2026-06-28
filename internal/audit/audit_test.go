package audit

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
	"strconv"
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
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
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
	NewService(db).Routes(r)
	return r
}

func TestAudit_ListReturnsEntries(t *testing.T) {
	r, db, _ := setupAs(t)
	// Insert two audit rows.
	if err := Write(context.Background(), db, 1, "room.create", "room", "r1", "", "ok", ""); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := Write(context.Background(), db, 1, "message.send", "message", "m1", "", "ok", ""); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []entryView `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 2 {
		t.Errorf("count: got %d, want 2", len(resp.Entries))
	}
}

func TestAudit_ListFilterByAction(t *testing.T) {
	r, db, _ := setupAs(t)
	_ = Write(context.Background(), db, 1, "room.create", "room", "r1", "", "ok", "")
	_ = Write(context.Background(), db, 1, "message.send", "message", "m1", "", "ok", "")
	_ = Write(context.Background(), db, 1, "room.create", "room", "r2", "", "ok", "")

	req := httptest.NewRequest(http.MethodGet, "/audit?action=room.create", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp struct {
		Entries []entryView `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 2 {
		t.Errorf("action filter: got %d, want 2", len(resp.Entries))
	}
}

func TestAudit_ListFilterByActor(t *testing.T) {
	r, db, _ := setupAs(t)
	_ = Write(context.Background(), db, 1, "room.create", "room", "r1", "", "ok", "")
	_ = Write(context.Background(), db, 2, "room.create", "room", "r2", "", "ok", "")

	req := httptest.NewRequest(http.MethodGet, "/audit?actor=1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp struct {
		Entries []entryView `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 {
		t.Errorf("actor filter: got %d, want 1", len(resp.Entries))
	}
}

func TestAudit_ListFilterBySince(t *testing.T) {
	r, db, _ := setupAs(t)
	// Insert one old and one new entry.
	_, _ = db.Exec(`INSERT INTO audit(ts, action, target_type, target_id, result) VALUES ('2020-01-01T00:00:00.000Z', 'a', 'b', 'c', 'ok')`)
	_ = Write(context.Background(), db, 1, "a", "b", "c", "", "ok", "")

	req := httptest.NewRequest(http.MethodGet, "/audit?since=2025-01-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp struct {
		Entries []entryView `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 {
		t.Errorf("since filter: got %d, want 1", len(resp.Entries))
	}
}

func TestAudit_ListRejectsBadLimit(t *testing.T) {
	r, _, _ := setupAs(t)
	req := httptest.NewRequest(http.MethodGet, "/audit?limit=-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit: %d, want 400", rec.Code)
	}
}

func TestAudit_ListRespectsLimit(t *testing.T) {
	r, db, _ := setupAs(t)
	for i := 0; i < 10; i++ {
		_ = Write(context.Background(), db, 1, "a", "b", "c", "", "ok", "")
	}
	req := httptest.NewRequest(http.MethodGet, "/audit?limit=3", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var resp struct {
		Entries []entryView `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 3 {
		t.Errorf("limit: got %d, want 3", len(resp.Entries))
	}
}

func TestAudit_WriteRejectsBadResult(t *testing.T) {
	db := setupDB(t)
	err := Write(context.Background(), db, 1, "a", "b", "c", "", "weird", "")
	if err == nil {
		t.Error("expected error for non-ok/non-error result")
	}
}

func setupAs(t *testing.T) (*chi.Mux, *sql.DB, int64) {
	t.Helper()
	db := setupDB(t)
	id := seedKey(t, db, "k-"+t.Name())
	return newServer(t, db, id), db, id
}

// silence unused imports
var _ = bytes.NewBuffer
var _ = strconv.Itoa
