package fedrooms

import (
	"strings"
	"testing"
)

// FuzzNormalizeDomain: a domain that normalizes is in canonical form
// once normalized.
func FuzzNormalizeDomain(f *testing.F) {
	for _, s := range []string{"conclave.example.org", "Conclave.Example.ORG", "127.0.0.1:7433", "host:443", "host:080",
		"xn--bcher-kva.example", "Kelvin.example", "[::1]:7433", "a..b", "-a.b", strings.Repeat("a.", 126) + "b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := NormalizeDomain(s)
		if err != nil {
			return
		}
		if again, err := NormalizeDomain(d); err != nil || again != d {
			t.Fatalf("%q normalizes to %q, which normalizes to %q, %v", s, d, again, err)
		}
	})
}
