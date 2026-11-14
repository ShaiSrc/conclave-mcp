package federation

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// Fuzz targets for every parser that reads another server's bytes, and
// for the public pages and feed, which show text that may come from
// elsewhere. go test runs the seeds; go test -fuzz explores. A target
// builds its server once per process, so state carries over from one
// input to the next: each property must hold in any state the inputs
// can reach.

func FuzzParseSigHeader(f *testing.F) {
	id := testIdentity(f, "a.example")
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/ping", nil)
	if err := id.SignRequest(req, local, []byte("{}")); err != nil {
		f.Fatal(err)
	}
	f.Add(req.Header.Get("Authorization"))
	sig := b64.EncodeToString(make([]byte, 64))
	f.Add(`Conclave-Sig-v1 sig="` + sig + `",nonce="AAAAAAAAAAAAAAAAAAAAAA",ts="1700000000",key="ed25519:abc",origin="a,key="`)
	f.Add(`Conclave-Sig-v1 origin="a\.example",key="",ts="01",nonce="AAAAAAAAAAAAAAAAAAAAAA",sig=""`)
	f.Add("Bearer abc")
	f.Fuzz(func(t *testing.T, h string) {
		p, err := ParseSigHeader(h)
		if err != nil {
			return
		}
		again, err := ParseSigHeader(formatSigHeader(p))
		if err != nil || !reflect.DeepEqual(again, p) {
			t.Fatalf("%q parses to %+v; formatted and parsed again: %+v, %v", h, p, again, err)
		}
	})
}

// fuzzRoutes are the /fed/v1 routes that take a body. {hosted} is a
// room of this server with a bot of the peer, {replica} the peer's
// room with a bot of ours and {new} a room id never seen.
var fuzzRoutes = []string{
	"/fed/v1/ping",
	"/fed/v1/rooms/{hosted}/messages",
	"/fed/v1/rooms/{replica}/invite",
	"/fed/v1/rooms/{new}/invite",
	"/fed/v1/rooms/{replica}/events",
	"/fed/v1/rooms/{hosted}/leave",
	"/fed/v1/rooms/{hosted}/claims?author=m1@" + peerDomain,
	"/fed/v1/rooms/{hosted}/claims/heartbeat?author=m1@" + peerDomain,
	"/fed/v1/rooms/{hosted}/claims/release?author=m1@" + peerDomain,
	"/fed/v1/follows",
	"/fed/v1/follows/withdraw",
}

const (
	routeMessages = 1
	routeLeave    = 5
)

func fuzzPath(route uint8, hosted, replica string) (int, string) {
	i := int(route) % len(fuzzRoutes)
	return i, strings.NewReplacer("{hosted}", hosted, "{replica}", replica, "{new}", ulid.Make().String()).Replace(fuzzRoutes[i])
}

// FuzzVerifyRequest sends requests with any Authorization header, Host
// and body to a server with one allowed peer, whose key the fuzzer does
// not have: none gets through, and none fails inside.
func FuzzVerifyRequest(f *testing.F) {
	svc := newLocal(f, "allowlist")
	svc.fetch.client.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no network while fuzzing")
	}
	peer := testIdentity(f, "a.example")
	allow(f, svc, peer)
	r := chi.NewRouter()
	svc.PublicRoutes(r)
	header := func(id *Identity, keyID, path, body string) string {
		const ts, nonce = 1700000000, "AAAAAAAAAAAAAAAAAAAAAA" // a valid header, but from 2023
		sig := id.Sign(SigningString(http.MethodPost, path, local, id.Domain, ts, nonce, []byte(body)))
		return formatSigHeader(SigParams{Origin: id.Domain, KeyID: keyID, TS: ts, Nonce: nonce, Sig: sig})
	}
	f.Add(header(peer, peer.KeyID, "/fed/v1/ping", "{}"), local, uint8(0), []byte("{}"))
	f.Add(header(peer, "ed25519:AAAAAAAAAAAAAAAA", "/fed/v1/ping", "{}"), local, uint8(0), []byte("{}")) // another key: re-pinning
	f.Add(header(testIdentity(f, "c.example"), "ed25519:x", "/fed/v1/follows", "{}"), local, uint8(9), []byte("{}"))
	f.Add("", local, uint8(4), []byte(`{"events":[]}`))
	f.Add(`Conclave-Sig-v1 origin="a.example",key="k",ts="1",nonce="n",sig="s"`, "B.example:443", uint8(1), []byte(nil))
	f.Fuzz(func(t *testing.T, auth, host string, route uint8, body []byte) {
		_, path := fuzzPath(route, ulid.Make().String(), ulid.Make().String())
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Host = host
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code/100 == 2 || rec.Code == http.StatusInternalServerError {
			t.Fatalf("Authorization %q, Host %q, %s: %d %s", auth, host, path, rec.Code, rec.Body)
		}
	})
}

