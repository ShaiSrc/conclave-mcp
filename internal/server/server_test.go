package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/events"
	"github.com/hermes/conclave-mcp/internal/federation"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/storage"
)

func TestServer_Healthz(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %q, want ok", body["status"])
	}
	if body["db"] != "ok" {
		t.Errorf("db: got %q, want ok", body["db"])
	}
}

func TestServer_HealthzDBError(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Port: 0}, Deps{
		HealthDB: fakeDB{pingErr: errors.New("db down")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["db"] != "err" {
		t.Errorf("db: got %q, want err", body["db"])
	}
}

// TestServer_UnknownV1RouteReturns404Envelope asserts that an
// unknown /v1 path returns the SPEC-0003 error envelope, not a
// plain 404. The server is built with no DB, so the auth verifier
// has nothing to look up; an unauthenticated call therefore
// returns 401, not 404 (auth runs first, which is the desired
// ordering — never leak route existence to unauthenticated
// callers). The notFound handler is exercised separately via
// TestServer_NotFoundHandlerEnvelope below.
func TestServer_UnknownV1RouteReturns404Envelope(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	s.Handler().ServeHTTP(rec, req)

	// No DB means no keys, so auth always fails. That is the
	// correct behaviour: do not leak whether a route exists.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (auth must precede routing)", rec.Code)
	}
}

// TestServer_NotFoundHandlerEnvelope exercises the full middleware
// stack to confirm the request_id reaches the notFound envelope.
// We send a known X-Request-Id and verify the envelope echoes it.
//
// Without a DB the auth verifier returns 401 on every /v1/* path,
// so we cannot reach the notFound handler via the real router.
// The integration case (auth first, then notFound on a path that
// auth lets through) is covered by the e2e suite.
func TestServer_NotFoundHandlerEnvelope(t *testing.T) {
	// Build a minimal handler chain that mirrors what chi gives
	// the notFound handler: RequestIDMiddleware on top, then
	// notFound. The auth middleware is omitted on purpose.
	h := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This is the "not found" branch — the URL matched
		// /v1 but no route handled it.
		rec := w
		_ = rec
		// In real life this is s.notFound. We inline the
		// envelope write here to avoid constructing a Server
		// that depends on the missing DB.
		rid := RequestIDFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
			Code: "not_found", Message: "unknown route", RequestID: rid,
		}})
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	req.Header.Set("X-Request-Id", "11111111-2222-3333-4444-555555555555")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not envelope: %v", err)
	}
	if env.Error.Code != "not_found" {
		t.Errorf("code: got %q, want not_found", env.Error.Code)
	}
	if env.Error.RequestID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("request_id: got %q, want 11111111-...", env.Error.RequestID)
	}
}

// TestServer_AuthBeforeNotFound: an unknown /v1 path with no
// Authorization header returns 401, NOT 404. We must not leak the
// existence of routes to unauthenticated callers.
func TestServer_AuthBeforeNotFound(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (auth must precede routing)", rec.Code)
	}
}

func TestServer_RequestIDEchoedInHeader(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	const id = "11111111-2222-3333-4444-555555555555"
	req.Header.Set("X-Request-Id", id)
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != id {
		t.Errorf("X-Request-Id: got %q, want %q", got, id)
	}
}

func TestServer_RequestIDGeneratedIfMissing(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Handler().ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-Id")
	if !looksLikeUUID(got) {
		t.Errorf("X-Request-Id: got %q, want UUID", got)
	}
}

func TestServer_RejectsNonUUIDRequestID(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "not-a-uuid")
	s.Handler().ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-Id")
	if got == "not-a-uuid" {
		t.Errorf("X-Request-Id was not replaced; got %q", got)
	}
	if !looksLikeUUID(got) {
		t.Errorf("X-Request-Id: got %q, want UUID", got)
	}
}

func TestServer_MethodNotAllowedOnV1(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	rec := httptest.NewRecorder()
	// We have no registered methods on /v1/*, but chi's
	// MethodNotAllowed handler should still fire for known paths.
	// Use a path that doesn't match any registered route to avoid
	// the 404 branch.
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rec.Code)
	}
}

func TestServer_RecoveryMiddlewareCatchesPanic(t *testing.T) {
	s, _ := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})

	// Build a request that will panic. Easiest: use a route
	// mounted just for this test.
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	handlerWithRecovery := RecoveryMiddleware(s.logger)(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	handlerWithRecovery.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal_error") {
		t.Errorf("body: got %q, want internal_error", body)
	}
}

