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
	"time"

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
		case "/v1/rooms/01M72DWQRAZXY3X8HWGJ0QZGNW/messages":
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
		if err := cmd.RunE(cmd, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
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

func TestInbox_ErrorsNeverExit2(t *testing.T) {
	// Exit 2 from a UserPromptSubmit hook makes Claude Code drop the
	// user's prompt; a missing key must be a non-blocking exit 1.
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return nil, fmt.Errorf("missing api key")
	}
	defer func() { openClient = oldOC }()
	cmd := newInboxCmd()
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`))
	cmd.SetContext(context.Background())
	if code := ExitCode(cmd.RunE(cmd, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"})); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}

// waitAPI serves an empty backlog until release is closed; its event
// stream first sends our own message (to be ignored), then, after
// release, another agent's message, which the backlog also returns.
func waitAPI(t *testing.T) (url string, release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	released := func() bool {
		select {
		case <-release:
			return true
		default:
			return false
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":2,"label":"agent-b"}]}`)
		case "/v1/rooms/" + waitRoom + "/messages":
			if released() && r.URL.Query().Get("since") != "t2" {
				fmt.Fprint(w, `{"messages":[{"id":2,"author_key_id":2,"ts":"t2","content":"review please"}]}`)
				return
			}
			fmt.Fprint(w, `{"messages":[]}`)
		case "/v1/rooms/" + waitRoom + "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "event: message.created\ndata: {\"id\":1,\"author_key_id\":1,\"ts\":\"t1\"}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
				fmt.Fprint(w, "event: message.created\ndata: {\"id\":2,\"author_key_id\":2,\"ts\":\"t2\"}\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, release
}

const waitRoom = "01M72DWQRAZXY3X8HWGJ0QZGNX"

func runInboxWait(t *testing.T, url, wait string) (string, error, time.Duration) {
	t.Helper()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: url, Token: "stub"})
	}
	t.Cleanup(func() { openClient = oldOC })
	var out bytes.Buffer
	cmd := newInboxCmd()
	_ = cmd.Flags().Set("state", filepath.Join(t.TempDir(), "cursor.json"))
	_ = cmd.Flags().Set("wait", wait)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	start := time.Now()
	err := cmd.RunE(cmd, []string{waitRoom})
	return out.String(), err, time.Since(start)
}

func TestInboxWait_ReturnsOnAnotherAgentsMessage(t *testing.T) {
	url, release := waitAPI(t)
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	out, err, took := runInboxWait(t, url, "10s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "agent-b") || !strings.Contains(out, "review please") {
		t.Errorf("want agent-b's message, got %q", out)
	}
	if took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("took %v: want it to wait for the message, then return at once", took)
	}
}

func TestInboxWait_TimesOutSilently(t *testing.T) {
	url, _ := waitAPI(t)
	out, err, took := runInboxWait(t, url, "400ms")
	if err != nil || out != "" {
		t.Fatalf("timeout: want no output and no error, got %q, %v", out, err)
	}
	if took < 350*time.Millisecond || took > 3*time.Second {
		t.Errorf("took %v, want about 400ms", took)
	}
}