// FuzzDocument decodes and checks a server document as the fetcher
// does, then judges its newest key as re-pinning does, endorsement
// included.
func FuzzDocument(f *testing.F) {
	ctx := context.Background()
	svc := newLocal(f, "allowlist")
	peerDB := testDB(f)
	peer, err := LoadOrCreateIdentity(ctx, peerDB, "a.example")
	if err != nil {
		f.Fatal(err)
	}
	allow(f, svc, peer)
	pinned, err := svc.peers.Get(ctx, peer.Domain)
	if err != nil {
		f.Fatal(err)
	}
	rotated, err := Rotate(ctx, peerDB, peer, time.Now()) // its current key is endorsed by the pinned one
	if err != nil {
		f.Fatal(err)
	}
	for _, doc := range []Document{peer.Document(time.Now()), rotated.Document(time.Now())} {
		b, _ := json.Marshal(doc)
		f.Add(b)
	}
	f.Add([]byte(`{"protocol":"conclave-federation/1","domain":"a.example","keys":[]}`))
	f.Add([]byte(`{"protocol":"conclave-federation/1","domain":"a.example","keys":[{"key_id":"ed25519:x","alg":"ed25519","public_key":"AAAA"}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		doc, err := parseDocument(body, peer.Domain)
		if err != nil {
			return
		}
		for _, k := range doc.Keys {
			if keyIn(doc, k.KeyID) == nil || keyIn(doc, k.Fingerprint) == nil {
				t.Fatalf("key %q of a checked document is not found by its ID or fingerprint", k.KeyID)
			}
		}
		k := doc.Keys[0]
		pub, err := svc.adopt(ctx, pinned, k)
		if err != nil {
			t.Fatal(err)
		}
		if pub == nil {
			return
		}
		var sig []byte
		if k.EndorsedBy != nil {
			sig, _ = decodeB64(k.EndorsedBy.Sig)
		}
		pinnedID := KeyIDFor(pinned.PublicKey)
		if len(sig) != ed25519.SignatureSize || k.EndorsedBy.KeyID != pinnedID || KeyIDFor(pub) != k.KeyID ||
			!ed25519.Verify(pinned.PublicKey, EndorsementString(peer.Domain, pinnedID, k.KeyID, b64.EncodeToString(pub)), sig) {
			t.Fatalf("adopted key %q without the pinned key's endorsement", k.KeyID)
		}
	})
}

// FuzzAddress parses name@domain as the admin and invite paths do. A
// canonical address parses to itself, and every bot handle is a remote
// name too.
func FuzzAddress(f *testing.F) {
	for _, s := range []string{"b1@a.example", "B1@A.Example:7433", "operator@a.example", "key-12@127.0.0.1:7433",
		"admin@a.example", "a@b@c", "@a.example", "b1@", "b1", "\u212Aelvin@a.example", "b1@a.example:443"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		name, domain, _ := strings.Cut(s, "@")
		h, herr := bots.NormalizeRemoteName(name)
		d, derr := NormalizeDomain(domain)
		if hh, err := bots.NormalizeHandle(name); err == nil && (herr != nil || hh != h) {
			t.Fatalf("handle %q is %q, but as a remote name %q, %v", name, hh, h, herr)
		}
		if herr != nil || derr != nil {
			return
		}
		addr := h + "@" + d
		name, domain, _ = strings.Cut(addr, "@")
		h2, herr := bots.NormalizeRemoteName(name)
		d2, derr := NormalizeDomain(domain)
		if herr != nil || derr != nil || h2+"@"+d2 != addr {
			t.Fatalf("%q is %q, which parses to %q@%q, %v, %v", s, addr, h2, d2, herr, derr)
		}
	})
}

// FuzzAuthorSig checks random message fields and signatures against a
// key: verification never panics, and nothing verifies but the key's
// own signature, which Ed25519 makes deterministic.
func FuzzAuthorSig(f *testing.F) {
	id := testIdentity(f, peerDomain)
	m := MessageSig{Room: "01ROOM@b.example", Author: "m1@m.example", ContentType: "text/plain", MessageType: "answer",
		Content: "hi", Metadata: []byte(`{"k":1}`), Nonce: "AAAAAAAAAAAAAAAAAAAAAA"}
	sig := id.SignMessage(m)
	f.Add(m.Room, m.Author, m.ContentType, m.MessageType, m.Content, m.Metadata, int64(41), true, m.Nonce, sig, []byte(id.Public))
	f.Add(m.Room, m.Author, m.ContentType, m.MessageType, m.Content, m.Metadata, int64(0), false, m.Nonce, sig, []byte(id.Public))
	f.Add("", "", "", "", "", []byte(nil), int64(0), false, "", "", []byte(nil))
	f.Add(m.Room, m.Author, m.ContentType, m.MessageType, m.Content, m.Metadata, int64(0), false, m.Nonce+"\n", sig+"\r", []byte("short"))
	f.Fuzz(func(t *testing.T, room, author, contentType, messageType, content string, metadata []byte, replyTo int64, reply bool,
		nonce, sig string, pub []byte) {
		m := MessageSig{Room: room, Author: author, ContentType: contentType, MessageType: messageType, Content: content,
			Metadata: metadata, Nonce: nonce}
		if reply {
			m.ReplyTo = &replyTo
		}
		VerifyMessage(pub, m, sig) // any key: no panic
		if VerifyMessage(id.Public, m, sig) && sig != id.SignMessage(m) {
			t.Fatalf("%+v: signature %q verifies, but the key signs %q", m, sig, id.SignMessage(m))
		}
	})
}

// peerDomain is the peer of a fuzzRig. Every request of the rig goes to
// a local server, so the domain stays the same across runs and inputs
// can name the peer's bots.
const peerDomain = "m.example"

// fuzzRig is a server with an allowed peer whose bot m1 is a member of
// the room hosted here and whose replica has our bot b1. The peer's
// server answers every request with answer, or else with 201, naming an
// invite's kind back.
type fuzzRig struct {
	g               *roomRig
	peer            *Identity
	b1              int64
	hosted, replica string

	mu sync.Mutex
	// answer returns the status and body the peer answers a request
	// with body req; a status of 0 drops the connection.
	answer func(req []byte) (int, []byte)
}

func newFuzzRig(tb testing.TB) *fuzzRig {
	tb.Helper()
	ctx := context.Background()
	g := newRoomRig(tb)
	rg := &fuzzRig{g: g, peer: testIdentity(tb, peerDomain), replica: ulid.Make().String()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := io.ReadAll(r.Body)
		rg.mu.Lock()
		answer := rg.answer
		rg.mu.Unlock()
		if answer == nil {
			var in struct{ Kind string }
			_ = json.Unmarshal(req, &in)
			writeJSON(w, http.StatusCreated, map[string]string{"kind": in.Kind})
			return
		}
		status, out := answer(req)
		if status == 0 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
	tb.Cleanup(srv.Close)
	g.svc.fetch.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	allow(tb, g.svc, rg.peer)
	// The fuzzer sends far more than a peer may; re-pin fetches keep
	// their limit.
	lims, err := g.svc.limitersOf(ctx, peerDomain)
	if err != nil {
		tb.Fatal(err)
	}
	inf := rate.NewLimiter(rate.Inf, 0)
	lims.requests, lims.dmInvites, lims.followRequests = inf, inf, inf

	rg.b1 = g.bot(tb, "b1")
	open, peers := bots.FollowOpen, bots.DMPeers
	if _, err := bots.NewStore(g.db).Update(ctx, "b1", bots.Patch{FollowPolicy: &open, DMPolicy: &peers}, true); err != nil {
		tb.Fatal(err)
	}
	rg.hosted = g.room(tb, "hosted")
	g.invite(tb, rg.hosted, "m1@"+peerDomain, "read,answer,claim")
	if rec := rg.send(tb, "/fed/v1/rooms/"+rg.replica+"/invite", []byte(`{"name":"proj","handle":"b1","rights":["read","answer"]}`)); rec.Code != http.StatusCreated {
		tb.Fatalf("replica invite: %d %s", rec.Code, rec.Body)
	}
	return rg
}

// send is a POST of body to path, signed by the peer.
func (rg *fuzzRig) send(tb testing.TB, path string, body []byte) *httptest.ResponseRecorder {
	tb.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Host = local
	if err := rg.peer.SignRequest(req, local, body); err != nil {
		tb.Fatal(err)
	}
	return rg.g.fedDo(req)
}

// authorSign sets the author signature of a message proposal as the
// peer's server would, so inputs get past it to the send path. A body
// that is not a JSON object goes as it is.
func (rg *fuzzRig) authorSign(body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return body
	}
	delete(obj, "sig")
	var p struct {
		Author, Content, Nonce string
		ContentType            string `json:"content_type"`
		MessageType            string `json:"message_type"`
		Metadata               json.RawMessage
		ReplyTo                *int64 `json:"reply_to"`
	}
	if json.Unmarshal(rawJSON(obj), &p) != nil {
		return body
	}
	wire, _, err := fedrooms.SplitMetadata(p.Metadata)
	if err != nil {
		return body
	}
	obj["sig"] = rawJSON(rg.peer.SignMessage(MessageSig{Room: rg.hosted + "@" + local, Author: p.Author,
		ContentType: cmp.Or(p.ContentType, "text/plain"), MessageType: cmp.Or(p.MessageType, rights.System),
		Content: p.Content, Metadata: wire, ReplyTo: p.ReplyTo, Nonce: p.Nonce}))
	return rawJSON(obj)
}

// FuzzInbound sends any body, signed by the peer, to each /fed/v1 route
// that takes one: it passes verification and the handler never answers
// 5xx. A message proposal gets its author signature too.
func FuzzInbound(f *testing.F) {
	rg := newFuzzRig(f)
	m1 := "m1@" + peerDomain
	for route, bodies := range [][]string{
		{`{}`, ``},
		{`{"author":"` + m1 + `","content":"hi","content_type":"text/plain","message_type":"answer","nonce":"AAAAAAAAAAAAAAAAAAAAAA","metadata":{"k":[1,{"b":2,"a":1}]},"reply_to":1}`,
			`{"author":"` + m1 + `","content":"<b>","nonce":"BAAAAAAAAAAAAAAAAAAAAA","metadata":{"reply_to":1}}`},
		{`{"name":"proj","handle":"b1","rights":["read","answer"]}`, `{"name":"renamed","handle":"B1","rights":["all"],"kind":"room"}`},
		{`{"name":"feed:` + m1 + `","handle":"b1","rights":["read"],"kind":"feed","feed":"` + m1 + `","seq":3}`,
			`{"name":"dm:b1@` + local + `~` + m1 + `","handle":"b1","rights":[],"kind":"direct"}`},
		{`{"events":[{"seq":1,"type":"room","payload":{"name":"proj"}},{"seq":2,"type":"member","payload":{"address":"m2@m.example","rights":["read"]}}]}`},
		{`{"author":"` + m1 + `"}`},
		{`{"paths":["x/"],"ttl_seconds":60,"note":"n"}`},
		{`{"ttl_seconds":60}`},
		{`{"paths":["x/"]}`},
		{`{"follower":"` + m1 + `","handle":"b1"}`},
		{`{"follower":"` + m1 + `","handle":"b1"}`},
	} {
		for _, b := range bodies {
			f.Add(uint8(route), []byte(b))
		}
	}
	f.Fuzz(func(t *testing.T, route uint8, body []byte) {
		i, path := fuzzPath(route, rg.hosted, rg.replica)
		if i == routeMessages {
			body = rg.authorSign(body)
		}
		rec := rg.send(t, path, body)
		if rec.Code >= 500 || rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s %q: %d %s", path, body, rec.Code, rec.Body)
		}
		if i == routeLeave && rec.Code == http.StatusNoContent {
			rg.g.invite(t, rg.hosted, m1, "read,answer,claim") // back for the next input
		}
	})
}

// FuzzApplyEvents pushes two events of any type and payload to the
// replica, numbered on from what it applied: each is applied or
// refused, never a 5xx, and the replica moves past both.
func FuzzApplyEvents(f *testing.F) {
	rg := newFuzzRig(f)
	msg := func(id int64, author, extra string) []byte {
		return []byte(`{"id":` + strconv.FormatInt(id, 10) + `,"author":"` + author + `","ts":"2026-11-13T00:00:00.000000000Z",` +
			`"content_type":"text/plain","message_type":"answer","content":"hi"` + extra + `}`)
	}
	f.Add("room", []byte(`{"name":"renamed"}`), "room", []byte(`{"name":"proj","archived":true}`))
	f.Add("room", []byte(`{"name":"dm:x","kind":"direct"}`), "skip", []byte(`null`))
	f.Add("member", []byte(`{"address":"m2@m.example","rights":["read","answer"]}`), "member.remove", []byte(`{"address":"m2@m.example"}`))
	f.Add("member", []byte(`{"address":"b1@b.example","rights":["read"]}`), "member", []byte(`{"address":"operator@b.example","rights":["all"]}`))
	f.Add("member.remove", []byte(`{"address":"b1@b.example"}`), "member", []byte(`{"address":"key-1@b.example","rights":[]}`))
	f.Add("message", msg(1, "m1@m.example", `,"metadata":{"k":1}`), "message.delete", []byte(`{"id":1}`))
	f.Add("message", msg(2, "c1@c.example", `,"nonce":"AAAAAAAAAAAAAAAAAAAAAA","origin_sig":"x"`), "message", msg(3, "m1@m.example", `,"reply_to":2`))
	f.Add("message", msg(4, "b1@b.example", `,"nonce":"AAAAAAAAAAAAAAAAAAAAAA","origin_sig":"x"`), "message", msg(5, "m1@m.example", `,"deleted":true`))
	f.Add("message.delete", []byte(`{"id":0}`), "future.type", []byte(`{"x":1}`))
	f.Fuzz(func(t *testing.T, type1 string, payload1 []byte, type2 string, payload2 []byte) {
		if !json.Valid(payload1) || !json.Valid(payload2) {
			return // the body would not decode: FuzzInbound covers that
		}
		var applied int64
		if err := rg.g.db.QueryRow(`SELECT applied_seq FROM rooms WHERE id = ?`, rg.replica).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		body := rawJSON(map[string]any{"events": []fedrooms.Event{{Seq: applied + 1, Type: type1, Payload: payload1},
			{Seq: applied + 2, Type: type2, Payload: payload2}}})
		rec := rg.send(t, "/fed/v1/rooms/"+rg.replica+"/events", body)
		var out struct{ Applied int64 }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Applied != applied+2 {
			t.Fatalf("%s: %d %s, want 200 applied %d", body, rec.Code, rec.Body, applied+2)
		}
	})
}

// peerCalls are the outgoing calls that parse a peer's answer, by
// FuzzPeerAnswers' selector.
var peerCalls = []string{"directory", "lookup", "invite room", "invite direct", "invite feed", "forward message",
	"claims", "claim", "claims heartbeat", "claims release", "pull", "follow", "follow withdraw"}

// wantRefusal fails unless err is nil or a refusal our caller answers
// with its own status: a *fedrooms.Error of a 4xx or 502. Any other
// error is a 500 there.
func wantRefusal(t *testing.T, call string, err error) {
	t.Helper()
	fe := (*fedrooms.Error)(nil)
	if err != nil && (!errors.As(err, &fe) || fe.Status < 400 || fe.Status >= 500 && fe.Status != http.StatusBadGateway) {
		t.Fatalf("%s: %v (%T): a 500 for our caller", call, err, err)
	}
}

// FuzzPeerAnswers gives each outgoing call that parses a peer's answer
// any status and body as that answer; {sig} and {nonce} in the body
// become those of the request, so an answer can echo them. Nothing
// panics, our caller never gets a 500, and an answer that fails its
// checks stores nothing.
func FuzzPeerAnswers(f *testing.F) {
	ctx := context.Background()
	rg := newFuzzRig(f)
	g, svc := rg.g, rg.g.svc
	// Rooms of each kind hosted here, to invite the peer's bot into.
	direct, feed := ulid.Make().String(), ulid.Make().String()
	if _, err := g.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind, feed) VALUES
		(?, 'dm:b1@b.example~m1@m.example', ?, '2026-01-01T00:00:00Z', 'private', 'direct', NULL),
		(?, 'feed:b1@b.example', ?, '2026-01-01T00:00:00Z', 'private', 'room', 'b1@b.example')`,
		direct, g.admin.KeyID, feed, rg.b1); err != nil {
		f.Fatal(err)
	}
	svc.keyLimiters[g.admin.KeyID] = rate.NewLimiter(rate.Inf, 0) // remote lookups
	api := chi.NewRouter()
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey{}, g.admin)))
		})
	})
	BotRoutes(api, bots.NewStore(g.db), svc)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	snapshot := func(t *testing.T, query string, args ...any) string {
		t.Helper()
		var s string
		if err := g.db.QueryRow(query, args...).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	const (
		botsOfPeer = `SELECT COALESCE(group_concat(handle || ' ' || profile, char(10)), '') FROM
			(SELECT handle, profile FROM remote_bots WHERE domain = 'm.example' ORDER BY handle)`
		replicaState = `SELECT applied_seq || ' ' || (SELECT COUNT(*) FROM messages WHERE room_id = rooms.id) FROM rooms WHERE id = ?`
		ourMessages  = `SELECT COUNT(*) FROM messages WHERE room_id = ? AND author_key_id = ?`
		ts           = `"ts":"2026-11-13T00:00:00.000000000Z"`
	)
	profile := `{"handle":"m1","display_name":"M1 ‮","description":"d\r\n<b>","skills":[{"id":"s1","name":"n","description":"d"},{"id":"BAD ID"}],"visibility":"public","updated_at":"2026-11-13T00:00:00+02:00"}`
	errBody := `{"error":{"code":"dm_refused","message":"no\nthanks"}}`
	for _, s := range []struct {
		call   string
		status int
		body   string
	}{
		{"directory", 200, `{"bots":[` + profile + `,{"handle":"admin"},{"handle":"m2@x.example"}],"next":null}`},
		{"directory", 200, `{"bots":[{"handle":"m3"}],"next":2}`},
		{"directory", 403, errBody},
		{"lookup", 200, profile},
		{"lookup", 200, `{"handle":"m2"}`},
		{"lookup", 404, errBody},
		{"invite room", 201, `{"kind":"room","max_events_bytes":589824}`},
		{"invite room", 409, `{"error":{"code":"name_taken"}}`},
		{"invite direct", 201, `{"kind":"direct","max_events_bytes":-1}`},
		{"invite direct", 429, errBody},
		{"invite feed", 201, `{"kind":"feed"}`},
		{"invite feed", 0, ``},
		{"forward message", 201, `{"seq":1,"type":"message","payload":{"id":7,"author":"b1@b.example",` + ts +
			`,"content_type":"text/plain","message_type":"answer","content":"hi","metadata":{"k":1},"origin_sig":"{sig}","nonce":"{nonce}"}}`},
		{"forward message", 201, `{"seq":9,"type":"message","payload":{"id":8,"author":"b1@b.example",` + ts +
			`,"content_type":"text/plain","message_type":"answer","content":"hi","metadata":{"k":1},"origin_sig":"{sig}","nonce":"{nonce}"}}`},
		{"forward message", 400, errBody},
		{"claims", 200, `{"claims":[{"id":1,"path":"x/","address":"b1@b.example","note":"n","expires_at":"2026-11-14T00:00:00Z"},` +
			`{"id":2,"path":"y/","address":"m1@m.example","auto":true,"expires_at":"2026-11-14T00:00:00+02:00"},{"id":"3","path":"../z"}]}`},
		{"claim", 409, `{"error":{"code":"conflict"},"conflicts":[{"path":"x/","held_path":"x/","address":"m1@m.example","expires_at":"2026-11-14T00:00:00Z"}]}`},
		{"claim", 201, `{"claims":[{"id":4,"path":"x/","address":"b1@b.example","expires_at":"2026-11-14T00:00:00Z"}]}`},
		{"claims heartbeat", 200, `{"refreshed":1}`},
		{"claims release", 200, `{"released":["x/","../bad",1]}`},
		{"pull", 200, `{"events":[{"seq":1,"type":"message","payload":{"id":11,"author":"m1@m.example",` + ts +
			`,"content_type":"text/plain","message_type":"answer","content":"pulled"}},{"seq":2,"type":"room","payload":{"name":"proj"}}]}`},
		{"pull", 200, `{"events":[{"seq":3,"type":"member","payload":{}}]}`},
		{"follow", 201, `{"status":"pending"}`},
		{"follow", 403, `{"error":{"code":"follow_refused"}}`},
		{"follow", 302, ``},
		{"follow withdraw", 204, ``},
		{"follow withdraw", 200, errBody},
	} {
		i := 0
		for peerCalls[i] != s.call {
			i++
		}
		f.Add(uint8(i), s.status, []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, call uint8, status int, body []byte) {
		if status != 0 && (status < 200 || status > 599) {
			return
		}
		rg.mu.Lock()
		rg.answer = func(req []byte) (int, []byte) {
			var in struct{ Sig, Nonce string }
			_ = json.Unmarshal(req, &in)
			return status, []byte(strings.NewReplacer("{sig}", in.Sig, "{nonce}", in.Nonce).Replace(string(body)))
		}
		rg.mu.Unlock()
		name := peerCalls[int(call)%len(peerCalls)]
		switch name {
		case "directory":
			before := snapshot(t, botsOfPeer)
			if err := svc.refreshDirectory(ctx, peerDomain); err != nil && snapshot(t, botsOfPeer) != before {
				t.Fatalf("a refused directory (%v) changed the cached bots", err)
			}
			if rec := get("/directory"); rec.Code != http.StatusOK {
				t.Fatalf("/v1/directory: %d %s", rec.Code, rec.Body)
			}
		case "lookup":
			rec := get("/bots/lookup?handle=m1@" + peerDomain)
			var e entry
			switch {
			case rec.Code >= 500 && rec.Code != http.StatusBadGateway:
				t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
			case rec.Code == http.StatusOK && (json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Handle != "m1" ||
				e.Address != "m1@"+peerDomain || !e.Remote):
				t.Fatalf("lookup answered another bot: %s", rec.Body)
			}
		case "invite room", "invite direct", "invite feed":
			room := map[string][2]string{"invite room": {rg.hosted, "hosted"},
				"invite direct": {direct, "dm:b1@b.example~m1@m.example"}, "invite feed": {feed, "feed:b1@b.example"}}[name]
			_, err := svc.InviteRemote(ctx, room[0], room[1], "m1@"+peerDomain, []string{rights.Read})
			wantRefusal(t, name, err)
		case "forward message":
			before := snapshot(t, ourMessages, rg.replica, rg.b1)
			_, err := svc.ForwardMessage(ctx, rg.replica, rg.b1, fedrooms.MessagePayload{ContentType: "text/plain",
				MessageType: "answer", Content: "hi", Metadata: []byte(`{"k":1}`)})
			wantRefusal(t, name, err)
			if err != nil && snapshot(t, ourMessages, rg.replica, rg.b1) != before {
				t.Fatalf("a refused forward (%v) stored the message", err)
			}
		case "claims", "claim", "claims heartbeat", "claims release":
			path := map[string]string{"claims": "/claims", "claim": "/claims", "claims heartbeat": "/claims/heartbeat",
				"claims release": "/claims/release"}[name]
			method, reqBody := http.MethodPost, `{"paths":["x/"]}`
			if name == "claims" {
				method, reqBody = http.MethodGet, ""
			}
			req := httptest.NewRequest(method, "/rooms/"+rg.replica+path, strings.NewReader(reqBody))
			code, out, err := svc.ForwardClaims(req, rg.replica, rg.b1)
			wantRefusal(t, name, err)
			if err == nil && (!json.Valid(out) || code/100 != 2 && code != http.StatusConflict) {
				t.Fatalf("%s answers %d %s", name, code, out)
			}
		case "pull":
			if _, err := g.db.Exec(`UPDATE rooms SET applied_seq = 0 WHERE id = ?`, rg.replica); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, replicaState, rg.replica)
			var out struct{ Events []fedrooms.Event }
			bad := status != http.StatusOK || json.Unmarshal([]byte(strings.NewReplacer("{sig}", "", "{nonce}", "").Replace(string(body))), &out) != nil ||
				len(out.Events) > maxRoomEvents
			_, _ = svc.pull(ctx, rg.replica, peerDomain, &refusals{})
			if bad && snapshot(t, replicaState, rg.replica) != before {
				t.Fatalf("a refused pull answer changed the replica: %s", body)
			}
		case "follow":
			wantRefusal(t, name, svc.FollowRemote(ctx, "b1@"+local, "m1@"+peerDomain))
		case "follow withdraw":
			_ = svc.WithdrawFollow(ctx, "b1@"+local, "m1@"+peerDomain) // best effort: its caller goes on either way
		}
	})
}