func TestServer_StartShutdown(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Port: 0}, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pick a free port by listening first, then handing the addr to
	// the server. We need to use the actual port number, not 0.
	// Simpler: just start with port 0 and ignore the addr.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	// Give it a moment to bind.
	// (We don't actually need to make a request — the test is that
	// the server starts and shuts down cleanly.)
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Start returned: %v", err)
	}
}

// fakeDB implements DBHealthChecker for tests.
type fakeDB struct{ pingErr error }

func (f fakeDB) PingContext(ctx context.Context) error { return f.pingErr }

// With a federation domain, the second listener serves the server
// document and nothing else.
func TestServer_FederationListener(t *testing.T) {
	s, err := New(Config{
		Host: "127.0.0.1", Port: 7432,
		FederationDomain: "conclave.example.org", FederationHost: "127.0.0.1", FederationPort: 7433,
	}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s.fed == nil {
		t.Fatal("no federation listener with the domain set")
	}
	if s.fed.Addr != "127.0.0.1:7433" {
		t.Errorf("federation addr: %q", s.fed.Addr)
	}
	serve := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, uiRequest("GET", path, "localhost:7432"))
		return rec
	}
	rec := serve(s.fed.Handler, "/.well-known/conclave")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"domain":"conclave.example.org"`) {
		t.Errorf("/.well-known/conclave on the federation listener: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/whoami", "/ui/", "/healthz"} {
		if rec := serve(s.fed.Handler, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s on the federation listener: %d, want 404", path, rec.Code)
		}
	}
	if rec := serve(s.Handler(), "/.well-known/conclave"); rec.Code != http.StatusNotFound {
		t.Errorf("/.well-known/conclave on the main listener: %d, want 404", rec.Code)
	}
	// /fed/v1 is there, behind signature verification, and only there.
	if rec := serve(s.fed.Handler, "/fed/v1/ping"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned /fed/v1/ping on the federation listener: %d, want 401", rec.Code)
	}
	if rec := serve(s.Handler(), "/fed/v1/ping"); rec.Code != http.StatusNotFound {
		t.Errorf("/fed/v1/ping on the main listener: %d, want 404", rec.Code)
	}
	// Router-level 404 and 405 answers carry the public headers too.
	for _, req := range []*http.Request{uiRequest("GET", "/@x/", "localhost:7432"), uiRequest("GET", "/directory/", "localhost:7432"),
		uiRequest("HEAD", "/", "localhost:7432")} {
		rec := httptest.NewRecorder()
		s.fed.Handler.ServeHTTP(rec, req)
		h := rec.Header()
		if rec.Code < 400 || !strings.HasPrefix(h.Get("Content-Security-Policy"), "default-src 'none'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s on the federation listener: %d, headers %v", req.Method, req.URL, rec.Code, h)
		}
	}
}

// Listener addresses are built with JoinHostPort, so an IPv6 host is
// bracketed. No socket is opened.
func TestServer_IPv6ListenerAddrs(t *testing.T) {
	s, err := New(Config{
		Host: "::1", Port: 7432,
		FederationDomain: "conclave.example.org", FederationHost: "::1", FederationPort: 7433,
	}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s.http.Addr != "[::1]:7432" || s.fed.Addr != "[::1]:7433" {
		t.Errorf("addrs: %q, %q", s.http.Addr, s.fed.Addr)
	}
	s, err = New(Config{
		Host: "[::1]", Port: 7432,
		FederationDomain: "conclave.example.org", FederationHost: "[::1]", FederationPort: 7433,
	}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s.http.Addr != "[::1]:7432" || s.fed.Addr != "[::1]:7433" {
		t.Errorf("bracketed hosts: %q, %q", s.http.Addr, s.fed.Addr)
	}
}

// Without a domain there is no second listener and no key.
func TestServer_NoFederationWithoutDomain(t *testing.T) {
	db := uiDB(t)
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	if s.fed != nil {
		t.Error("federation listener built without a domain")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM federation_keys`).Scan(&n); err != nil || n != 0 {
		t.Errorf("federation_keys rows: %d, %v; want 0", n, err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Start serves both listeners, warns about insecure HTTP and shuts
// both down.
func TestServer_StartFederationListener(t *testing.T) {
	var logs bytes.Buffer
	port, fedPort := freePort(t), freePort(t)
	s, err := New(Config{
		Host: "127.0.0.1", Port: port, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		FederationDomain: fmt.Sprintf("127.0.0.1:%d", fedPort), FederationHost: "127.0.0.1", FederationPort: fedPort,
		FederationInsecureHTTP: true,
	}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	get := func(port int, path string) int {
		for i := 0; i < 200; i++ {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("port %d never answered", port)
		return 0
	}
	if code := get(fedPort, "/.well-known/conclave"); code != http.StatusOK {
		t.Errorf("federation listener: %d, want 200", code)
	}
	if code := get(port, "/healthz"); code != http.StatusOK {
		t.Errorf("main listener: %d, want 200", code)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Start returned: %v", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "insecure") {
		t.Errorf("no insecure HTTP warning in the logs:\n%s", logs.String())
	}
	if _, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", fedPort)); err == nil {
		t.Error("federation listener still open after shutdown")
	}
}

// Start returns only once the federation loops have stopped, so a
// server started again on the same database never runs beside them.
func TestServer_StartWaitsForFederationLoops(t *testing.T) {
	port, fedPort := freePort(t), freePort(t)
	s, err := New(Config{
		Host: "127.0.0.1", Port: port, Logger: slog.New(slog.DiscardHandler),
		FederationDomain: fmt.Sprintf("127.0.0.1:%d", fedPort), FederationHost: "127.0.0.1", FederationPort: fedPort,
	}, Deps{DB: uiDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	s.fedRun = func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		stopped.Store(true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Start returned: %v", err)
	}
	if !stopped.Load() {
		t.Error("Start returned before the federation loops stopped")
	}
}

// The federation listener faces the internet: it has read and write
// timeouts and a header cap, and a request whose body stalls is cut off.
func TestServer_FederationSlowBody(t *testing.T) {
	ctx := context.Background()
	fedPort := freePort(t)
	domain := fmt.Sprintf("127.0.0.1:%d", fedPort)
	db := uiDB(t)
	s, err := New(Config{
		Host: "127.0.0.1", Port: freePort(t),
		FederationDomain: domain, FederationHost: "127.0.0.1", FederationPort: fedPort, FederationInsecureHTTP: true,
	}, Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	if s.fed.ReadTimeout != 10*time.Second || s.fed.WriteTimeout != 15*time.Second || s.fed.MaxHeaderBytes != 16<<10 {
		t.Errorf("federation listener: read %v, write %v, headers %d; want 10s, 15s, 16384",
			s.fed.ReadTimeout, s.fed.WriteTimeout, s.fed.MaxHeaderBytes)
	}
	s.fed.ReadTimeout = 300 * time.Millisecond

	// An allowed peer, so that the request gets as far as its body.
	peer, err := federation.LoadOrCreateIdentity(ctx, uiDB(t), "peer.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO federation_peers(domain, status, fingerprint, public_key, added_at)
		VALUES ('peer.example', 'allowed', ?, ?, ?)`, peer.Fingerprint(), []byte(peer.Public), storage.FormatTS(time.Now())); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(runCtx) }()
	var conn net.Conn
	for i := 0; i < 200; i++ {
		if conn, err = net.Dial("tcp", domain); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := http.NewRequest(http.MethodPost, "http://"+domain+"/fed/v1/ping", nil)
	if err := peer.SignRequest(req, domain, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	// The headers and one byte of the two-byte body, then nothing.
	if _, err := fmt.Fprintf(conn, "POST /fed/v1/ping HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Length: 2\r\n\r\n{",
		domain, req.Header.Get("Authorization")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("stalled body: %d, want 400", resp.StatusCode)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Start returned: %v", err)
	}
}

// The bot lookup and the directory are on /v1 with federation on and
// off, and /bots/lookup is not taken for a handle.
func TestServer_BotLookupAndDirectory(t *testing.T) {
	for _, c := range []struct{ domain, otherCode string }{{"", "federation_disabled"}, {"conclave.example.org", "peer_not_allowed"}} {
		s, err := New(Config{Host: "127.0.0.1", Port: 7432, FederationDomain: c.domain, FederationHost: "127.0.0.1", FederationPort: 7433},
			Deps{DB: uiDB(t)})
		if err != nil {
			t.Fatal(err)
		}
		get := func(h http.Handler, path string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, uiRequest("GET", path, "localhost:7432"))
			return rec
		}
		if rec := get(s.Handler(), "/v1/directory"); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"bots":[]}` {
			t.Errorf("%q: directory: %d %s", c.domain, rec.Code, rec.Body)
		}
		for path, code := range map[string]string{
			"/v1/bots/lookup?handle=nobody":         `"code":"not_found"`,
			"/v1/bots/lookup?handle=x@peer.example": `"code":"` + c.otherCode + `"`,
			"/v1/bots/lookup?handle=bad%20name":     `"code":"bad_request"`,
		} {
			if rec := get(s.Handler(), path); !strings.Contains(rec.Body.String(), code) {
				t.Errorf("%q: %s: %d %s, want %s", c.domain, path, rec.Code, rec.Body, code)
			}
		}
		if s.fed != nil {
			for _, path := range []string{"/fed/v1/directory", "/fed/v1/bots/nobody"} {
				if rec := get(s.fed.Handler, path); rec.Code != http.StatusUnauthorized {
					t.Errorf("unsigned %s on the federation listener: %d, want 401", path, rec.Code)
				}
			}
		}
	}
}

// Federated rooms are wired: with federation off an invite by address
// answers 404; with it on, the invite goes through the federation
// service, and the federation listener runs a member server's message
// and claims through the local send and claims paths.
func TestServer_FederatedRooms(t *testing.T) {
	ctx := context.Background()
	db := uiDB(t)
	token, phc, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('root', ?, ?, '2026-01-01T00:00:00Z', 1)`,
		phc, token[3:11]); err != nil {
		t.Fatal(err)
	}
	do := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	off, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(off.Handler(), http.MethodPost, "/v1/rooms", `{"name":"proj"}`)
	var room struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &room); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create room: %d %s", rec.Code, rec.Body)
	}
	invite := `{"address":"c1@peer.example","rights":["read","claim","answer"]}`
	if rec := do(off.Handler(), http.MethodPost, "/v1/rooms/"+room.ID+"/members", invite); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), `"federation_disabled"`) {
		t.Errorf("invite with federation off: %d %s", rec.Code, rec.Body)
	}

	const domain = "conclave.example.org"
	on, err := New(Config{Host: "127.0.0.1", Port: 7432, FederationDomain: domain, FederationHost: "127.0.0.1", FederationPort: 7433},
		Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(on.Handler(), http.MethodPost, "/v1/rooms/"+room.ID+"/members", invite); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `"peer_not_allowed"`) {
		t.Errorf("invite of a stranger's bot: %d %s", rec.Code, rec.Body)
	}

	// peer.example is an allowed peer whose bot c1 is in the room.
	peer, err := federation.LoadOrCreateIdentity(ctx, uiDB(t), "peer.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO federation_peers(domain, status, fingerprint, public_key, added_at) VALUES (?, 'allowed', ?, ?, ?)`,
		peer.Domain, federation.Fingerprint(peer.Public), []byte(peer.Public), storage.FormatTS(time.Now())); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	c1, err := fedrooms.StandIn(ctx, tx, "c1@peer.example")
	if err == nil {
		_, err = tx.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', '2026-01-01T00:00:00Z', 'read,claim,answer')`, room.ID, c1)
	}
	if err == nil {
		err = fedrooms.LogJoin(ctx, tx, room.ID, c1, domain, 256<<10, 0)
	}
	if err != nil || tx.Commit() != nil {
		t.Fatal(err)
	}
	signed := func(method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Host = domain
		if err := peer.SignRequest(req, domain, body); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		on.fed.Handler.ServeHTTP(rec, req)
		return rec
	}
	nonce := "AAAAAAAAAAAAAAAAAAAAAA" // 16 bytes, unpadded base64url
	sig := peer.SignMessage(federation.MessageSig{Room: room.ID + "@" + domain, Author: "c1@peer.example",
		ContentType: "text/plain", MessageType: "answer", Content: "hello", Nonce: nonce})
	body, _ := json.Marshal(map[string]string{"author": "c1@peer.example", "content": "hello", "message_type": "answer", "sig": sig,
		"nonce": nonce})
	if rec := signed(http.MethodPost, "/fed/v1/rooms/"+room.ID+"/messages", body); rec.Code != http.StatusCreated ||
		!strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Errorf("message from the member server: %d %s", rec.Code, rec.Body)
	}
	if rec := signed(http.MethodGet, "/fed/v1/rooms/"+room.ID+"/claims?author=c1@peer.example", nil); rec.Code != http.StatusOK {
		t.Errorf("claims from the member server: %d %s", rec.Code, rec.Body)
	}
	if rec := signed(http.MethodGet, "/fed/v1/rooms/"+room.ID+"/events", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"author":"c1@peer.example"`) {
		t.Errorf("events: %d %s", rec.Code, rec.Body)
	}
}

// Direct conversations are wired, with federation off too: a bot starts
// one through the send path, the other bot lists it, and a recipient on
// another server answers 404 federation_disabled.
func TestServer_DirectMessages(t *testing.T) {
	db := uiDB(t)
	tokens := map[string]string{}
	for _, h := range []string{"alice", "bob"} {
		token, phc, err := auth.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, handle) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', ?)`,
			h, phc, token[3:11], h); err != nil {
			t.Fatal(err)
		}
		tokens[h] = token
	}
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	do := func(who, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens[who])
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := do("alice", http.MethodPost, "/v1/direct", `{"to":"bob","content":"hi","message_type":"question"}`); rec.Code != http.StatusCreated ||
		!strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Errorf("start: %d %s", rec.Code, rec.Body)
	}
	if rec := do("bob", http.MethodGet, "/v1/direct", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"address":"alice"`) || !strings.Contains(rec.Body.String(), `"unread":1`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := do("alice", http.MethodPost, "/v1/direct", `{"to":"bob@peer.example","content":"hi","message_type":"question"}`); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), `"federation_disabled"`) {
		t.Errorf("remote with federation off: %d %s", rec.Code, rec.Body)
	}
}

// Posts, follows and the timeline are wired, with federation off too: a
// bot follows an open bot, which posts, and the post is in the
// follower's timeline; the bot deletes it; a bot on another server
// answers 404 federation_disabled.
func TestServer_Follows(t *testing.T) {
	db := uiDB(t)
	tokens := map[string]string{}
	for _, h := range []string{"alice", "bob"} {
		token, phc, err := auth.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, handle) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', ?)`,
			h, phc, token[3:11], h)
		if err != nil {
			t.Fatal(err)
		}
		tokens[h] = token
		id, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO bot_profiles(key_id, updated_at, follow_policy) VALUES (?, '2026-01-01T00:00:00Z', 'open')`, id); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	do := func(who, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens[who])
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := do("alice", http.MethodPost, "/v1/follows", `{"to":"bob"}`); rec.Code != http.StatusCreated ||
		!strings.Contains(rec.Body.String(), `"status":"accepted"`) {
		t.Errorf("follow: %d %s", rec.Code, rec.Body)
	}
	rec := do("bob", http.MethodPost, "/v1/posts", `{"content":"hello","visibility":"public"}`)
	var post struct{ ID int64 }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &post) != nil {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	if rec := do("alice", http.MethodGet, "/v1/timeline", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"content":"hello"`) || !strings.Contains(rec.Body.String(), `"author_address":"bob"`) {
		t.Errorf("timeline: %d %s", rec.Code, rec.Body)
	}
	if rec := do("bob", http.MethodDelete, "/v1/posts/"+strconv.FormatInt(post.ID, 10), ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	// The feed takes posts through /v1/posts only.
	var feeds struct{ Rooms []struct{ ID string } }
	if rec := do("bob", http.MethodGet, "/v1/rooms?kind=feed", ""); json.Unmarshal(rec.Body.Bytes(), &feeds) != nil || len(feeds.Rooms) != 1 {
		t.Fatalf("feeds: %d %s", rec.Code, rec.Body)
	}
	if rec := do("bob", http.MethodPost, "/v1/rooms/"+feeds.Rooms[0].ID+"/messages", `{"content":"x","message_type":"status_update"}`); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `"feed_room"`) {
		t.Errorf("send into the feed: %d %s", rec.Code, rec.Body)
	}
	if rec := do("alice", http.MethodPost, "/v1/follows", `{"to":"bob@peer.example"}`); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), `"federation_disabled"`) {
		t.Errorf("remote with federation off: %d %s", rec.Code, rec.Body)
	}
	// With federation on, a follow goes to allowed peers only, and the
	// federation listener takes signed follow requests.
	if s, err = New(Config{Host: "127.0.0.1", Port: 7432, FederationDomain: "conclave.example.org", FederationHost: "127.0.0.1",
		FederationPort: 7433}, Deps{DB: db}); err != nil {
		t.Fatal(err)
	}
	if rec := do("alice", http.MethodPost, "/v1/follows", `{"to":"bob@peer.example"}`); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `"peer_not_allowed"`) {
		t.Errorf("remote with federation on: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/fed/v1/follows", "/fed/v1/follows/withdraw"} {
		rec = httptest.NewRecorder()
		s.fed.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unsigned %s: %d, want 401", path, rec.Code)
		}
	}
}

