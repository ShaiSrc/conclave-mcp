package federation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// local is the domain of the server under test.
const local = "b.example"

// remote is a peer: an identity whose document is served on 127.0.0.1.
type remote struct {
	id   *Identity
	hits atomic.Int32 // document requests
}

func (rm *remote) domain() string { return rm.id.Domain }

func newRemote(t *testing.T) *remote { return newRemoteHook(t, nil) }

// newRemoteHook is newRemote, calling onFetch with the remote's
// identity before it serves its document.
func newRemoteHook(t *testing.T, onFetch func(*Identity)) *remote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rm := &remote{id: testIdentity(t, ln.Addr().String())}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rm.hits.Add(1)
		if onFetch != nil {
			onFetch(rm.id)
		}
		_ = json.NewEncoder(w).Encode(rm.id.Document())
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return rm
}

func newLocal(t *testing.T, mode string) *Service {
	t.Helper()
	svc, err := New(testDB(t), Options{Domain: local, Mode: mode, InsecureHTTP: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// allowLoopback lets svc fetch documents from 127.0.0.1 for any
// domain, so open-mode tests can reach a remote.
func allowLoopback(svc *Service) {
	svc.fetch.client.Transport.(*http.Transport).DialContext = (&net.Dialer{}).DialContext
}

func allow(t *testing.T, svc *Service, id *Identity) {
	t.Helper()
	if err := svc.peers.Allow(context.Background(), id.Domain, id.Public, ""); err != nil {
		t.Fatal(err)
	}
}

// signed is a ping to dest carrying body, signed by id for dest.
func signed(t *testing.T, id *Identity, dest, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/ping", strings.NewReader(body))
	req.Host = dest
	if err := id.SignRequest(req, dest, []byte(body)); err != nil {
		t.Fatal(err)
	}
	return req
}

// signedAt is a ping to local signed by id with timestamp ts.
func signedAt(id *Identity, ts int64) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/ping", strings.NewReader("{}"))
	req.Host = local
	nonce := newNonce()
	sig := id.Sign(SigningString(http.MethodPost, "/fed/v1/ping", local, id.Domain, ts, nonce, []byte("{}")))
	req.Header.Set("Authorization", formatSigHeader(SigParams{Origin: id.Domain, KeyID: id.KeyID, TS: ts, Nonce: nonce, Sig: sig}))
	return req
}

func serve(svc *Service, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	svc.PublicRoutes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != status || env.Error.Code != code {
		t.Errorf("got %d %s, want %d %s", rec.Code, strings.TrimSpace(rec.Body.String()), status, code)
	}
}

func wantOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"domain":"b.example"}` {
		t.Errorf("got %d %s, want 200", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Step 1.
func TestVerify_MalformedHeader(t *testing.T) {
	svc := newLocal(t, "open")
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/ping", nil)
	req.Host = local
	wantErr(t, serve(svc, req), http.StatusUnauthorized, "bad_signature")
	req.Header.Set("Authorization", "Bearer abc")
	wantErr(t, serve(svc, req), http.StatusUnauthorized, "bad_signature")
}

// Step 2.
func TestVerify_InvalidOrigin(t *testing.T) {
	svc := newLocal(t, "open")
	id := testIdentity(t, "a.example")
	for _, origin := range []string{"bad..example", "A.example", "a.example/x", ""} {
		id.Domain = origin
		wantErr(t, serve(svc, signed(t, id, local, "{}")), http.StatusUnauthorized, "bad_signature")
	}
}

// Step 3, allowlist: an unknown or merely seen origin is refused
// before anything is fetched.
func TestVerify_AllowlistRefusesUnknown(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	wantErr(t, serve(svc, signed(t, rm.id, local, "{}")), http.StatusForbidden, "peer_not_allowed")
	if _, err := svc.peers.RecordSeen(context.Background(), rm.domain(), rm.id.Public); err != nil {
		t.Fatal(err)
	}
	wantErr(t, serve(svc, signed(t, rm.id, local, "{}")), http.StatusForbidden, "peer_not_allowed")
	if n := rm.hits.Load(); n != 0 {
		t.Errorf("document fetched %d times", n)
	}
}

// Step 3: blocked wins in both modes.
func TestVerify_BlockedWins(t *testing.T) {
	for _, mode := range []string{"allowlist", "open"} {
		svc := newLocal(t, mode)
		allowLoopback(svc)
		rm := newRemote(t)
		allow(t, svc, rm.id)
		if err := svc.peers.Block(context.Background(), rm.domain()); err != nil {
			t.Fatal(err)
		}
		wantErr(t, serve(svc, signed(t, rm.id, local, "{}")), http.StatusForbidden, "peer_not_allowed")
		if n := rm.hits.Load(); n != 0 {
			t.Errorf("%s: document fetched %d times", mode, n)
		}
	}
}

func TestVerify_AllowedPeer(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	wantOK(t, serve(svc, signed(t, rm.id, local, "{}")))
	wantOK(t, serve(svc, signed(t, rm.id, local, "{}")))
	if n := rm.hits.Load(); n != 0 {
		t.Errorf("document fetched %d times, want 0 (stored key)", n)
	}
	p, err := svc.peers.Get(context.Background(), rm.domain())
	if err != nil || p.Status != "allowed" || p.LastSeenAt == nil {
		t.Errorf("peer after ping: %+v, %v", p, err)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_nonces WHERE origin = ?`, rm.domain()); n != 2 {
		t.Errorf("nonces stored: %d, want 2", n)
	}
}

