package keys

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// setupDB opens a fresh DB, runs migrations, returns it.
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

// migrationsDir returns an fs.FS rooted at the repo's migrations/
// directory. It mirrors the helper in internal/storage but is
// duplicated here to keep the keys package self-contained.
func migrationsDir() fs.FS {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return os.DirFS(filepath.Join(repoRoot, "migrations"))
}

// keep import of strconv in case other tests need it
var _ = strconv.Itoa

// newTestServer wires chi with the keys routes, called as an
// admin key.
func newTestServer(t *testing.T) (*chi.Mux, *sql.DB) {
	t.Helper()
	return newTestServerAs(t, []string{ScopeAdmin})
}

// newTestServerAs wires chi with the keys routes; every request
// carries claims with the given scopes.
func newTestServerAs(t *testing.T, scopes []string) (*chi.Mux, *sql.DB) {
	t.Helper()
	db := setupDB(t)
	s := NewService(db)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: 1, Scopes: scopes})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	s.Routes(r)
	return r, db
}

func TestKeys_NonAdminCannotCreateOrRevoke(t *testing.T) {
	r, _ := newTestServerAs(t, nil)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(`{"label":"x"}`)),
		httptest.NewRequest(http.MethodDelete, "/keys/1", nil),
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: got %d, want 403", req.Method, req.URL.Path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /keys: got %d, want 200 (list stays open so agents can find each other)", rec.Code)
	}
}

func TestKeys_CreateReturnsPlaintextOnce(t *testing.T) {
	r, _ := newTestServer(t)
	body := bytes.NewBufferString(`{"label":"alice"}`)
	req := httptest.NewRequest(http.MethodPost, "/keys", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(resp.Key, "tk_") {
		t.Errorf("key: missing tk_ prefix: %q", resp.Key)
	}
	if resp.ID == 0 {
		t.Errorf("id not set")
	}
	if resp.Label != "alice" {
		t.Errorf("label: got %q", resp.Label)
	}
}

func TestKeys_CreateRejectsEmptyLabel(t *testing.T) {
	r, _ := newTestServer(t)
	body := bytes.NewBufferString(`{"label":""}`)
	req := httptest.NewRequest(http.MethodPost, "/keys", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestKeys_CreateRejectsMalformedJSON(t *testing.T) {
	r, _ := newTestServer(t)
	body := bytes.NewBufferString(`{not json`)
	req := httptest.NewRequest(http.MethodPost, "/keys", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestKeys_CreateDuplicateLabelIs409(t *testing.T) {
	r, _ := newTestServer(t)
	for i := 0; i < 2; i++ {
		body := bytes.NewBufferString(`{"label":"same"}`)
		req := httptest.NewRequest(http.MethodPost, "/keys", body)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first create: got %d, want 201", rec.Code)
		}
		if i == 1 && rec.Code != http.StatusConflict {
			t.Errorf("second create: got %d, want 409", rec.Code)
		}
	}
}

func TestKeys_ListReturnsKeysWithoutPlaintext(t *testing.T) {
	r, db := newTestServer(t)
	// Insert two keys directly so we don't depend on create.
	if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('a', 'phc:fake1', 'prefix01', '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('b', 'phc:fake2', 'prefix02', '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/keys", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "phc:fake") {
		t.Errorf("list leaked hash: %s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `"key":`) || strings.Contains(line, `"hash":`) {
			t.Errorf("list response contains secret field: %s", line)
		}
	}
	// The real prefixes are exposed as "tk_<8chars>".
	if !strings.Contains(body, "tk_prefix01") {
		t.Errorf("list should expose real prefix tk_prefix01, got: %s", body)
	}
	if !strings.Contains(body, "tk_prefix02") {
		t.Errorf("list should expose real prefix tk_prefix02, got: %s", body)
	}
}

func TestKeys_RevokeSoftDeletes(t *testing.T) {
	r, db := newTestServer(t)
	res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('r1', 'phc:fake', 'revokeme', '2026-01-01T00:00:00.000Z')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	req := httptest.NewRequest(http.MethodDelete, "/keys/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("revoke: got %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// Row still exists but revoked_at is set.
	var revokedAt sql.NullString
	if err := db.QueryRow("SELECT revoked_at FROM api_keys WHERE id=?", id).Scan(&revokedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !revokedAt.Valid {
		t.Error("revoked_at not set after revoke")
	}

	// Second revoke -> 404 (idempotent: already-revoked treated as gone)
	req2 := httptest.NewRequest(http.MethodDelete, "/keys/"+itoa(id), nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("re-revoke: got %d, want 404", rec2.Code)
	}
}

func TestKeys_RevokeRejectsBadID(t *testing.T) {
	r, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/keys/not-an-int", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestKeys_AuthIntegration_VerifyAfterCreate(t *testing.T) {
	// End-to-end: create a key, then verify the returned plaintext
	// against the stored hash via auth.VerifyPHC, AND confirm
	// auth.Verifier can look up the key by its prefix.
	r, db := newTestServer(t)

	body := bytes.NewBufferString(`{"label":"e2e"}`)
	req := httptest.NewRequest(http.MethodPost, "/keys", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	// 1) PHC roundtrip
	var phc string
	if err := db.QueryRow("SELECT hash FROM api_keys WHERE id=?", resp.ID).Scan(&phc); err != nil {
		t.Fatalf("lookup hash: %v", err)
	}
	ok, err := auth.VerifyPHC(resp.Key, phc)
	if err != nil {
		t.Fatalf("VerifyPHC: %v", err)
	}
	if !ok {
		t.Error("VerifyPHC returned false for the just-created key")
	}

	// 2) Prefix lookup via Service.LookupByPrefix
	svc := NewService(db)
	// The lookup prefix is the 8 chars after "tk_".
	prefix := resp.Key[3:11]
	id, _, phc2, err := svc.LookupByPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatalf("LookupByPrefix: %v", err)
	}
	if id != resp.ID {
		t.Errorf("id: got %d, want %d", id, resp.ID)
	}
	if phc2 != phc {
		t.Errorf("phc mismatch on lookup")
	}

	// 3) Full Verifier integration: build a verifier, verify the bearer.
	verifier := auth.NewVerifier(svc, 1)
	gotID, _, err := verifier.Verify(context.Background(), "Bearer "+resp.Key)
	if err != nil {
		t.Fatalf("Verifier: %v", err)
	}
	if gotID != resp.ID {
		t.Errorf("Verifier id: got %d, want %d", gotID, resp.ID)
	}
}

// itoa avoids pulling in strconv just for these tests.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// silence unused imports
var _ = io.EOF
