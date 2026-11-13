package federation

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
)

var (
	plainKey = auth.Claims{KeyID: 99}
	adminKey = auth.Claims{KeyID: 1, Scopes: admin}
)

// addBot creates a key on db with handle, display name "Bot <handle>"
// and visibility vis.
func addBot(t *testing.T, db *sql.DB, handle, vis string) {
	t.Helper()
	ctx := context.Background()
	res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES (?, 'x', ?, '2026-01-01T00:00:00Z')`, handle, handle)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	st, name := bots.NewStore(db), "Bot "+handle
	if err := st.SetHandle(ctx, id, handle); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(ctx, handle, bots.Patch{DisplayName: &name, Visibility: &vis}, true); err != nil {
		t.Fatal(err)
	}
}

// signedGet is a GET of path on local, signed by id.
func signedGet(t *testing.T, id *Identity, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = local
	if err := id.SignRequest(req, local, nil); err != nil {
		t.Fatal(err)
	}
	return req
}

// newPeer starts a federation Service on 127.0.0.1 that has svc as an
// allowed peer. svc does not know it yet.
func newPeer(t *testing.T, svc *Service) *Service {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := New(testDB(t), Options{Domain: ln.Addr().String(), Mode: "allowlist", InsecureHTTP: true,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	allow(t, peer, svc.id)
	r := chi.NewRouter()
	peer.PublicRoutes(r)
	srv := httptest.NewUnstartedServer(r)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return peer
}

// fakePeer is an allowed peer of svc on 127.0.0.1 that answers with h.
func fakePeer(t *testing.T, svc *Service, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	domain := srv.Listener.Addr().String()
	allow(t, svc, testIdentity(t, domain))
	return domain
}

// botDo serves a /v1 GET with claims c on the routes the server mounts
// there: the local bots API and BotRoutes. A nil svc is federation off.
func botDo(svc *Service, db *sql.DB, c auth.Claims, path string) *httptest.ResponseRecorder {
	store := bots.NewStore(db)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, c)))
			})
		})
		store.Routes(r)
		BotRoutes(r, store, svc)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func lookup(svc *Service, db *sql.DB, c auth.Claims, handle string) *httptest.ResponseRecorder {
	return botDo(svc, db, c, "/v1/bots/lookup?handle="+url.QueryEscape(handle))
}

func entryOf(t *testing.T, rec *httptest.ResponseRecorder) entry {
	t.Helper()
	var e entry
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &e) != nil {
		t.Fatalf("want an entry, got %d %s", rec.Code, rec.Body)
	}
	return e
}

// addresses returns the address of each directory entry, with a
// trailing " remote" for remote ones.
func addresses(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct{ Bots []entry }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Bots == nil {
		t.Fatalf("want a directory, got %d %s", rec.Code, rec.Body)
	}
	got := []string{}
	for _, e := range out.Bots {
		if e.Remote {
			e.Address += " remote"
		}
		got = append(got, e.Address)
	}
	return got
}

func wantList(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The federation bot endpoints answer only signed requests of a peer
// the policy lets in.
func TestFedBots_SignedOnly(t *testing.T) {
	svc := newLocal(t, "allowlist")
	addBot(t, svc.db, "alice", bots.Peers)
	stranger := testIdentity(t, "c.example")
	for _, path := range []string{"/fed/v1/bots/alice", "/fed/v1/directory"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = local
		wantErr(t, serve(svc, req), http.StatusUnauthorized, "bad_signature")
		wantErr(t, serve(svc, signedGet(t, stranger, path)), http.StatusForbidden, "peer_not_allowed")
	}
	peer := testIdentity(t, "a.example")
	allow(t, svc, peer)
	rec := serve(svc, signedGet(t, peer, "/fed/v1/bots/alice"))
	var p bots.Profile
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.Handle != "alice" || p.DisplayName != "Bot alice" ||
		p.Visibility != bots.Peers {
		t.Errorf("bot: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(svc, signedGet(t, peer, "/fed/v1/directory")); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"handle":"alice"`) {
		t.Errorf("directory: %d %s", rec.Code, rec.Body)
	}
}

// Private bots answer the same 404 as unknown names.
func TestFedBots_PrivateIs404(t *testing.T) {
	svc := newLocal(t, "allowlist")
	addBot(t, svc.db, "secret", bots.Private)
	addBot(t, svc.db, "pub", bots.Public)
	peer := testIdentity(t, "a.example")
	allow(t, svc, peer)
	for _, name := range []string{"secret", "nobody", "Bad!"} {
		wantErr(t, serve(svc, signedGet(t, peer, "/fed/v1/bots/"+name)), http.StatusNotFound, "not_found")
	}
	if rec := serve(svc, signedGet(t, peer, "/fed/v1/bots/pub")); rec.Code != 200 {
		t.Errorf("public bot: %d %s", rec.Code, rec.Body)
	}
}

