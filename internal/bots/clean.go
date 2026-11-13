package bots

import (
	"strings"
	"unicode"
)

// lineBreaks turns every line break into \n.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n")

// Clean removes the characters used to hide or spoof text: C0 and C1
// controls, every format character (Unicode Cf: bidi controls,
// zero-width characters, the soft hyphen, invisible operators, tags),
// the whole tag block and the variation selectors. Line breaks (\r\n,
// \r, U+2028, U+2029) become \n when multiline and go otherwise. It then
// trims surrounding space.
func Clean(s string, multiline bool) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && multiline:
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r),
			r >= 0xE0000 && r <= 0xE007F,                             // tag block, unassigned code points too
			r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
			return -1
		}
		return r
	}, lineBreaks.Replace(s)))
}
