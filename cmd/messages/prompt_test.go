package messages

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func runPrompt(t *testing.T, args []string, stored bool, server ...string) (string, string, error) {
	t.Helper()
	old := storedKey
	storedKey = func(string) (string, error) {
		if stored {
			return "k", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { storedKey = old })
	var out, errOut bytes.Buffer
	cmd := NewPromptCmd()
	cmd.Flags().String("server", "", "") // a persistent flag of the root command
	if len(server) > 0 {
		_ = cmd.Flags().Set("server", server[0])
	}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, args)
	return out.String(), errOut.String(), err
}

func TestPrompt_UsesLabelAndRoom(t *testing.T) {
	out, errOut, err := runPrompt(t, []string{"codex-backend", "acme/widgets"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`You are "codex-backend"`,
		"conclave-mcp --as codex-backend messages inbox acme/widgets --wait 8h",
		"--reply-to",
		"messages thread acme/widgets",
		"conclave-mcp --as codex-backend status acme/widgets",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("no warning expected, got %q", errOut)
	}
}

func TestPrompt_QuotesRoomAndWarnsWithoutKey(t *testing.T) {
	out, errOut, err := runPrompt(t, []string{"a1", "my room"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "messages inbox 'my room' --wait 8h") {
		t.Errorf("room not quoted:\n%s", out)
	}
	if !strings.Contains(errOut, "agent add a1 --store") {
		t.Errorf("want a hint to store the key, got %q", errOut)
	}
}

func TestPrompt_RoomFromEnvAndBadLabel(t *testing.T) {
	t.Setenv("CONCLAVE_ROOM", "acme/env")
	out, _, err := runPrompt(t, []string{"a1"}, true)
	if err != nil || !strings.Contains(out, "messages inbox acme/env") {
		t.Fatalf("got %v:\n%s", err, out)
	}
	if _, _, err := runPrompt(t, []string{"../x", "r"}, true); err == nil {
		t.Error("want an error for a bad label")
	}
}

func TestPrompt_KeepsAnExplicitServer(t *testing.T) {
	t.Setenv("CONCLAVE_BASE_URL", "")
	out, _, err := runPrompt(t, []string{"a1", "r"}, true, "http://127.0.0.1:9000")
	if err != nil || !strings.Contains(out, "conclave-mcp --server http://127.0.0.1:9000 --as a1 messages inbox r --wait 8h") {
		t.Fatalf("server not kept (%v):\n%s", err, out)
	}
	out, _, _ = runPrompt(t, []string{"a1", "r"}, true)
	if strings.Contains(out, "--server") {
		t.Errorf("default server should not be spelled out:\n%s", out)
	}
	t.Setenv("CONCLAVE_BASE_URL", "http://host with space:1")
	out, _, _ = runPrompt(t, []string{"a1", "r"}, true)
	if !strings.Contains(out, "--server 'http://host with space:1' --as a1") {
		t.Errorf("env server not kept and quoted:\n%s", out)
	}
}
