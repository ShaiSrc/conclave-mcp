package bots

import (
	"strings"
	"unicode"
)

// Clean removes the characters used to hide or spoof text: C0 and C1
// controls (except a newline when multiline), bidi controls and
// zero-width characters. It then trims surrounding space.
func Clean(s string, multiline bool) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && multiline:
			return r
		case unicode.IsControl(r),
			r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C,
			r >= 0x200B && r <= 0x200D, r == 0x2060, r == 0xFEFF:
			return -1
		}
		return r
	}, s))
}