// Step 4: a key other than the stored one is refused. The peer's
// document lists the presented key, which is evidence of a key change:
// audited, once per peer a minute.
func TestVerify_KeyChanged(t *testing.T) {
	ctx := context.Background()
	other := testIdentity(t, "x.example")
	for _, tc := range []struct {
		mode   string
		record func(*Service, string) error
	}{
		{"allowlist", func(s *Service, d string) error { return s.peers.Allow(ctx, d, other.Public, "") }},
		{"open", func(s *Service, d string) error { _, err := s.peers.RecordSeen(ctx, d, other.Public); return err }},
	} {
		svc := newLocal(t, tc.mode)
		allowLoopback(svc)
		rm := newRemote(t)
		if err := tc.record(svc, rm.domain()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			wantErr(t, serve(svc, signed(t, rm.id, local, "{}")), http.StatusUnauthorized, "bad_signature")
		}
		var targetType, targetID, detail string
		if err := svc.db.QueryRow(`SELECT target_type, target_id, detail FROM audit
			WHERE action = 'federation.key_changed'`).Scan(&targetType, &targetID, &detail); err != nil {
			t.Fatalf("%s: no single key_changed audit row: %v", tc.mode, err)
		}
		want := fmt.Sprintf(`{"presented":%q,"expected":%q}`, rm.id.Fingerprint(), other.Fingerprint())
		if targetType != "federation_peer" || targetID != rm.domain() || detail != want {
			t.Errorf("%s: audit %s %s %s, want peer %s %s", tc.mode, targetType, targetID, detail, rm.domain(), want)
		}
		if n := rm.hits.Load(); n != 1 {
			t.Errorf("%s: document fetched %d times, want 1", tc.mode, n)
		}
	}
}

// Step 4: a known peer's stored key verifies its requests, whatever its
// document serves now; nothing is fetched.
func TestVerify_UsesStoredKey(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	old := testIdentity(t, rm.domain())
	allow(t, svc, old)
	wantOK(t, serve(svc, signed(t, old, local, "{}")))
	if n := rm.hits.Load(); n != 0 {
		t.Errorf("document fetched %d times", n)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM audit`); n != 0 {
		t.Errorf("audit rows: %d", n)
	}
}

// Step 5.
func TestVerify_WrongDestination(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	wantErr(t, serve(svc, signed(t, rm.id, "c.example", "{}")), http.StatusMisdirectedRequest, "wrong_destination")
	// Sent to us, but signed for another server.
	req := signed(t, rm.id, "c.example", "{}")
	req.Host = local
	wantErr(t, serve(svc, req), http.StatusUnauthorized, "bad_signature")
}

// Step 6.
func TestVerify_ClockSkew(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	now := time.Now().Unix()
	for _, ts := range []int64{now - 301, now + 301} {
		wantErr(t, serve(svc, signedAt(rm.id, ts)), http.StatusUnauthorized, "clock_skew")
	}
	for _, ts := range []int64{now - 290, now + 290} {
		wantOK(t, serve(svc, signedAt(rm.id, ts)))
	}
}

// Step 7: the body cap.
func TestVerify_TooLarge(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	body := `{"x":"` + strings.Repeat("a", 64<<10) + `"}`
	wantErr(t, serve(svc, signed(t, rm.id, local, body)), http.StatusRequestEntityTooLarge, "too_large")
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_nonces`); n != 0 {
		t.Errorf("nonces stored: %d", n)
	}
}

// Step 7: a bad signature stores no nonce, no seen peer and no audit row.
func TestVerify_BadSignatureStoresNothing(t *testing.T) {
	svc := newLocal(t, "open")
	allowLoopback(svc)
	rm := newRemote(t)
	req := signed(t, rm.id, local, "{}")
	req.Body = io.NopCloser(strings.NewReader("[]"))
	wantErr(t, serve(svc, req), http.StatusUnauthorized, "bad_signature")
	for _, table := range []string{"federation_nonces", "federation_peers", "audit"} {
		if n := count(t, svc.db, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("%s rows: %d", table, n)
		}
	}
}

