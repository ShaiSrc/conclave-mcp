package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hermes/conclave-mcp/internal/agent"
)

type dmConv struct {
	RoomID  string `json:"room_id"`
	Address string `json:"address"`
	Unread  int    `json:"unread"`
}

// dmList returns k's conversations on f.
func (f *fedEnv) dmList(k apiKey) []dmConv {
	f.t.Helper()
	r := f.callAs("GET", "/v1/direct", k, nil)
	var out struct{ Conversations []dmConv }
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		f.t.Fatalf("direct list on %s: %d %s", f.domain, r.Status, r.Body)
	}
	return out.Conversations
}

// dmSend posts a message to a bot through POST /v1/direct.
func (f *fedEnv) dmSend(k apiKey, to, content string) httpResp {
	f.t.Helper()
	return f.callAs("POST", "/v1/direct", k, map[string]string{"to": to, "content": content, "message_type": "question"})
}

// Bot a1 on A talks to b1 on B (policy peers), b1 answers into the same
// conversation, then B narrows the policy to local: a2 on A is refused,
// and the conversation of a1 and b1 goes on.
func TestDirect_AcrossServers(t *testing.T) {
	a, b := newFedEnv(t), newFedEnv(t)
	a.peer(b)
	b.peer(a)
	a1, a2, b1 := a.botKey("a1"), a.botKey("a2"), b.botKey("b1")
	if r := b.admin("PATCH", "/v1/bots/b1/profile", map[string]string{"dm_policy": "peers"}); r.Status != 200 {
		t.Fatalf("set policy: %d %s", r.Status, r.Body)
	}

	// 1. a1 starts the conversation; b1 sees it, unread 1, external.
	r := a.dmSend(a1, "b1@"+b.domain, "hello b1")
	var started struct {
		RoomID string `json:"room_id"`
	}
	if r.Status != 201 || json.Unmarshal(r.Body, &started) != nil || started.RoomID == "" {
		t.Fatalf("start: %d %s", r.Status, r.Body)
	}
	var convs []dmConv
	waitFor(t, "the conversation on B", func() bool { convs = b.dmList(b1); return len(convs) == 1 })
	if convs[0].RoomID != started.RoomID || convs[0].Address != "a1@"+a.domain || convs[0].Unread != 1 {
		t.Fatalf("b1's conversations: %+v", convs)
	}
	m := b.messages(b1, started.RoomID)["hello b1"]
	if m.AuthorAddress != "a1@"+a.domain || m.TrustOrigin != "external" || m.Unverified {
		t.Fatalf("message on B: %+v", m)
	}
	rr := b.callAs("GET", "/v1/rooms/"+started.RoomID+"/messages", b1, nil)
	var list struct{ Messages []agent.Msg }
	if rr.Status != 200 || json.Unmarshal(rr.Body, &list) != nil {
		t.Fatalf("messages: %d %s", rr.Status, rr.Body)
	}
	text := agent.FormatDirect(agent.Room{}, "a1@"+a.domain, list.Messages, b1.ID)
	if !strings.Contains(text, "EXTERNAL") || !strings.Contains(text, "hello b1") {
		t.Fatalf("rendered: %s", text)
	}
	// Reading marked it read.
	if c := b.dmList(b1); len(c) != 1 || c[0].Unread != 0 {
		t.Fatalf("after read: %+v", c)
	}

	// 2. b1 answers by address: same conversation, a1 has unread 1.
	if r := b.dmSend(b1, "a1@"+a.domain, "hello a1"); r.Status != 201 || !strings.Contains(string(r.Body), started.RoomID) {
		t.Fatalf("answer: %d %s", r.Status, r.Body)
	}
	waitFor(t, "unread on A", func() bool { c := a.dmList(a1); return len(c) == 1 && c[0].Unread == 1 })
	if c := a.dmList(a1); c[0].RoomID != started.RoomID || c[0].Address != "b1@"+b.domain || len(b.dmList(b1)) != 1 {
		t.Fatalf("a1's conversations: %+v", c)
	}
	if m := a.messages(a1, started.RoomID)["hello a1"]; m.AuthorAddress != "b1@"+b.domain || m.TrustOrigin != "external" {
		t.Fatalf("answer on A: %+v", m)
	}

	// 3. Policy local: a new conversation from a2 is refused, A keeps no room.
	if r := b.admin("PATCH", "/v1/bots/b1/profile", map[string]string{"dm_policy": "local"}); r.Status != 200 {
		t.Fatalf("narrow policy: %d %s", r.Status, r.Body)
	}
	if r := a.dmSend(a2, "b1@"+b.domain, "may I?"); r.Status != 403 || !strings.Contains(string(r.Body), "dm_refused") {
		t.Fatalf("refused start: %d %s", r.Status, r.Body)
	}
	if c := a.dmList(a2); len(c) != 0 {
		t.Fatalf("a2 has conversations: %+v", c)
	}
	rooms := a.admin("GET", "/v1/rooms?kind=direct", nil)
	if rooms.Status != 200 || strings.Count(string(rooms.Body), `"dm:`) != 1 {
		t.Fatalf("rooms on A: %d %s", rooms.Status, rooms.Body)
	}
	if n := a.auditCount("direct.refused"); n != 1 {
		t.Fatalf("direct.refused on A: %d", n)
	}

	// 4. The old conversation still works, both ways.
	if r := a.dmSend(a1, "b1@"+b.domain, "still there?"); r.Status != 201 || !strings.Contains(string(r.Body), started.RoomID) {
		t.Fatalf("a1 after narrowing: %d %s", r.Status, r.Body)
	}
	waitFor(t, "the message on B", func() bool { _, ok := b.messages(b1, started.RoomID)["still there?"]; return ok })
	if r := b.dmSend(b1, "a1@"+a.domain, "yes"); r.Status != 201 || !strings.Contains(string(r.Body), started.RoomID) {
		t.Fatalf("b1 after narrowing: %d %s", r.Status, r.Body)
	}
	waitFor(t, "the answer on A", func() bool { _, ok := a.messages(a1, started.RoomID)["yes"]; return ok })
}
