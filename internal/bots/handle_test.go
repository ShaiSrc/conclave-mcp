package bots

import (
	"strings"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	for in, want := range map[string]string{
		"a":                     "a",
		"bob":                   "bob",
		"Agent-C":               "agent-c",
		"0day":                  "0day",
		"a.b_c-d":               "a.b_c-d",
		"Directory2":            "directory2",
		strings.Repeat("x", 32): strings.Repeat("x", 32),
	} {
		if got, err := NormalizeHandle(in); err != nil || got != want {
			t.Errorf("NormalizeHandle(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "-a", ".a", "_a", "a b", " a", "a\n", "a@b", "a/b", "a:b",
		strings.Repeat("x", 33),
		"é", "\uFF41", "\u212Aey", // the Kelvin sign lowercases to an ASCII k
		"admin", "operator", "directory", "well-known", "lookup", "key-5", "KEY-12", "ADMIN", "Well-Known",
	} {
		if got, err := NormalizeHandle(in); err == nil {
			t.Errorf("NormalizeHandle(%q) = %q, want an error", in, got)
		}
	}
}

// The name of another server's address may be operator or key-<id> too;
// other reserved names and invalid ones stay refused.
func TestNormalizeRemoteName(t *testing.T) {
	for in, want := range map[string]string{"c1": "c1", "Operator": "operator", "KEY-12": "key-12", "key-x": "key-x"} {
		if got, err := NormalizeRemoteName(in); err != nil || got != want {
			t.Errorf("NormalizeRemoteName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "admin", "lookup", "\u212Aey-1", "key-" + strings.Repeat("1", 29), "a b"} {
		if got, err := NormalizeRemoteName(in); err == nil {
			t.Errorf("NormalizeRemoteName(%q) = %q, want an error", in, got)
		}
	}
}