// Step 8.
func TestVerify_Replay(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	req := signed(t, rm.id, local, "{}")
	hdr := req.Header.Get("Authorization")
	wantOK(t, serve(svc, req))
	again := httptest.NewRequest(http.MethodPost, "/fed/v1/ping", strings.NewReader("{}"))
	again.Host = local
	again.Header.Set("Authorization", hdr)
	wantErr(t, serve(svc, again), http.StatusUnauthorized, "bad_signature")
}

// Step 9.
func TestVerify_RateLimit(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	for i := 0; i < 30; i++ {
		if rec := serve(svc, signed(t, rm.id, local, "{}")); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := serve(svc, signed(t, rm.id, local, "{}"))
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

// Steps 3 and 10, open mode: an unknown origin's document is fetched,
// the request verified, and the origin recorded as seen and audited.
func TestVerify_OpenModeFirstContact(t *testing.T) {
	svc := newLocal(t, "open")
	allowLoopback(svc)
	rm := newRemote(t)
	wantOK(t, serve(svc, signed(t, rm.id, local, "{}")))
	p, err := svc.peers.Get(context.Background(), rm.domain())
	if err != nil || p.Status != "seen" || p.Fingerprint != rm.id.Fingerprint() || p.LastSeenAt == nil {
		t.Errorf("peer after first contact: %+v, %v", p, err)
	}
	var targetType, targetID, detail string
	if err := svc.db.QueryRow(`SELECT target_type, target_id, detail FROM audit
		WHERE action = 'federation.peer.seen'`).Scan(&targetType, &targetID, &detail); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"fingerprint":%q}`, rm.id.Fingerprint()); targetType != "federation_peer" || targetID != rm.domain() || detail != want {
		t.Errorf("audit %s %s %s", targetType, targetID, detail)
	}
	wantOK(t, serve(svc, signed(t, rm.id, local, "{}")))
	if n := rm.hits.Load(); n != 1 {
		t.Errorf("document fetched %d times, want 1", n)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.peer.seen'`); n != 1 {
		t.Errorf("peer.seen rows: %d, want 1", n)
	}
}

// Open mode: the dialer refuses an unknown origin on a loopback address.
func TestVerify_OpenModeRefusesPrivateOrigin(t *testing.T) {
	svc := newLocal(t, "open")
	rm := newRemote(t)
	wantErr(t, serve(svc, signed(t, rm.id, local, "{}")), http.StatusBadGateway, "peer_unreachable")
	if n := rm.hits.Load(); n != 0 {
		t.Errorf("document fetched %d times", n)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peers`); n != 0 {
		t.Errorf("peers: %d", n)
	}
}

// Open mode: a key the origin's document does not list is refused.
func TestVerify_OpenModeUnknownKey(t *testing.T) {
	svc := newLocal(t, "open")
	allowLoopback(svc)
	rm := newRemote(t)
	stranger := testIdentity(t, rm.domain())
	wantErr(t, serve(svc, signed(t, stranger, local, "{}")), http.StatusUnauthorized, "bad_signature")
	// The header names keys by key ID, not by fingerprint.
	byFingerprint := *rm.id
	byFingerprint.KeyID = rm.id.Fingerprint()
	wantErr(t, serve(svc, signed(t, &byFingerprint, local, "{}")), http.StatusUnauthorized, "bad_signature")
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peers`); n != 0 {
		t.Errorf("peers: %d", n)
	}
}

