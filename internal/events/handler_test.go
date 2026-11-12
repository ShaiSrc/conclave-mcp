package events

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Taking read away while a stream is open ends it at the next message.
func TestHandle_StreamEndsWhenReadIsRevoked(t *testing.T) {
	ctx := context.Background()
	db, closeFn, err := storage.Open(ctx, filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeFn() })
	_, thisFile, _, _ := runtime.Caller(0)
	if _, err := storage.Migrate(ctx, db, os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")), slog.Default()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (1, 'agent', 'x', 'p1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('R', 'r', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('R', 1, 'reader', '2026-01-01T00:00:00Z', 'read')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	broker := NewBroker()
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: 1})))
		})
	})
	router.Get("/rooms/{id}/events", NewHandler(db, broker).Handle)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/rooms/R/events?wait=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	waitFor := func(line string) bool {
		for sc.Scan() {
			if sc.Text() == line {
				return true
			}
		}
		return false
	}
	if !waitFor(": ok") {
		t.Fatal("no preamble")
	}
	broker.Publish("R", []byte(`{"id":1}`))
	if !waitFor(`data: {"id":1}`) {
		t.Fatal("event not delivered while the key holds read")
	}
	if _, err := db.Exec(`UPDATE room_members SET rights = '' WHERE room_id = 'R' AND key_id = 1`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	broker.Publish("R", []byte(`{"id":2}`))
	if waitFor(`data: {"id":2}`) {
		t.Error("event delivered after read was taken away")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stream ended after %v, want at the next message (not at wait=10s)", took)
	}
}