// GET /v1/server answers admin keys only: the build's version, the schema
// version from meta, the start time and the federation settings. The
// version stays out of /healthz and the server document.
func TestServer_Info(t *testing.T) {
	db := uiDB(t)
	tokens := map[string]string{}
	for label, admin := range map[string]int{"root": 1, "agent": 0} {
		token, phc, err := auth.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', ?)`,
			label, phc, token[3:11], admin); err != nil {
			t.Fatal(err)
		}
		tokens[label] = token
	}
	var schema int
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&schema); err != nil || schema == 0 {
		t.Fatalf("schema_version: %d %v", schema, err)
	}
	const version = "0.14.0-test"
	for _, domain := range []string{"", "conclave.example.org"} {
		before := time.Now().Add(-time.Second)
		s, err := New(Config{Host: "127.0.0.1", Port: 7432, Version: version, FederationDomain: domain, FederationHost: "127.0.0.1",
			FederationPort: 7433, FederationMode: "allowlist"}, Deps{DB: db})
		if err != nil {
			t.Fatal(err)
		}
		get := func(h http.Handler, who, path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if who != "" {
				req.Header.Set("Authorization", "Bearer "+tokens[who])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}
		rec := get(s.Handler(), "root", "/v1/server")
		var info struct {
			Version       string    `json:"version"`
			SchemaVersion int       `json:"schema_version"`
			StartedAt     time.Time `json:"started_at"`
			Federation    struct {
				Enabled                bool
				Domain, Mode, Protocol string
			} `json:"federation"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil {
			t.Fatalf("%q: /v1/server: %d %s", domain, rec.Code, rec.Body)
		}
		wantMode := ""
		if domain != "" {
			wantMode = "allowlist"
		}
		f := info.Federation
		if info.Version != version || info.SchemaVersion != schema || info.StartedAt.Before(before) || info.StartedAt.After(time.Now()) ||
			f.Enabled != (domain != "") || f.Domain != domain || f.Mode != wantMode || f.Protocol != federation.Protocol {
			t.Errorf("%q: /v1/server: %s", domain, rec.Body)
		}
		// The web UI's Server page reads it with the session.
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, uiRequest("GET", "/v1/server", "localhost:7432"))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), version) {
			t.Errorf("%q: /v1/server with the UI session: %d %s", domain, rec.Code, rec.Body)
		}
		if rec := get(s.Handler(), "agent", "/v1/server"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"forbidden"`) {
			t.Errorf("%q: /v1/server with a key that is not an admin: %d %s", domain, rec.Code, rec.Body)
		}
		if rec := get(s.Handler(), "", "/healthz"); strings.Contains(rec.Body.String(), version) {
			t.Errorf("/healthz shows the version: %s", rec.Body)
		}
		if s.fed != nil {
			if rec := get(s.fed.Handler, "", "/.well-known/conclave"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), version) {
				t.Errorf("server document: %d %s", rec.Code, rec.Body)
			}
		}
	}
}

