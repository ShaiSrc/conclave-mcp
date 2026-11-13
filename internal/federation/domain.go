package federation

import (
	"errors"
	"strconv"
	"strings"
)

// ErrInvalidDomain is returned by NormalizeDomain.
var ErrInvalidDomain = errors.New("invalid federation domain")

// NormalizeDomain returns the canonical form of a federation domain:
// lowercase ASCII letters, digits, dots and hyphens, an optional
// :port from 1 to 65535 without leading zeros, at most 253 characters
// in all. :443 is refused: it is the same server as no port, so it
// would be a second spelling. Labels are non-empty and neither start
// nor end with a hyphen. IDNA names are accepted only in their ASCII
// (xn--) form.
func NormalizeDomain(s string) (string, error) {
	if s == "" || len(s) > 253 {
		return "", ErrInvalidDomain
	}
	// ASCII only: strings.ToLower would fold some non-ASCII letters
	// (U+212A KELVIN SIGN) into ASCII ones, a homoglyph.
	s = strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
	host, port, hasPort := strings.Cut(s, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || n == 443 || port != strconv.Itoa(n) {
			return "", ErrInvalidDomain
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidDomain
		}
		for _, c := range []byte(label) {
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return "", ErrInvalidDomain
			}
		}
	}
	return s, nil
}
