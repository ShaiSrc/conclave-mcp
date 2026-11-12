package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestE2E_ExternalKeySeesOnlyItsRoom: a real server, two rooms, an
// internal and an external key with one room each. The external key
// finds nothing outside its room, through the CLI and through /v1/mcp.
func TestE2E_ExternalKeySeesOnlyItsRoom(t *testing.T) {
	env, admin := bootstrapKeyFor(t)
	room := func(name string) string {
		t.Helper()
		r := env.call("POST", "/v1/rooms", admin.Plaintext, map[string]any{"name": name})
		if r.Status != 201 {
			t.Fatalf("create %s: %d %s", name, r.Status, r.Body)
		}
		return mustField(t, r.Body, "id")
	}
	key := func(body map[string]any) (int64, string) {
		t.Helper()
		r := env.call("POST", "/v1/keys", admin.Plaintext, body)
		var k struct {
			ID  int64  `json:"id"`
			Key string `json:"key"`
		}
		if r.Status != 201 || json.Unmarshal(r.Body, &k) != nil {
			t.Fatalf("create key %v: %d %s", body, r.Status, r.Body)
		}
		return k.ID, k.Key
	}
	invite := func(roomID string, id int64, rights []string) httpResp {
		return env.call("POST", "/v1/rooms/"+roomID+"/invite", admin.Plaintext, map[string]any{"key_id": id, "rights": rights})
	}

	intRoom, extRoom := room("e2e-internal-room"), room("e2e-external-room")
	intID, intKey := key(map[string]any{"label": "e2e-int", "trust_origin": "internal"})
	extID, extKey := key(map[string]any{"label": "e2e-ext"}) // no mark: external
	if r := invite(intRoom, intID, []string{"all"}); r.Status != 201 {
		t.Fatalf("invite internal key: %d %s", r.Status, r.Body)
	}
	if r := invite(extRoom, extID, []string{"read", "handoff"}); r.Status != 201 {
		t.Fatalf("invite external key: %d %s", r.Status, r.Body)
	}
	if r := invite(intRoom, extID, []string{"read"}); r.Status != 409 || !strings.Contains(string(r.Body), "key_in_other_room") {
		t.Errorf("second room for the external key: %d %s, want 409 key_in_other_room", r.Status, r.Body)
	}
	if r := env.call("POST", "/v1/rooms/"+intRoom+"/messages", intKey, map[string]any{"content": "internal-secret"}); r.Status != 201 {
		t.Fatalf("internal send: %d %s", r.Status, r.Body)
	}
	// leaks reports output that names anything of the internal room, in
	// table or JSON form alike.
	leaks := func(s string) bool {
		return strings.Contains(s, "internal-secret") || strings.Contains(s, "e2e-int") || strings.Contains(s, intRoom)
	}

	// Through the CLI, as the external key.
	cli := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, env.BinaryPath(), args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CONCLAVE_API_KEY=" + extKey, "CONCLAVE_SERVER=" + env.URL()}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	if out, err := cli("rooms", "list", "--format", "json"); err != nil || !strings.Contains(out, extRoom) || strings.Contains(out, intRoom) {
		t.Errorf("cli rooms list: %v\n%s", err, out)
	}
	const (
		notFound   = "404 (not_found): room not found"
		adminOnly  = "403 (forbidden): admin key required"
		noQuestion = "403 (missing_right): missing right: question"
	)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"messages", "list", intRoom}, notFound},
		{[]string{"rooms", "members", intRoom}, notFound},
		{[]string{"rooms", "get", intRoom}, notFound},
		{[]string{"keys", "list"}, adminOnly},
		{[]string{"audit", "list"}, adminOnly},
		{[]string{"messages", "send", extRoom, "--content", "q?", "--message-type", "question"}, noQuestion},
	} {
		if out, err := cli(c.args...); err == nil || !strings.Contains(out, c.want) || leaks(out) {
			t.Errorf("cli %v: want refusal %q and nothing of the other room, got err=%v\n%s", c.args, c.want, err, out)
		}
	}
	if out, err := cli("messages", "send", extRoom, "--content", "delivered", "--message-type", "handoff"); err != nil {
		t.Errorf("cli handoff: %v\n%s", err, out)
	}
	if out, err := cli("messages", "list", extRoom, "--format", "json"); err != nil || !strings.Contains(strings.Join(strings.Fields(out), ""), `"trust_origin":"external"`) {
		t.Errorf("cli own room: want the handoff marked external, got %v\n%s", err, out)
	}

	// Through /v1/mcp, as the external key.
	mcp := func(tool string, args map[string]any) (string, bool) {
		r := env.call("POST", "/v1/mcp", extKey, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args},
		})
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if r.Status != 200 {
			t.Fatalf("mcp %s: status %d %s", tool, r.Status, r.Body)
		}
		if err := json.Unmarshal(r.Body, &resp); err != nil || len(resp.Result.Content) != 1 {
			t.Fatalf("mcp %s: malformed envelope: %v %s", tool, err, r.Body)
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}
	if text, isErr := mcp("rooms.list", map[string]any{}); isErr || !strings.Contains(text, extRoom) || strings.Contains(text, intRoom) {
		t.Errorf("mcp rooms.list: isError=%v %s", isErr, text)
	}
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"messages.list", map[string]any{"room_id": intRoom}, "not_found"},
		{"rooms.members", map[string]any{"room_id": intRoom}, "not_found"},
		{"claims.list", map[string]any{"room_id": intRoom}, "not_found"},
		{"keys.list", map[string]any{}, "admin key required"},
		{"audit.list", map[string]any{}, "admin key required"},
		{"messages.send", map[string]any{"room_id": extRoom, "content": "q?", "message_type": "question"}, "missing_right"},
	} {
		if text, isErr := mcp(c.tool, c.args); !isErr || !strings.Contains(text, c.want) || leaks(text) {
			t.Errorf("mcp %s: want error %q and nothing of the other room, got isError=%v %s", c.tool, c.want, isErr, text)
		}
	}
	if text, isErr := mcp("rooms.members", map[string]any{"room_id": extRoom}); isErr || !strings.Contains(text, "e2e-ext") || strings.Contains(text, `"rights"`) {
		t.Errorf("mcp own room members: isError=%v %s", isErr, text)
	}
}
