package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func useFileStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldK, oldD := useKeychain, configDir
	useKeychain = false
	configDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { useKeychain, configDir = oldK, oldD })
	return dir
}

func TestFileStore_PutGet(t *testing.T) {
	dir := useFileStore(t)
	if _, err := Get("agent-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put: err=%v, want ErrNotFound", err)
	}
	if err := Put("agent-a", "tk_secret.1"); err != nil {
		t.Fatal(err)
	}
	if err := Put("agent-a", "tk_secret.2"); err != nil { // replaces
		t.Fatal(err)
	}
	got, err := Get("agent-a")
	if err != nil || got != "tk_secret.2" {
		t.Fatalf("Get = %q, %v; want tk_secret.2", got, err)
	}
	if runtime.GOOS != "windows" {
		p := filepath.Join(dir, "conclave-mcp", "agents", "agent-a.key")
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v, want 0600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(p))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("agents dir mode %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestBadLabelsAreRejected(t *testing.T) {
	useFileStore(t)
	for _, l := range []string{"", "../x", "a/b", ".hidden", "with space", strings.Repeat("a", 65)} {
		if err := Put(l, "k"); err == nil {
			t.Errorf("Put(%q): want error", l)
		}
		if _, err := Get(l); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): want a label error, got %v", l, err)
		}
	}
	if err := ValidLabel("claude-peer_2.b"); err != nil {
		t.Errorf("ValidLabel: %v", err)
	}
}

// fakeSecurity puts a `security` stub on PATH that keeps one item in
// a file, logs its argv and returns exit 44 (not found) when empty.
func fakeSecurity(t *testing.T) (argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	script := `#!/bin/sh
echo "$@" >> "` + argvLog + `"
store="` + filepath.Join(dir, "item") + `"
if [ "$1" = "-i" ]; then cat > "$store"; exit 0; fi
if [ "$1" = "find-generic-password" ]; then
  [ -s "$store" ] || exit 44
  sed -n 's/.* -w \([^ ]*\).*/\1/p' "$store"; exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := useKeychain
	useKeychain = true
	t.Cleanup(func() { useKeychain = old })
	return argvLog
}

func TestKeychain_KeyNeverInArgv(t *testing.T) {
	argv := fakeSecurity(t)
	if _, err := Get("agent-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put: err=%v, want ErrNotFound", err)
	}
	if err := Put("agent-b", "tk_topsecret"); err != nil {
		t.Fatal(err)
	}
	got, err := Get("agent-b")
	if err != nil || got != "tk_topsecret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	log, _ := os.ReadFile(argv)
	if strings.Contains(string(log), "tk_topsecret") {
		t.Errorf("key leaked into argv:\n%s", log)
	}
	if !strings.Contains(string(log), "-s conclave-agent-agent-b") {
		t.Errorf("service name not used:\n%s", log)
	}
	if w := Where("agent-b"); !strings.Contains(w, "conclave-agent-agent-b") {
		t.Errorf("Where = %q", w)
	}
}
