package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fedMsg is a message as GET /v1/rooms/{id}/messages lists it.
type fedMsg struct {
	ID            int64  `json:"id"`
	Content       string `json:"content"`
	AuthorAddress string `json:"author_address"`
	TrustOrigin   string `json:"trust_origin"`
	Unverified    bool   `json:"unverified"`
	Metadata      struct {
		ReplyTo int64 `json:"reply_to"`
	} `json:"metadata"`
}

// messages returns the messages of room that k reads on f, by content.
func (f *fedEnv) messages(k apiKey, room string) map[string]fedMsg {
	f.t.Helper()
	r := f.callAs("GET", "/v1/rooms/"+room+"/messages?limit=500", k, nil)
	var out struct{ Messages []fedMsg }
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		f.t.Fatalf("messages on %s: %d %s", f.domain, r.Status, r.Body)
	}
	m := map[string]fedMsg{}
	for _, msg := range out.Messages {
		m[msg.Content] = msg
	}
	return m
}

// waitFor polls ok until it holds, for at most 10 s: delivery between
// servers is asynchronous.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(25 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Host A, member servers B and C: invites, messages both ways, a thread
// across the three servers, a claim conflict across servers, B down and
// catching up, removal of C's bot, and A blocking B.
func TestFederatedRooms(t *testing.T) {
	a, b, c := newFedEnv(t), newFedEnv(t), newFedEnv(t)
	for _, p := range [][2]*fedEnv{{a, b}, {b, a}, {a, c}, {c, a}, {b, c}, {c, b}} {
		p[0].peer(p[1])
	}
	a1, b1, c1 := a.botKey("a1"), b.botKey("b1"), c.botKey("c1")
	a1Addr, b1Addr, c1Addr := "a1@"+a.domain, "b1@"+b.domain, "c1@"+c.domain

	// Local messages first, so the three servers number the room's
	// messages differently and reply_to must be translated.
	for i, f := range []*fedEnv{b, c} {
		r := f.admin("POST", "/v1/rooms", map[string]string{"name": "local"})
		var local struct{ ID string }
		if r.Status != 201 || json.Unmarshal(r.Body, &local) != nil {
			t.Fatalf("local room: %d %s", r.Status, r.Body)
		}
		for range 2 * (i + 1) {
			if r := f.admin("POST", "/v1/rooms/"+local.ID+"/messages", map[string]string{"content": "local"}); r.Status != 201 {
				t.Fatalf("local message: %d %s", r.Status, r.Body)
			}
		}
	}

	// 1. A creates a room and invites b1@B and c1@C by address. Both
	// member servers have a replica at once.
	r := a.admin("POST", "/v1/rooms", map[string]string{"name": "proj"})
	var room struct{ ID string }
	if r.Status != 201 || json.Unmarshal(r.Body, &room) != nil {
		t.Fatalf("create room: %d %s", r.Status, r.Body)
	}
	path := "/v1/rooms/" + room.ID
	if r := a.admin("POST", path+"/members", map[string]any{"key_id": a1.ID, "rights": []string{"all"}}); r.Status != 201 {
		t.Fatalf("add a1: %d %s", r.Status, r.Body)
	}
	standIn := map[string]int64{}
	for _, addr := range []string{b1Addr, c1Addr} {
		r := a.admin("POST", path+"/members", map[string]any{"address": addr, "rights": []string{"all"}})
		var out struct {
			KeyID   int64 `json:"key_id"`
			Address string
		}
		if r.Status != 201 || json.Unmarshal(r.Body, &out) != nil || out.Address != addr {
			t.Fatalf("invite %s: %d %s", addr, r.Status, r.Body)
		}
		standIn[addr] = out.KeyID
	}
	for _, m := range []struct {
		f *fedEnv
		k apiKey
	}{{b, b1}, {c, c1}} {
		r := m.f.callAs("GET", "/v1/rooms", m.k, nil)
		var list struct {
			Rooms []struct{ ID, Name, Home string }
		}
		if r.Status != 200 || json.Unmarshal(r.Body, &list) != nil || len(list.Rooms) != 1 ||
			list.Rooms[0].ID != room.ID || list.Rooms[0].Home != a.domain || list.Rooms[0].Name != "proj" {
			t.Fatalf("replica on %s: %d %s", m.f.domain, r.Status, r.Body)
		}
	}

	post := func(f *fedEnv, k apiKey, body map[string]any) fedMsg {
		t.Helper()
		r := f.callAs("POST", path+"/messages", k, body)
		var m fedMsg
		if r.Status != 201 || json.Unmarshal(r.Body, &m) != nil {
			t.Fatalf("post on %s: %d %s", f.domain, r.Status, r.Body)
		}
		return m
	}
	// arrived waits until k reads content in the room on f and returns it.
	arrived := func(f *fedEnv, k apiKey, content string) fedMsg {
		t.Helper()
		var m fedMsg
		waitFor(t, fmt.Sprintf("%q on %s", content, f.domain), func() bool {
			var ok bool
			m, ok = f.messages(k, room.ID)[content]
			return ok
		})
		return m
	}
	// remote checks a message of a remote author as a server shows it:
	// external, with the author's address, its signature verified.
	remote := func(m fedMsg, author string) {
		t.Helper()
		if m.AuthorAddress != author || m.TrustOrigin != "external" || m.Unverified {
			t.Fatalf("message %q: %+v, want external from %s", m.Content, m, author)
		}
	}

	// 2. A message from A reaches B and C. On B, remote messages are
	// external and carry their author's address.
	post(a, a1, map[string]any{"content": "hello from A", "message_type": "question"})
	remote(arrived(b, b1, "hello from A"), a1Addr)
	remote(arrived(c, c1, "hello from A"), a1Addr)

	// b1 posts on B; A and C see it as b1@B.
	onB := post(b, b1, map[string]any{"content": "hello from B", "message_type": "answer"})
	if onB.AuthorAddress != "" || onB.TrustOrigin != "internal" {
		t.Fatalf("b1's message on B: %+v", onB)
	}
	onA := arrived(a, a1, "hello from B")
	remote(onA, b1Addr)
	onC := arrived(c, c1, "hello from B")
	remote(onC, b1Addr)

	// A thread across the three servers: c1 replies with C's id of
	// b1's message, and each server threads it under its own id.
	reply := post(c, c1, map[string]any{"content": "reply from C", "message_type": "answer", "reply_to": onC.ID})
	if reply.Metadata.ReplyTo != onC.ID {
		t.Fatalf("reply on C: %+v, want reply_to %d", reply, onC.ID)
	}
	for _, s := range []struct {
		f      *fedEnv
		k      apiKey
		parent int64
	}{{a, a1, onA.ID}, {b, b1, onB.ID}} {
		m := arrived(s.f, s.k, "reply from C")
		remote(m, c1Addr)
		if m.Metadata.ReplyTo != s.parent {
			t.Fatalf("reply on %s: reply_to %d, want %d", s.f.domain, m.Metadata.ReplyTo, s.parent)
		}
	}
	if onA.ID == onB.ID || onB.ID == onC.ID {
		t.Fatalf("ids %d (A), %d (B), %d (C) should differ", onA.ID, onB.ID, onC.ID)
	}

	// 3. Claims go to the host: b1 claims a path, and c1's claim of the
	// same path gets 409 naming b1@B, external.
	claim := func(f *fedEnv, k apiKey) httpResp {
		return f.callAs("POST", path+"/claims", k, map[string]any{"paths": []string{"src/main.go"}})
	}
	r = claim(b, b1)
	var held struct {
		Claims []struct {
			KeyID int64 `json:"key_id"`
		}
	}
	if r.Status != 201 || json.Unmarshal(r.Body, &held) != nil || len(held.Claims) != 1 || held.Claims[0].KeyID != b1.ID {
		t.Fatalf("b1 claims: %d %s", r.Status, r.Body)
	}
	r = claim(c, c1)
	var conflict struct {
		Error     struct{ Code, Message string }
		Conflicts []struct {
			Label       string
			TrustOrigin string `json:"trust_origin"`
		}
	}
	if r.Status != 409 || json.Unmarshal(r.Body, &conflict) != nil || len(conflict.Conflicts) != 1 ||
		conflict.Conflicts[0].Label != b1Addr || conflict.Conflicts[0].TrustOrigin != "external" ||
		!strings.Contains(conflict.Error.Message, b1Addr+" (EXTERNAL, untrusted)") {
		t.Fatalf("c1 claims the same path: %d %s", r.Status, r.Body)
	}

	// 4. B is down while A posts twice; A's push to B fails. Started
	// again on the same database and ports, B catches up.
	b.close()
	post(a, a1, map[string]any{"content": "while B was down 1", "message_type": "status_update"})
	post(a, a1, map[string]any{"content": "while B was down 2", "message_type": "status_update"})
	waitFor(t, "a failed push to B", func() bool {
		r := a.admin("GET", path+"/delivery", nil)
		var d struct {
			Delivery []struct {
				Peer      string
				Attempts  int
				LastError string `json:"last_error"`
			}
		}
		_ = json.Unmarshal(r.Body, &d)
		for _, row := range d.Delivery {
			if row.Peer == b.domain && row.Attempts > 0 && row.LastError != "" {
				return true
			}
		}
		return false
	})
	b.start()
	remote(arrived(b, b1, "while B was down 1"), a1Addr)
	remote(arrived(b, b1, "while B was down 2"), a1Addr)

	// 5. A removes c1: C's replica is archived.
	if r := a.admin("DELETE", fmt.Sprintf("%s/members/%d", path, standIn[c1Addr]), nil); r.Status != 204 {
		t.Fatalf("remove c1: %d %s", r.Status, r.Body)
	}
	waitFor(t, "C's replica archived", func() bool {
		r := c.admin("GET", path, nil)
		var rv struct {
			ArchivedAt string `json:"archived_at"`
		}
		return r.Status == 200 && json.Unmarshal(r.Body, &rv) == nil && rv.ArchivedAt != ""
	})

	// 6. A blocks B: b1's stand-in leaves A's room, and B's posts are
	// refused by A, so B stores nothing.
	if r := a.admin("POST", "/v1/federation/peers/"+b.domain+"/block", nil); r.Status != 200 {
		t.Fatalf("block B: %d %s", r.Status, r.Body)
	}
	if r := a.admin("GET", path+"/members", nil); r.Status != 200 || strings.Contains(string(r.Body), b1Addr) {
		t.Fatalf("members on A after the block: %d %s", r.Status, r.Body)
	}
	r = b.callAs("POST", path+"/messages", b1, map[string]any{"content": "after the block", "message_type": "answer"})
	if r.Status != 403 || !strings.Contains(string(r.Body), "peer_not_allowed") {
		t.Fatalf("post from B after the block: %d %s", r.Status, r.Body)
	}
	for _, s := range []struct {
		f *fedEnv
		k apiKey
	}{{a, a1}, {b, b1}} {
		if _, ok := s.f.messages(s.k, room.ID)["after the block"]; ok {
			t.Fatalf("%s stored a message refused by the host", s.f.domain)
		}
	}
}
