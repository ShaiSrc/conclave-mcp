package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/internal/ui"
	"github.com/hermes/conclave-mcp/migrations"
)

// uiDB is a migrated database with an operator key and a UI session
// whose cookie value is "session-token".
func uiDB(t *testing.T) *sql.DB {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sum := sha256.Sum256([]byte("session-token"))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES ('operator', 'x', 'opprefix', '2026-11-12T10:00:00Z', 'internal', 1)`, nil},
		{`INSERT INTO meta (key, value) VALUES ('ui_operator_key_id', (SELECT CAST(id AS TEXT) FROM api_keys WHERE label = 'operator'))`, nil},
		{`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?)`,
			[]any{hex.EncodeToString(sum[:]), storage.FormatTS(now), storage.FormatTS(now), storage.FormatTS(now.Add(time.Hour))}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func uiRequest(method, path, host string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: ui.CookieName, Value: "session-token"})
	req.Header.Set(ui.HeaderName, "1")
	return req
}

// On a loopback address a UI session reaches /v1 as the operator key.
func TestServer_UISessionOnV1(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest("GET", "/v1/whoami", "localhost:7432"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"label":"operator"`) {
		t.Fatalf("whoami with the UI cookie: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, uiRequest("GET", "/ui/auth/state", "localhost:7432"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logged_in":true`) {
		t.Errorf("/ui/auth/state: %d %s", rec.Code, rec.Body)
	}
}

// On any other address there is no UI and the cookie counts for nothing.
func TestServer_NoUIOffLoopback(t *testing.T) {
	s, err := New(Config{Host: "0.0.0.0", Port: 7432}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/v1/whoami": http.StatusUnauthorized, "/ui/auth/state": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, uiRequest("GET", path, "localhost:7432"))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}
