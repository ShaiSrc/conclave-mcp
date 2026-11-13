package federation

import (
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	max := strings.Repeat("a.", 126) + "b" // 253 characters
	accept := map[string]string{
		"conclave.example.org":  "conclave.example.org",
		"Conclave.Example.ORG":  "conclave.example.org",
		"localhost":             "localhost",
		"127.0.0.1:7433":        "127.0.0.1:7433",
		"host:1":                "host:1",
		"host:65535":            "host:65535",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"a-b.c-d.e":             "a-b.c-d.e",
		max:                     max,
	}
	for in, want := range accept {
		got, err := NormalizeDomain(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	refuse := []string{
		"",
		".",
		"example..org",
		".example.org",
		"example.org.",
		"-a.example",
		"a-.example",
		"a.-b.example",
		"exa_mple.org",
		"exa mple.org",
		"bücher.example",
		"\u212Aelvin.example", // KELVIN SIGN, which strings.ToLower folds to k
		"https://example.org",
		"example.org/x",
		"[::1]:7433",
		":7433",
		"host:",
		"host:0",
		"host:65536",
		"host:080",
		"host:+80",
		"host:80:80",
		max + "c", // 254 characters
	}
	for _, in := range refuse {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("NormalizeDomain(%q) = %q, want an error", in, got)
		}
	}
}