// The directory lists peers and public bots, 100 a page, by handle.
func TestFedDirectory_Paging(t *testing.T) {
	svc := newLocal(t, "allowlist")
	for i := range 101 {
		addBot(t, svc.db, fmt.Sprintf("bot%03d", i), []string{bots.Peers, bots.Public}[i%2])
	}
	addBot(t, svc.db, "aaa-private", bots.Private)
	peer := testIdentity(t, "a.example")
	allow(t, svc, peer)
	page := func(q string) ([]bots.Profile, *int) {
		t.Helper()
		rec := serve(svc, signedGet(t, peer, "/fed/v1/directory"+q))
		var out struct {
			Bots []bots.Profile
			Next *int
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Bots == nil {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		return out.Bots, out.Next
	}
	b, next := page("")
	if len(b) != 100 || b[0].Handle != "bot000" || b[99].Handle != "bot099" || next == nil || *next != 2 {
		t.Errorf("page 1: %d bots, next %v", len(b), next)
	}
	if b, next = page("?page=2"); len(b) != 1 || b[0].Handle != "bot100" || next != nil {
		t.Errorf("page 2: %+v, next %v", b, next)
	}
	if b, next = page("?page=3"); len(b) != 0 || next != nil {
		t.Errorf("page 3: %+v, next %v", b, next)
	}
	for _, q := range []string{"?page=0", "?page=-1", "?page=x"} {
		wantErr(t, serve(svc, signedGet(t, peer, "/fed/v1/directory"+q)), http.StatusBadRequest, "bad_request")
	}
}

// A lookup of a bare handle or of our own domain is a local lookup,
// with the visibility rules of GET /v1/bots/{handle}.
func TestLookup_OwnDomain(t *testing.T) {
	svc := newLocal(t, "allowlist")
	addBot(t, svc.db, "alice", bots.Peers)
	addBot(t, svc.db, "secret", bots.Private)
	for _, h := range []string{"alice", "alice@b.example", "ALICE@B.Example"} {
		if e := entryOf(t, lookup(svc, svc.db, plainKey, h)); e.Handle != "alice" || e.Address != "alice@b.example" || e.Remote ||
			e.DisplayName != "Bot alice" {
			t.Errorf("%s: %+v", h, e)
		}
	}
	wantErr(t, lookup(svc, svc.db, plainKey, "secret"), http.StatusNotFound, "not_found")
	wantErr(t, lookup(svc, svc.db, plainKey, "nobody@b.example"), http.StatusNotFound, "not_found")
	if e := entryOf(t, lookup(svc, svc.db, adminKey, "secret")); e.Visibility != bots.Private {
		t.Errorf("admin: %+v", e)
	}
	for _, h := range []string{"", "bad handle", "@b.example"} {
		wantErr(t, lookup(svc, svc.db, plainKey, h), http.StatusBadRequest, "bad_request")
	}
	for _, h := range []string{"alice@bad..example", "alice@"} {
		wantErr(t, lookup(svc, svc.db, plainKey, h), http.StatusBadRequest, "invalid_domain")
	}
}

// A lookup on an allowed peer fetches the bot from it, signed.
func TestLookup_Peer(t *testing.T) {
	svc := newLocal(t, "allowlist")
	peer := newPeer(t, svc)
	allow(t, svc, peer.id)
	addBot(t, peer.db, "bob", bots.Public)
	addBot(t, peer.db, "hidden", bots.Private)
	d := peer.id.Domain
	if e := entryOf(t, lookup(svc, svc.db, plainKey, "bob@"+d)); e.Handle != "bob" || e.Address != "bob@"+d || !e.Remote ||
		e.DisplayName != "Bot bob" || e.Visibility != bots.Public {
		t.Errorf("bob: %+v", e)
	}
	wantErr(t, lookup(svc, svc.db, plainKey, "hidden@"+d), http.StatusNotFound, "not_found")
	wantErr(t, lookup(svc, svc.db, plainKey, "nobody@"+d), http.StatusNotFound, "not_found")
}

// In allowlist mode only allowed peers are asked; blocked ones never.
func TestLookup_PeerNotAllowed(t *testing.T) {
	svc := newLocal(t, "allowlist")
	peer := newPeer(t, svc)
	addBot(t, peer.db, "bob", bots.Peers)
	d := peer.id.Domain
	wantErr(t, lookup(svc, svc.db, plainKey, "bob@"+d), http.StatusForbidden, "peer_not_allowed")
	wantErr(t, lookup(svc, svc.db, plainKey, "bob@unknown.example"), http.StatusForbidden, "peer_not_allowed")
	allow(t, svc, peer.id)
	if err := svc.peers.Block(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	wantErr(t, lookup(svc, svc.db, plainKey, "bob@"+d), http.StatusForbidden, "peer_not_allowed")
}

// In open mode a lookup on a domain that is not allowed spends the
// budget of fetches for strangers.
func TestLookup_OpenModeBudget(t *testing.T) {
	svc := newLocal(t, "open")
	for svc.fetch.limit.Allow() {
	}
	wantErr(t, lookup(svc, svc.db, plainKey, "bob@unknown.example"), http.StatusTooManyRequests, "rate_limited")
}

// With federation off, a bare handle is a local lookup and any domain
// is another server's: 404 federation_disabled. The directory is local.
func TestLookup_FederationOff(t *testing.T) {
	db := testDB(t)
	addBot(t, db, "alice", bots.Peers)
	if e := entryOf(t, lookup(nil, db, plainKey, "alice")); e.Handle != "alice" || e.Address != "alice" || e.Remote {
		t.Errorf("alice: %+v", e)
	}
	wantErr(t, lookup(nil, db, plainKey, "nobody"), http.StatusNotFound, "not_found")
	for _, h := range []string{"alice@x.example", "alice@b.example", "alice@"} {
		wantErr(t, lookup(nil, db, plainKey, h), http.StatusNotFound, "federation_disabled")
	}
	if _, err := db.Exec(`INSERT INTO remote_bots(domain, handle, profile, fetched_at)
		VALUES ('a.example', 'bob', '{"handle":"bob"}', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	wantList(t, addresses(t, botDo(nil, db, plainKey, "/v1/directory")), "alice")
}

// A remote answer is signed for, checked and cleaned: an answer for
// another bot or another server is refused, as is one over 256 KiB.
func TestLookup_RemoteAnswerChecked(t *testing.T) {
	svc := newLocal(t, "allowlist")
	answers := map[string]string{
		"/fed/v1/bots/bob":  `{"handle":"bob","display_name":"‮Bob\u0000","description":"hi​","skills":[{"id":"x","name":"X"},{"id":"Bad Id"}],"visibility":"peers"}`,
		"/fed/v1/bots/wide": `{"handle":"wide","description":"` + strings.Repeat("a", 100<<10) + `"}`,
		"/fed/v1/bots/big":  `{"handle":"big","description":"` + strings.Repeat("a", 300<<10) + `"}`,
		"/fed/v1/bots/eve":  `{"handle":"mallory"}`,
		"/fed/v1/bots/carl": `{"handle":"carl@other.example"}`,
		"/fed/v1/bots/junk": `not json`,
	}
	d := fakePeer(t, svc, func(w http.ResponseWriter, r *http.Request) {
		p, err := ParseSigHeader(r.Header.Get("Authorization"))
		if err != nil || p.Origin != local || p.KeyID != svc.id.KeyID ||
			!ed25519.Verify(svc.id.Public, SigningString(r.Method, r.RequestURI, r.Host, local, p.TS, p.Nonce, nil), p.Sig) {
			http.Error(w, "unsigned", http.StatusUnauthorized)
			return
		}
		body, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	if e := entryOf(t, lookup(svc, svc.db, plainKey, "bob@"+d)); e.DisplayName != "Bob" || e.Description != "hi" ||
		len(e.Skills) != 1 || e.Skills[0].ID != "x" || e.Address != "bob@"+d || !e.Remote {
		t.Errorf("bob: %+v", e)
	}
	if e := entryOf(t, lookup(svc, svc.db, plainKey, "wide@"+d)); len(e.Description) != 1000 {
		t.Errorf("wide: description of %d bytes", len(e.Description))
	}
	for _, name := range []string{"big", "eve", "carl", "junk"} {
		wantErr(t, lookup(svc, svc.db, plainKey, name+"@"+d), http.StatusBadGateway, "peer_unreachable")
	}
	wantErr(t, lookup(svc, svc.db, plainKey, "nobody@"+d), http.StatusNotFound, "not_found")
}

// A refresh caches a peer's peers and public bots; the directory lists
// the visible local bots, then the cached ones, each with its address.
func TestDirectory_Refresh(t *testing.T) {
	ctx := context.Background()
	svc := newLocal(t, "allowlist")
	addBot(t, svc.db, "alice", bots.Peers)
	addBot(t, svc.db, "secret", bots.Private)
	peer := newPeer(t, svc)
	allow(t, svc, peer.id)
	addBot(t, peer.db, "bob", bots.Peers)
	addBot(t, peer.db, "pub", bots.Public)
	addBot(t, peer.db, "hidden", bots.Private)
	d := peer.id.Domain
	if err := svc.refreshDirectory(ctx, d); err != nil {
		t.Fatal(err)
	}
	wantList(t, addresses(t, botDo(svc, svc.db, plainKey, "/v1/directory")),
		"alice@b.example", "bob@"+d+" remote", "pub@"+d+" remote")
	wantList(t, addresses(t, botDo(svc, svc.db, adminKey, "/v1/directory")),
		"alice@b.example", "secret@b.example", "bob@"+d+" remote", "pub@"+d+" remote")

	// The next refresh replaces the rows.
	priv := bots.Private
	if _, err := peer.botStore.Update(ctx, "bob", bots.Patch{Visibility: &priv}, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.refreshDirectory(ctx, d); err != nil {
		t.Fatal(err)
	}
	wantList(t, addresses(t, botDo(svc, svc.db, plainKey, "/v1/directory")), "alice@b.example", "pub@"+d+" remote")
}

// A refresh reads at most 10 pages, drops entries with an invalid
// handle or another server's, and cleans the text. A failed refresh
// keeps the rows.
func TestDirectory_RefreshChecksEntries(t *testing.T) {
	ctx := context.Background()
	svc := newLocal(t, "allowlist")
	var pages atomic.Int32
	var failing atomic.Bool
	d := fakePeer(t, svc, func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		if failing.Load() || r.URL.Path != "/fed/v1/directory" || r.URL.Query().Get("page") != strconv.Itoa(int(n)) {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"bots":[{"handle":"p%d","display_name":"‮P\u0000"},{"handle":"x@other.example"},{"handle":"Bad Handle"}],"next":%d}`, n, n+1)
	})
	if err := svc.refreshDirectory(ctx, d); err != nil {
		t.Fatal(err)
	}
	if n := pages.Load(); n != 10 {
		t.Errorf("pages fetched: %d, want 10", n)
	}
	got := addresses(t, botDo(svc, svc.db, plainKey, "/v1/directory"))
	if len(got) != 10 || got[0] != "p1@"+d+" remote" {
		t.Errorf("directory: %q", got)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM remote_bots WHERE domain = ? AND json_extract(profile, '$.display_name') = 'P'`, d); n != 10 {
		t.Errorf("cleaned rows: %d", n)
	}
	failing.Store(true)
	if err := svc.refreshDirectory(ctx, d); err == nil {
		t.Error("refresh from a failing peer succeeded")
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM remote_bots WHERE domain = ?`, d); n != 10 {
		t.Errorf("rows after a failed refresh: %d, want 10", n)
	}
}

// A peer blocked while its directory is fetched gets no rows.
func TestDirectory_RefreshAfterBlock(t *testing.T) {
	svc := newLocal(t, "allowlist")
	d := fakePeer(t, svc, func(w http.ResponseWriter, r *http.Request) {
		_ = svc.peers.Block(context.Background(), r.Host)
		_, _ = io.WriteString(w, `{"bots":[{"handle":"p"}],"next":null}`)
	})
	_ = svc.refreshDirectory(context.Background(), d)
	if n := count(t, svc.db, `SELECT COUNT(*) FROM remote_bots`); n != 0 {
		t.Errorf("rows of a blocked peer: %d", n)
	}
}

// Blocking or removing a peer deletes its cached bots.
func TestDirectory_DeletedOnBlockAndRemove(t *testing.T) {
	svc := newLocal(t, "allowlist")
	for _, d := range []string{"a.example", "c.example"} {
		allow(t, svc, testIdentity(t, d))
		if _, err := svc.db.Exec(`INSERT INTO remote_bots(domain, handle, profile, fetched_at)
			VALUES (?, 'bob', '{"handle":"bob"}', '2026-01-01T00:00:00Z')`, d); err != nil {
			t.Fatal(err)
		}
	}
	if rec := adminDo(svc, admin, "POST", "/v1/federation/peers/a.example/block", ""); rec.Code != 200 {
		t.Fatalf("block: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM remote_bots WHERE domain = 'a.example'`); n != 0 {
		t.Errorf("rows after block: %d", n)
	}
	if rec := adminDo(svc, admin, "DELETE", "/v1/federation/peers/c.example", ""); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM remote_bots`); n != 0 {
		t.Errorf("rows after remove: %d", n)
	}
}

// Run refreshes a peer's directory right after the peer is added, and
// stops with its context.
func TestDirectory_RefreshAfterAdd(t *testing.T) {
	svc := newLocal(t, "allowlist")
	peer := newPeer(t, svc)
	addBot(t, peer.db, "bob", bots.Peers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()

	add := `{"domain":"` + peer.id.Domain + `","fingerprint":"` + peer.id.Fingerprint() + `"}`
	if rec := adminDo(svc, admin, "POST", "/v1/federation/peers", add); rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); count(t, svc.db,
		`SELECT COUNT(*) FROM remote_bots WHERE domain = ? AND handle = 'bob'`, peer.id.Domain) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no refresh after the peer was added")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}