// The handler gets the verified origin and the whole body.
func TestVerify_PassesOriginAndBody(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	var origin, body string
	h := svc.Verify(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		origin = OriginFrom(r.Context())
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	h.ServeHTTP(httptest.NewRecorder(), signed(t, rm.id, local, `{"a":1}`))
	if origin != rm.domain() || body != `{"a":1}` {
		t.Errorf("handler saw origin %q body %q", origin, body)
	}
	if OriginFrom(context.Background()) != "" {
		t.Error("OriginFrom without Verify is not empty")
	}
}

func TestPing_Body(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	for _, body := range []string{"", "{}", " { } "} {
		wantOK(t, serve(svc, signed(t, rm.id, local, body)))
	}
	for _, body := range []string{`{"x":1}`, "[]", "null", "{"} {
		wantErr(t, serve(svc, signed(t, rm.id, local, body)), http.StatusBadRequest, "bad_request")
	}
}

// One warn line per origin and error code a minute: a forged origin
// cannot silence another code for it.
func TestVerify_LogsRefusalsOncePerOriginAndCode(t *testing.T) {
	var logs bytes.Buffer
	svc, err := New(testDB(t), Options{Domain: local, Mode: "allowlist", Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	a, b := newRemote(t), newRemote(t)
	for i := 0; i < 3; i++ {
		serve(svc, signed(t, a.id, local, "{}")) // peer_not_allowed
	}
	allow(t, svc, a.id)
	serve(svc, signedAt(a.id, time.Now().Unix()-1000)) // clock_skew
	serve(svc, signed(t, b.id, local, "{}"))
	if n := strings.Count(logs.String(), "level=WARN"); n != 3 {
		t.Errorf("%d warn lines, want 3:\n%s", n, logs.String())
	}
}

// Step 4: a request naming a key other than the stored one is refused.
// Anyone can send one, so it is audited only when the peer's own
// document lists that key, and the document is fetched at most once
// per peer a minute.
func TestVerify_ForgedKeyNotAudited(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	forger := testIdentity(t, rm.domain())
	for i := 0; i < 2; i++ {
		wantErr(t, serve(svc, signed(t, forger, local, "{}")), http.StatusUnauthorized, "bad_signature")
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM audit`); n != 0 {
		t.Errorf("audit rows: %d, want 0", n)
	}
	if n := rm.hits.Load(); n != 1 {
		t.Errorf("document fetched %d times, want 1", n)
	}
}

// Step 10: a row that appears for the origin while its first request is
// verified decides; the request is served only if that row lets the
// origin in with the key that signed it.
func TestVerify_FirstContactRace(t *testing.T) {
	ctx := context.Background()
	other := testIdentity(t, "x.example")
	for _, tc := range []struct {
		name      string
		meanwhile func(*Service, *Identity) error
		status    int
		code      string
	}{
		{"blocked", func(s *Service, id *Identity) error { return s.peers.Block(ctx, id.Domain) }, http.StatusForbidden, "peer_not_allowed"},
		{"another key", func(s *Service, id *Identity) error {
			_, err := s.peers.RecordSeen(ctx, id.Domain, other.Public)
			return err
		}, http.StatusUnauthorized, "bad_signature"},
		{"same key", func(s *Service, id *Identity) error {
			_, err := s.peers.RecordSeen(ctx, id.Domain, id.Public)
			return err
		}, http.StatusOK, ""},
	} {
		svc := newLocal(t, "open")
		allowLoopback(svc)
		rm := newRemoteHook(t, func(id *Identity) {
			if err := tc.meanwhile(svc, id); err != nil {
				t.Error(err)
			}
		})
		rec := serve(svc, signed(t, rm.id, local, "{}"))
		if tc.status == http.StatusOK {
			wantOK(t, rec)
		} else {
			wantErr(t, rec, tc.status, tc.code)
		}
		if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.peer.seen'`); n != 0 {
			t.Errorf("%s: peer.seen rows: %d, want 0", tc.name, n)
		}
	}
}

// startService runs a service with its federation routes on 127.0.0.1.
func startService(t *testing.T, mode string) *Service {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(testDB(t), Options{Domain: ln.Addr().String(), Mode: mode, InsecureHTTP: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc.PublicRoutes(r)
	srv := httptest.NewUnstartedServer(r)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return svc
}

func TestPing_Outbound(t *testing.T) {
	ctx := context.Background()
	a, b, c := startService(t, "allowlist"), startService(t, "allowlist"), startService(t, "allowlist")
	allow(t, a, b.id)
	allow(t, b, a.id)
	if rtt, err := a.Ping(ctx, b.id.Domain); err != nil || rtt <= 0 {
		t.Errorf("a→b: %v, %v", rtt, err)
	}
	if _, err := b.Ping(ctx, a.id.Domain); err != nil {
		t.Errorf("b→a: %v", err)
	}
	// c is not allowed on a. c allows a, so its dialer may reach a's
	// loopback address.
	allow(t, c, a.id)
	if _, err := c.Ping(ctx, a.id.Domain); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "peer_not_allowed") {
		t.Errorf("c→a: %v, want 403 peer_not_allowed", err)
	}
	// a has not allowed c, so its dialer refuses c's loopback address.
	if _, err := a.Ping(ctx, c.id.Domain); !errors.Is(err, errNonPublicAddr) {
		t.Errorf("a→c: %v, want the private address refused", err)
	}
}

func TestPruneNonces(t *testing.T) {
	svc := newLocal(t, "allowlist")
	now := time.Now()
	for nonce, exp := range map[string]time.Duration{"old": -2 * time.Minute, "just-expired": -30 * time.Second, "live": time.Minute} {
		if _, err := svc.db.Exec(`INSERT INTO federation_nonces(origin, nonce, expires_at) VALUES ('a.example', ?, ?)`,
			nonce, now.Add(exp).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.pruneNonces(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	// Rows are kept a minute past expiry; see pruneNonces.
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_nonces WHERE nonce = 'old'`); n != 0 {
		t.Error("an expired nonce was kept")
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_nonces`); n != 2 {
		t.Errorf("%d nonces left, want 2", n)
	}
}

func TestRun_StopsWithContext(t *testing.T) {
	svc := newLocal(t, "allowlist")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
