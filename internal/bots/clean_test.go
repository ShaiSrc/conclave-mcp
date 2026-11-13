package bots

import (
	"fmt"
	"strings"
	"testing"
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
	if p.KeyID != 0 || p.Handle != "bob" || p.Visibility != Peers {
		t.Errorf("key %d handle %q visibility %q", p.KeyID, p.Handle, p.Visibility)
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
