// Package keystore keeps agent API keys out of environment variables
// and shell history: `agent add --store` puts a key here, `--as
// <label>` / CONCLAVE_AGENT reads it back.
//
// macOS uses the login Keychain (service "conclave-agent-<label>"),
// everything else a 0600 file under the user config dir.
package keystore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ErrNotFound: no key is stored for the label.
var ErrNotFound = errors.New("no stored key (create one with: conclave-mcp agent add <label> --store)")

// Seams for tests.
var (
	useKeychain = runtime.GOOS == "darwin"
	configDir   = os.UserConfigDir
)

var labelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// keyRE: what the server issues; anything else could break the
// `security -i` command line.
var keyRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// ValidLabel reports whether label can name a stored key.
func ValidLabel(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("agent label %q: use 1-64 of A-Z a-z 0-9 . _ -, starting with a letter or digit", label)
	}
	return nil
}

// Get returns the key stored for label.
func Get(label string) (string, error) {
	if err := ValidLabel(label); err != nil {
		return "", err
	}
	if useKeychain {
		out, err := exec.Command("security", "find-generic-password", "-s", service(label), "-w").Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return "", fmt.Errorf("agent %q: %w", label, ErrNotFound)
		}
		if err != nil {
			return "", fmt.Errorf("read Keychain item %s: %w", service(label), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	p, err := file(label)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("agent %q: %w", label, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Put stores key for label, replacing an older one.
func Put(label, key string) error {
	if err := ValidLabel(label); err != nil {
		return err
	}
	if !keyRE.MatchString(key) {
		return errors.New("refusing to store a key with unexpected characters")
	}
	if useKeychain {
		// `security -i` reads the command from stdin, so the key never
		// shows up in the process list.
		cmd := exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -a %s -s %s -l %q -w %s\n",
			account(), service(label), "Conclave agent key "+label, key))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("store in Keychain: %w: %s", err, strings.TrimSpace(string(out)))
		}
		// security -i exits 0 even when a command fails; read it back.
		if got, err := Get(label); err != nil || got != key {
			return fmt.Errorf("store in Keychain: item %s not readable after writing", service(label))
		}
		return nil
	}
	p, err := file(label)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p) // CreateTemp files are 0600
}

// Where describes where label's key lives, for messages.
func Where(label string) string {
	if useKeychain {
		return "macOS Keychain item " + service(label)
	}
	p, err := file(label)
	if err != nil {
		return "key file for " + label
	}
	return p
}

func service(label string) string { return "conclave-agent-" + label }

func account() string {
	if u := os.Getenv("USER"); u != "" && labelRE.MatchString(u) {
		return u
	}
	return "conclave"
}

func file(label string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", fmt.Errorf("find config dir: %w", err)
	}
	return filepath.Join(dir, "conclave-mcp", "agents", label+".key"), nil
}
