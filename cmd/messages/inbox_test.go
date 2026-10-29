package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func TestInbox_HookOutputSkipsOwnAndAdvancesCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":1,"label":"agent-a"},{"id":2,"label":"agent-b"}]}`)
		case "/v1/rooms/r1/messages":
			if r.URL.Query().Get("since") == "t2" {
				fmt.Fprint(w, `{"messages":[]}`)
				return
			}
			fmt.Fprint(w, `{"messages":[
				{"id":1,"author_key_id":1,"ts":"t1","content":"mine"},
				{"id":2,"author_key_id":2,"ts":"t2","content":"I take src/auth","message_type":"status_update","metadata":{"files":["src/auth"]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()
	state := filepath.Join(t.TempDir(), "cursor.json")

	run := func() string {
		var out bytes.Buffer
		cmd := newInboxCmd()
		_ = cmd.Flags().Set("state", state)
		cmd.SetIn(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`))
		cmd.SetOut(&out)
		cmd.SetContext(context.Background())
		if err := cmd.RunE(cmd, []string{"r1"}); err != nil {
			t.Fatalf("inbox: %v", err)
		}
		return out.String()
	}

	var hook struct {
		Out struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(run()), &hook); err != nil {
		t.Fatalf("first run: want hook JSON: %v", err)
	}
	if hook.Out.Event != "PostToolUse" {
		t.Errorf("hookEventName: got %q", hook.Out.Event)
	}
	ctx := hook.Out.Context
	if !strings.Contains(ctx, "agent-b <status_update>") || !strings.Contains(ctx, "I take src/auth") || !strings.Contains(ctx, `"src/auth"`) {
		t.Errorf("context missing agent-b's message: %q", ctx)
	}
	if strings.Contains(ctx, "mine") {
		t.Errorf("context contains our own message: %q", ctx)
	}
	if out := run(); out != "" {
		t.Errorf("second run with nothing new: got %q, want no output", out)
	}
}