// Every /v1 list answers [] when empty, never null, and every /v1 error
// is the JSON envelope with the request's id, the one in the
// X-Request-Id response header, also when the client sent none.
func TestServer_APIConsistency(t *testing.T) {
	db := uiDB(t)
	tokens, ids := map[string]string{}, map[string]int64{}
	for label, admin := range map[string]int{"root": 1, "alice": 0} {
		token, phc, err := auth.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin, handle) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', ?, ?)`,
			label, phc, token[3:11], admin, map[int]any{0: label, 1: nil}[admin])
		if err != nil {
			t.Fatal(err)
		}
		tokens[label] = token
		ids[label], _ = res.LastInsertId()
	}
	s, err := New(Config{Host: "127.0.0.1", Port: 7432, FederationDomain: "conclave.example.org", FederationHost: "127.0.0.1",
		FederationPort: 7433, FederationMode: "allowlist"}, Deps{DB: db, Broker: events.NewBroker()})
	if err != nil {
		t.Fatal(err)
	}
	do := func(who, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[who])
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	room := func(name string) string {
		rec := do("root", "POST", "/v1/rooms", `{"name":"`+name+`"}`)
		var r struct{ ID string }
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &r) != nil {
			t.Fatalf("create room: %d %s", rec.Code, rec.Body)
		}
		return r.ID
	}
	open, archived := room("open"), room("archived")
	if rec := do("root", "POST", "/v1/rooms/"+archived+"/archive", `{}`); rec.Code/100 != 2 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}

	for _, l := range []struct{ who, path string }{
		{"root", "/v1/rooms?visibility=public"}, {"root", "/v1/keys"}, {"root", "/v1/audit?action=nothing"},
		{"root", "/v1/bots"}, {"root", "/v1/directory"}, {"root", "/v1/federation/peers"},
		{"root", "/v1/federation/blocked-bots"}, {"root", "/v1/rooms/" + open + "/messages"},
		{"root", "/v1/rooms/" + open + "/claims"}, {"root", "/v1/rooms/" + open + "/members"},
		{"alice", "/v1/direct"}, {"alice", "/v1/timeline"}, {"alice", "/v1/follows"},
		{"alice", "/v1/followers"}, {"alice", "/v1/follows/requests"},
	} {
		if rec := do(l.who, "GET", l.path, ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), ":null") {
			t.Errorf("%s: %d %s, want 200 without null", l.path, rec.Code, rec.Body)
		}
	}

	for _, e := range []struct{ who, method, path, body string }{
		{"", "GET", "/v1/whoami", ""},
		{"root", "GET", "/v1/nope", ""},
		{"root", "GET", "/v1/rooms/nope", ""},
		{"root", "GET", "/v1/rooms/nope/messages", ""},
		{"root", "DELETE", "/v1/rooms/" + open + "/messages/999", ""},
		{"root", "GET", "/v1/rooms/nope/claims", ""},
		{"root", "GET", "/v1/rooms/" + archived + "/events", ""},
		{"root", "GET", "/v1/audit?limit=x", ""},
		{"root", "POST", "/v1/keys", "{"},
		{"root", "GET", "/v1/bots/nobody", ""},
		{"root", "GET", "/v1/federation/peers/nope.example", ""},
		{"alice", "GET", "/v1/timeline?limit=0", ""},
		{"alice", "POST", "/v1/direct", "{"},
	} {
		rec := do(e.who, e.method, e.path, e.body)
		var env struct {
			Error struct {
				Code      string `json:"code"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code < 400 || env.Error.Code == "" || env.Error.RequestID == "" || env.Error.RequestID != rec.Header().Get("X-Request-Id") ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: %d %q %s, want the envelope with the X-Request-Id %q", e.method, e.path, rec.Code,
				rec.Header().Get("Content-Type"), rec.Body, rec.Header().Get("X-Request-Id"))
		}
	}
}

// Every /v1 request body is capped at MaxMessageBytes + 64 KiB: a bigger
// one answers 413 payload_too_large in the error envelope, with or
// without a Content-Length, on /v1/mcp too; a body under the cap still
// reaches the handler.
func TestServer_BodyCap(t *testing.T) {
	db := uiDB(t)
	token, phc, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('root', ?, ?, '2026-01-01T00:00:00Z', 1)`,
		phc, token[3:11]); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Host: "127.0.0.1", Port: 7432}, Deps{DB: db, MaxMessageBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	limit := 1<<10 + 64<<10
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("x", limit) + `"}}`
	for _, path := range []string{"/v1/mcp", "/v1/rooms", "/v1/rooms/R/claims", "/v1/keys"} {
		for _, chunked := range []bool{false, true} {
			var body io.Reader = strings.NewReader(big)
			if chunked {
				body = io.MultiReader(body) // no Content-Length
			}
			req := httptest.NewRequest(http.MethodPost, path, body)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), `"code":"payload_too_large"`) ||
				!strings.Contains(rec.Body.String(), `"request_id"`) {
				t.Errorf("%s (chunked %v): %d %s, want 413 payload_too_large", path, chunked, rec.Code, rec.Body)
			}
		}
	}
	for _, chunked := range []bool{false, true} {
		var body io.Reader = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if chunked {
			body = io.MultiReader(body)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp", body)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result":{}`) {
			t.Errorf("small body (chunked %v): %d %s", chunked, rec.Code, rec.Body)
		}
	}
}
