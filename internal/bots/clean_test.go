package bots

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	for _, c := range []struct {
		name, in  string
		multiline bool
		want      string
	}{
		{"trim", "  plain text  ", false, "plain text"},
		{"C0 and DEL", "a\x00b\x07c\x1bd\x1fe\x7ff", false, "abcdef"},
		{"C1", "a\u0080b\u0085c\u009fd", false, "abcd"},
		{"tab", "a\tb", true, "ab"},
		{"newline outside descriptions", "line1\nline2", false, "line1line2"},
		{"newline kept in descriptions", "line1\nline2", true, "line1\nline2"},
		{"CRLF and CR in descriptions", "a\r\nb\rc", true, "a\nb\nc"},
		{"CRLF and CR outside descriptions", "a\r\nb\rc", false, "abc"},
		{"line and paragraph separators in descriptions", "a\u2028b\u2029c", true, "a\nb\nc"},
		{"line and paragraph separators outside descriptions", "a\u2028b\u2029c", false, "abc"},
		{"outer line breaks trimmed", "\n\u2029keep\r\n\u2028", true, "keep"},
		{"bidi embeddings and overrides", "a\u202Ab\u202Bc\u202Cd\u202De\u202Ef", false, "abcdef"},
		{"bidi isolates", "a\u2066b\u2067c\u2068d\u2069e", false, "abcde"},
		{"bidi marks", "a\u200Eb\u200Fc\u061Cd", false, "abcd"},
		{"zero-width", "a\u200Bb\u200Cc\u200Dd\u2060e\uFEFFf", false, "abcdef"},
		{"soft hyphen and Mongolian vowel separator", "a\u00ADb\u180Ec", false, "abc"},
		{"invisible operators", "a\u2061b\u2062c\u2063d\u2064e", false, "abcde"},
		{"tag block, unassigned code points too", "a\U000E0000b\U000E0001c\U000E0002d\U000E0020e\U000E0041f\U000E007Fg", false, "abcdefg"},
		{"variation selectors", "a\uFE00b\uFE0Ec\uFE0Fd", false, "abcd"},
		{"variation selectors supplement", "a\U000E0100b\U000E01EFc", false, "abc"},
		{"other format characters", "a\uFFF9b\U000110BDc\U0001D173d", true, "abcd"},
		{"trim after removal", "\u200B  padded \u202E", false, "padded"},
		{"other text stays", "Grüße, 日本語 ✓ — ok 😀", true, "Grüße, 日本語 ✓ — ok 😀"},
	} {
		if got := Clean(c.in, c.multiline); got != c.want {
			t.Errorf("%s: Clean(%q, %v) = %q, want %q", c.name, c.in, c.multiline, got, c.want)
		}
	}
	// Every format character goes, whatever its block.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Cf, r) {
			if got := Clean("a"+string(r)+"b", true); got != "ab" {
				t.Errorf("format character %U kept: %q", r, got)
			}
		}
	}
}

// A remote profile is cleaned like a local one and truncated instead
// of refused; skills with an invalid id are dropped.
func TestCleanRemote(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	skills := []Skill{{ID: "ok", Name: "a‮b", Description: "x\ny​"}, {ID: "Bad Id"}, {ID: ""}, {ID: "x@y"}, {ID: "long", Name: long(70), Description: long(310)}}
	for i := 0; i < 30; i++ {
		skills = append(skills, Skill{ID: fmt.Sprintf("s%d", i)})
	}
	p, ok := CleanRemote(Profile{KeyID: 7, Handle: "Bob", DisplayName: "‮bob\x00 " + long(70), Description: "l1\nl2⁦" + long(1100),
		Skills: skills, Visibility: "private\x1b"})
	if !ok {
		t.Fatal("valid handle refused")
	}
	if p.KeyID != 0 || p.Handle != "bob" || p.Visibility != Peers || p.TrustOrigin != "external" {
		t.Errorf("key %d handle %q visibility %q trust origin %q", p.KeyID, p.Handle, p.Visibility, p.TrustOrigin)
	}
	if want := "bob " + long(60); p.DisplayName != want {
		t.Errorf("display name %q", p.DisplayName)
	}
	if !strings.HasPrefix(p.Description, "l1\nl2é") || utf8.RuneCountInString(p.Description) != 1000 {
		t.Errorf("description %q (%d)", p.Description[:10], utf8.RuneCountInString(p.Description))
	}
	if len(p.Skills) != 20 || p.Skills[0] != (Skill{ID: "ok", Name: "ab", Description: "x\ny"}) ||
		p.Skills[1].ID != "long" || p.Skills[1].Name != long(64) || p.Skills[1].Description != long(300) || p.Skills[2].ID != "s0" {
		t.Errorf("skills %+v", p.Skills[:3])
	}
	if p, _ := CleanRemote(Profile{Handle: "pub", Visibility: Public}); p.Visibility != Public || p.Skills == nil {
		t.Errorf("public profile: %+v", p)
	}
	for _, h := range []string{"", "bad handle", "x@other.example", "admin", "lookup", "-x"} {
		if _, ok := CleanRemote(Profile{Handle: h}); ok {
			t.Errorf("handle %q accepted", h)
		}
	}
}