// publicRig is a server with the public bot pub and one public post of
// it in its feed. Its texts are written straight to the database, past
// the API's cleaning: the pages must escape whatever is stored.
type publicRig struct {
	svc  *Service
	r    chi.Router
	post int64
}

func newPublicRig(tb testing.TB) *publicRig {
	tb.Helper()
	svc := newLocal(tb, "allowlist")
	addBot(tb, svc.db, "pub", bots.Public)
	post := addPost(tb, svc, addFeed(tb, svc, "pub", false), "pub", 1, "post", pubMeta, false)
	r := chi.NewRouter()
	svc.PublicRoutes(r)
	return &publicRig{svc: svc, r: r, post: post}
}

// set stores the bot's name, description, skill name, skill description
// and the post's text, and returns them as read back.
func (pr *publicRig) set(t *testing.T, texts [5]string) [5]string {
	t.Helper()
	skills, _ := json.Marshal([]bots.Skill{{ID: "s1", Name: texts[2], Description: texts[3]}})
	if _, err := pr.svc.db.Exec(`UPDATE bot_profiles SET display_name = ?, description = ?, skills = ?
		WHERE key_id = (SELECT id FROM api_keys WHERE handle = 'pub')`, texts[0], texts[1], string(skills)); err != nil {
		t.Fatal(err)
	}
	if _, err := pr.svc.db.Exec(`UPDATE messages SET content = ? WHERE id = ?`, texts[4], pr.post); err != nil {
		t.Fatal(err)
	}
	p, err := pr.svc.botStore.Get(context.Background(), "pub")
	if err != nil || len(p.Skills) != 1 {
		t.Fatalf("profile: %+v, %v", p, err)
	}
	out := [5]string{p.DisplayName, p.Description, p.Skills[0].Name, p.Skills[0].Description}
	if err := pr.svc.db.QueryRow(`SELECT content FROM messages WHERE id = ?`, pr.post).Scan(&out[4]); err != nil {
		t.Fatal(err)
	}
	return out
}

