package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func readJSONFile(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestWriteFiles_CreatesThenMergesIdempotently(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, ".claude"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644))
	must(os.WriteFile(filepath.Join(root, ".claude", "settings.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]},"model":"x"}`), 0o644))
	must(os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Project\n"), 0o644))

	changed, err := WriteFiles(root, "acme/widgets")
	if err != nil || len(changed) != 4 {
		t.Fatalf("first run: changed=%v err=%v", changed, err)
	}
	if changed, err := WriteFiles(root, "acme/widgets"); err != nil || len(changed) != 0 {
		t.Fatalf("second run must change nothing: %v %v", changed, err)
	}

	servers := readJSONFile(t, filepath.Join(root, ".mcp.json"))["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["conclave"] == nil {
		t.Errorf("mcpServers: %v", servers)
	}
	settings := readJSONFile(t, filepath.Join(root, ".claude", "settings.json"))
	hooks := settings["hooks"].(map[string]any)
	if pre := hooks["PreToolUse"].([]any); len(pre) != 2 || settings["model"] != "x" {
		t.Errorf("PreToolUse must keep the Bash hook and add ours once: %v", pre)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"} {
		if g, _ := hooks[ev].([]any); len(g) != 1 {
			t.Errorf("%s: want one conclave hook, got %v", ev, hooks[ev])
		}
	}
	md, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if !strings.HasPrefix(string(md), "# Project\n") || strings.Count(string(md), markerStart) != 1 || !strings.Contains(string(md), "acme/widgets") {
		t.Errorf("CLAUDE.md:\n%s", md)
	}
	if cfg := readJSONFile(t, filepath.Join(root, ".conclave.json")); cfg["room"] != "acme/widgets" {
		t.Errorf(".conclave.json: %v", cfg)
	}
}

// fakeAPI serves the endpoints init and agent add use.
func fakeAPI(t *testing.T, inviteStatus int, rooms string) (*httptest.Server, *[]string) {
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io_ReadAll(r)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+b)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/rooms":
			w.Write([]byte(rooms))
		case "POST /v1/rooms":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"NEW","name":"acme/widgets"}`))
		case "POST /v1/keys":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":7,"key":"tk_newkey"}`))
		case "POST /v1/rooms/R/invite":
			w.WriteHeader(inviteStatus)
			if inviteStatus >= 400 {
				w.Write([]byte(`{"error":{"code":"forbidden","message":"only owner can invite"}}`))
			}
		case "DELETE /v1/keys/7":
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(api.Close)
	return api, &calls
}

func io_ReadAll(r *http.Request) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.String(), err
}

func useAPI(t *testing.T, url string) {
	old := openClient
	openClient = func(*cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: url, Token: "tk_admin"})
	}
	t.Cleanup(func() { openClient = old })
}

func runAgentAdd(t *testing.T, args ...string) (string, error) {
	cmd := NewAgentCmd()
	cmd.SilenceUsage = true // the root command does this in the binary
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"add"}, args...))
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), err
}

func TestAgentAdd_PrintsExportAndInvites(t *testing.T) {
	api, calls := fakeAPI(t, 201, `{"rooms":[{"id":"R","name":"acme/widgets"}]}`)
	useAPI(t, api.URL)
	out, err := runAgentAdd(t, "agent-c", "--room", "acme/widgets")
	if err != nil || out != "export CONCLAVE_API_KEY=tk_newkey\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), `POST /v1/rooms/R/invite {"key_id":7,"role":"writer"}`) {
		t.Errorf("invite not sent as expected: %v", *calls)
	}
}

func TestAgentAdd_RevokesKeyWhenInviteFails(t *testing.T) {
	api, calls := fakeAPI(t, 403, `{"rooms":[{"id":"R","name":"acme/widgets"}]}`)
	useAPI(t, api.URL)
	if out, err := runAgentAdd(t, "agent-c", "--room", "acme/widgets"); err == nil || out != "" {
		t.Fatalf("want an error and no key printed, got out=%q err=%v", out, err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "DELETE /v1/keys/7") {
		t.Errorf("orphan key not revoked: %v", *calls)
	}
}

func TestEnsureRoom_CreatesWhenMissing(t *testing.T) {
	api, _ := fakeAPI(t, 201, `{"rooms":[]}`)
	c, _ := client.New(client.Options{Server: api.URL, Token: "tk_admin"})
	id, created, err := ensureRoom(context.Background(), c, "acme/widgets")
	if err != nil || id != "NEW" || !created {
		t.Errorf("got %q %v %v", id, created, err)
	}
}

func TestWriteFiles_RefusesToClobberUnexpectedTypes(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{"hooks":{"PreToolUse":{"oops":"an object, not a list"}}}`
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteFiles(root, "acme/widgets"); err == nil {
		t.Fatal("want an error for hooks.PreToolUse of the wrong type")
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Errorf("settings.json was modified:\n%s", b)
	}
}
