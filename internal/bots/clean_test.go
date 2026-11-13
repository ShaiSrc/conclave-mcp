package bots

import "testing"

func TestClean(t *testing.T) {
	for _, c := range []struct {
		name, in  string
		multiline bool
		want      string
	}{
		{"trim", "  plain text  ", false, "plain text"},
		{"C0 and DEL", "a\x00b\x07c\x1bd\x1fe\x7ff", false, "abcdef"},
		{"C1", "a\u0080b\u0085c\u009fd", false, "abcd"},
		{"newline outside descriptions", "line1\nline2", false, "line1line2"},
		{"newline kept in descriptions", "line1\r\nline2\tx", true, "line1\nline2x"},
		{"outer newlines trimmed", "\n\nkeep\n\n", true, "keep"},
		{"bidi embeddings and overrides", "a\u202Ab\u202Bc\u202Cd\u202De\u202Ef", false, "abcdef"},
		{"bidi isolates", "a\u2066b\u2067c\u2068d\u2069e", false, "abcde"},
		{"bidi marks", "a\u200Eb\u200Fc\u061Cd", false, "abcd"},
		{"zero-width", "a\u200Bb\u200Cc\u200Dd\u2060e\uFEFFf", false, "abcdef"},
		{"trim after removal", "\u200B  padded \u202E", false, "padded"},
		{"other text stays", "Grüße, 日本語 ✓ — ok", true, "Grüße, 日本語 ✓ — ok"},
	} {
		if got := Clean(c.in, c.multiline); got != c.want {
			t.Errorf("%s: Clean(%q, %v) = %q, want %q", c.name, c.in, c.multiline, got, c.want)
		}
	}
}
