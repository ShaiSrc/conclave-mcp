package federation

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// The extension rules of protocol version 1 (docs/protocol.md): a room
// event of a type the receiver does not know is applied as nothing, and
// receivers ignore JSON fields they do not know.

// An unknown event type, pushed or pulled, is skipped: applied_seq
// moves past it, the events after it apply, and nothing is refused.
func TestReplicaEvents_UnknownTypeSkipped(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	host := h.id.Domain
	if got := h.push(t, g, room, roomEv(1, "proj", false), event(2, "poll", map[string]any{"question": "?"}),
		memberEv(3, h.addr("h1"), "read"), msg(4, 100, room, host, h.addr("h1"), "pushed", nil, "", nil)); got != 4 {
		t.Fatalf("push: applied %d, want 4", got)
	}
	// 5 to 7 come by a pull after the gap: two of them of unknown types,
	// one without a payload.
	log := []fedrooms.Event{{Seq: 5, Type: "reaction"}, msg(6, 101, room, host, h.addr("h1"), "pulled", nil, "", nil),
		event(7, "poll.close", map[string]any{"id": 2}), msg(8, 102, room, host, h.addr("h1"), "after", nil, "", nil)}
	h.setLog(room, log...)
	if got := h.push(t, g, room, log[3]); got != 8 {
		t.Fatalf("push after a gap: applied %d, want 8", got)
	}
	if got := appliedSeq(t, g, room); got != 8 {
		t.Errorf("applied_seq %d, want 8", got)
	}
	if ms := storedMessages(t, g, room); len(ms) != 3 || ms[0].Content != "pushed" || ms[1].Content != "pulled" || ms[2].Content != "after" {
		t.Errorf("messages: %+v", ms)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
		t.Errorf("%d event_refused rows, want 0", n)
	}
}

// A signed request to an endpoint this server does not have gets 404,
// and one with a method it does not route 405, after verification: the
// answers by which a newer server learns an optional endpoint is not
// supported. Unsigned, both are 401.
func TestUnknownEndpoint(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	for _, c := range []struct {
		method, path string
		status       int
	}{{http.MethodPost, "/fed/v1/polls", http.StatusNotFound}, {http.MethodGet, "/fed/v1/ping", http.StatusMethodNotAllowed}} {
		if rec := serve(svc, signedReq(t, rm.id, local, c.method, c.path, nil)); rec.Code != c.status {
			t.Errorf("signed %s %s: %d, want %d", c.method, c.path, rec.Code, c.status)
		}
		wantErr(t, serve(svc, httptest.NewRequest(c.method, c.path, nil)), http.StatusUnauthorized, "bad_signature")
	}
}

// withExtra is v as a JSON object with a field no version 1 server
// knows.
func withExtra(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["x_future"] = map[string]any{"n": 1, "list": []string{"a"}}
	return m
}

// Every body a /fed/v1 endpoint reads, and the server document, may
// carry fields the receiver does not know: they are ignored.
func TestUnknownFieldsIgnored(t *testing.T) {
	t.Run("ping", func(t *testing.T) {
		svc := newLocal(t, "allowlist")
		rm := newRemote(t)
		allow(t, svc, rm.id)
		wantOK(t, serve(svc, signed(t, rm.id, local, `{"x_future":{"n":1}}`)))
	})

	t.Run("invite and push", func(t *testing.T) {
		g := newRoomRig(t)
		h := newFakeHost(t, g)
		g.bot(t, "b1")
		room := ulid.Make().String()
		invite := withExtra(t, map[string]any{"name": "proj", "handle": "b1", "rights": []string{"read"}, "kind": "room"})
		if rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/invite", invite)); rec.Code != http.StatusCreated {
			t.Fatalf("invite: %d %s", rec.Code, rec.Body)
		}
		var evs []map[string]any
		for _, ev := range []fedrooms.Event{roomEv(1, "renamed", false), memberEv(2, h.addr("h1"), "read"),
			msg(3, 100, room, h.id.Domain, h.addr("h1"), "hello", nil, `{"k":"v"}`, nil)} {
			e := withExtra(t, ev)
			e["payload"] = withExtra(t, ev.Payload)
			evs = append(evs, e)
		}
		rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/rooms/"+room+"/events",
			withExtra(t, map[string]any{"events": evs})))
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"applied\":3}\n" {
			t.Fatalf("push: %d %s", rec.Code, rec.Body)
		}
		if n := count(t, g.db, `SELECT COUNT(*) FROM rooms WHERE id = ? AND name = 'renamed'`, room); n != 1 {
			t.Error("the room event was not applied")
		}
		if ms := storedMessages(t, g, room); len(ms) != 1 || ms[0].Content != "hello" || ms[0].Meta != `{"k":"v"}` {
			t.Errorf("messages: %+v", ms)
		}
		if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused'`); n != 0 {
			t.Errorf("%d event_refused rows, want 0", n)
		}
	})

	t.Run("message, claims and leave", func(t *testing.T) {
		g, m, room, _ := fedRoom(t, "read,answer,claim")
		base, as := "/fed/v1/rooms/"+room, "?author="+m.addr("c1")
		for _, c := range []struct {
			method, path string
			body         any
			status       int
		}{
			{http.MethodPost, base + "/messages", withExtra(t, m.proposal(room, m.addr("c1"), "answer", "hi", json.RawMessage(`{"k":"v"}`), nil)), http.StatusCreated},
			{http.MethodPost, base + "/claims" + as, withExtra(t, map[string]any{"paths": []string{"a/"}}), http.StatusCreated},
			{http.MethodPost, base + "/claims/heartbeat" + as, withExtra(t, map[string]any{"ttl_seconds": 60}), http.StatusOK},
			{http.MethodPost, base + "/claims/release" + as, withExtra(t, map[string]any{}), http.StatusOK},
			{http.MethodPost, base + "/leave", withExtra(t, map[string]any{"author": m.addr("c1")}), http.StatusNoContent},
		} {
			if rec := g.fedDo(m.req(t, c.method, c.path, c.body)); rec.Code != c.status {
				t.Errorf("%s %s: %d %s, want %d", c.method, c.path, rec.Code, rec.Body, c.status)
			}
		}
	})

	t.Run("follow and withdraw", func(t *testing.T) {
		g := newRoomRig(t)
		h := newFakeHost(t, g)
		g.followBot(t, "b1", "")
		body := withExtra(t, map[string]any{"follower": h.addr("x"), "handle": "b1"})
		if rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/follows", body)); rec.Code != http.StatusCreated {
			t.Errorf("follow: %d %s", rec.Code, rec.Body)
		}
		if rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodPost, "/fed/v1/follows/withdraw", body)); rec.Code != http.StatusNoContent {
			t.Errorf("withdraw: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("server document", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		id := testIdentity(t, ln.Addr().String())
		doc := id.Document(time.Now())
		key := withExtra(t, doc.Keys[0])
		key["endorsed_by"] = withExtra(t, Endorsement{KeyID: "ed25519:AAAAAAAAAAAAAAAA", Sig: "AA"})
		body := withExtra(t, doc)
		body["keys"] = []any{key}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(body)
		}))
		_ = srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		t.Cleanup(srv.Close)
		got, err := NewFetcher(true, always).Document(context.Background(), id.Domain)
		if err != nil || len(got.Keys) != 1 || got.Keys[0].KeyID != id.KeyID || got.Keys[0].EndorsedBy == nil {
			t.Errorf("document: %+v, %v", got, err)
		}
	})
}
