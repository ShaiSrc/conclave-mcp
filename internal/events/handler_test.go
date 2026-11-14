package events

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// streamServer serves the handler for room R, where keys 1 and 2 hold
// read; the X-Key header names the caller's key (default 1).
func streamServer(t *testing.T) (*sql.DB, *Broker, *Handler, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	db, closeFn, err := storage.Open(ctx, filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeFn() })
	_, thisFile, _, _ := runtime.Caller(0)
	if _, err := storage.Migrate(ctx, db, os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (1, 'agent', 'x', 'p1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (2, 'other', 'x', 'p2', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('R', 'r', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('R', 1, 'reader', '2026-01-01T00:00:00Z', 'read')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('R', 2, 'reader', '2026-01-01T00:00:00Z', 'read')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	broker := NewBroker()
	h := NewHandler(db, broker)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, _ := strconv.ParseInt(req.Header.Get("X-Key"), 10, 64)
			if id == 0 {
				id = 1
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: id})))
		})
	})
	router.Get("/rooms/{id}/events", h.Handle)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return db, broker, h, srv
}

// open opens a stream as key and waits for the preamble.
func open(t *testing.T, srv *httptest.Server, key int64) (*http.Response, *bufio.Scanner) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/rooms/R/events?wait=30", nil)
	req.Header.Set("X-Key", strconv.FormatInt(key, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	sc := bufio.NewScanner(resp.Body)
	if resp.StatusCode == http.StatusOK && (!sc.Scan() || sc.Text() != ": ok") {
		t.Fatal("no preamble")
	}
	return resp, sc
}

// Taking read away (or revoking the key) while a stream is open ends it
// within the recheck interval, with no message sent meanwhile.
func TestHandle_StreamEndsWhenReadIsTaken(t *testing.T) {
	for name, revoke := range map[string]string{
		"read revoked": `UPDATE room_members SET rights = '' WHERE room_id = 'R' AND key_id = 1`,
		"key revoked":  `UPDATE api_keys SET revoked_at = '2026-01-02T00:00:00Z' WHERE id = 1`,
	} {
		t.Run(name, func(t *testing.T) {
			db, broker, h, srv := streamServer(t)
			h.Recheck = 100 * time.Millisecond
			_, sc := open(t, srv, 1)
			broker.Publish("R", []byte(`{"id":1}`))
			for sc.Scan() && sc.Text() != `data: {"id":1}` {
			}
			if _, err := db.Exec(revoke); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: {\"id\"") {
					t.Errorf("message after revocation: %s", sc.Text())
				}
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("stream ended after %v, want within the recheck interval (not at wait=30s)", took)
			}
		})
	}
	if h := NewHandler(nil, nil); h.Recheck != 5*time.Second || h.MaxStreams != 16 || h.WriteTimeout != 10*time.Second {
		t.Errorf("defaults: recheck %v, max streams %d, write timeout %v", h.Recheck, h.MaxStreams, h.WriteTimeout)
	}
}

// A key holds at most MaxStreams open streams: one more answers 429 in
// the error envelope; another key is not affected; a closed stream
// frees its slot.
func TestHandle_StreamsPerKeyCapped(t *testing.T) {
	_, _, h, srv := streamServer(t)
	var first *http.Response
	for i := 0; i < h.MaxStreams; i++ {
		resp, _ := open(t, srv, 1)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i, resp.StatusCode)
		}
		if first == nil {
			first = resp
		}
	}
	resp, _ := open(t, srv, 1)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), `"code":"rate_limited"`) {
		t.Errorf("stream over the cap: %d %s, want 429 rate_limited", resp.StatusCode, body)
	}
	if resp, _ := open(t, srv, 2); resp.StatusCode != http.StatusOK {
		t.Errorf("another key: %d, want 200", resp.StatusCode)
	}
	_ = first.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := open(t, srv, 1)
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("closing a stream did not free its slot: %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client that stops reading is dropped once a write blocks for
// WriteTimeout, and its slot is freed.
func TestHandle_DropsClientThatDoesNotRead(t *testing.T) {
	_, broker, h, srv := streamServer(t)
	h.WriteTimeout = 200 * time.Millisecond
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /rooms/R/events?wait=300 HTTP/1.1\r\nHost: x\r\nX-Key: 1\r\n\r\n") // and never read
	deadline := time.Now().Add(5 * time.Second)
	for broker.SubscriberCount("R") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stream did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	payload := []byte(`"` + strings.Repeat("x", 1<<20) + `"`)
	deadline = time.Now().Add(20 * time.Second)
	for broker.SubscriberCount("R") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a client that does not read is still subscribed")
		}
		broker.Publish("R", payload)
		time.Sleep(5 * time.Millisecond)
	}
	// The handler releases its slot after the broker drops the subscriber.
	for {
		h.mu.Lock()
		n := h.open[1]
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("open streams of key 1: %d, want 0", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