// render returns path with texts, and path with each stored text that
// is not empty replaced by x: the same template output around other
// text.
func (pr *publicRig) render(t *testing.T, path string, texts [5]string) (got, masked string) {
	t.Helper()
	get := func() string {
		rec := httptest.NewRecorder()
		pr.r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	stored := pr.set(t, texts)
	got = get()
	for i, s := range stored {
		if s != "" {
			stored[i] = "x"
		}
	}
	pr.set(t, stored)
	return got, get()
}

// sameMarkup fails unless got has as many <, >, " and ' as masked: a
// text that adds an element or an attribute, or leaves one, needs them.
func sameMarkup(t *testing.T, path, got, masked string) {
	t.Helper()
	for _, c := range []string{"<", ">", `"`, "'"} {
		if n, want := strings.Count(got, c), strings.Count(masked, c); n != want {
			t.Fatalf("%s has %d %s, the template writes %d:\n%s", path, n, c, want, got)
		}
	}
}

var publicSeeds = [][5]string{
	{"Bot", "line one\nline two", "skill", "does things", "first line\nsecond"},
	{`<script>alert(1)</script>`, `"><img src=x onerror=alert(2)>`, `' onmouseover='x`, `</p></li></ul><!--`, `]]><x/></content></entry></feed>`},
	{"", "", "", "", ""},
	{"\x00\x01\r\n\u2028", "\xff\xfe", "&amp;&lt;", "{{.Domain}}", "\ufffe\uffff" + strings.Repeat("é", 100)},
}

// FuzzPublicPage renders the bot page and the directory with any texts:
// neither holds an element or attribute the template does not write.
func FuzzPublicPage(f *testing.F) {
	pr := newPublicRig(f)
	for _, s := range publicSeeds {
		f.Add(s[0], s[1], s[2], s[3], s[4])
	}
	f.Fuzz(func(t *testing.T, name, description, skillName, skillDescription, post string) {
		texts := [5]string{name, description, skillName, skillDescription, post}
		for _, path := range []string{"/@pub", "/directory"} {
			got, masked := pr.render(t, path, texts)
			sameMarkup(t, path, got, masked)
		}
	})
}

// FuzzFeed renders the bot's Atom feed with any texts: it parses as
// XML, with one entry and no element or attribute the encoder does not
// write.
func FuzzFeed(f *testing.F) {
	pr := newPublicRig(f)
	for _, s := range publicSeeds {
		f.Add(s[0], s[1], s[2], s[3], s[4])
	}
	f.Fuzz(func(t *testing.T, name, description, skillName, skillDescription, post string) {
		const path = "/@pub/feed.xml"
		got, masked := pr.render(t, path, [5]string{name, description, skillName, skillDescription, post})
		var feed atomFeed
		if err := xml.Unmarshal([]byte(got), &feed); err != nil || len(feed.Entries) != 1 {
			t.Fatalf("feed does not parse (%v) or has %d entries:\n%s", err, len(feed.Entries), got)
		}
		sameMarkup(t, path, got, masked)
	})
}
