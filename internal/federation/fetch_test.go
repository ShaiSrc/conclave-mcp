package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func always(string) bool { return true }
func never(string) bool  { return false }

// docServer serves id's document for whatever domain the request names
// (its Host), changed by mutate. It returns its port and a hit counter.
func docServer(t *testing.T, mutate func(*Document, http.ResponseWriter) bool) (string, *atomic.Int32) {
	t.Helper()
	id := testIdentity(t, "placeholder.example")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		doc := id.Document()
		doc.Domain = r.Host
		if mutate != nil && !mutate(&doc, w) {
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return port, &hits
}

func TestRefuseNonPublic(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:80", "127.1.2.3:80", "[::1]:80", // loopback
		"10.1.2.3:80", "172.16.0.1:80", "172.31.255.255:80", "192.168.1.1:80", // RFC 1918
		"[fc00::1]:80", "[fd12:3456::1]:80", // RFC 4193
		"169.254.169.254:80", "[fe80::1]:80", // link-local
		"100.64.0.1:80", "100.127.255.255:80", // CGNAT
		"224.0.0.1:80", "239.1.2.3:80", "[ff02::1]:80", // multicast
		"0.0.0.0:80", "[::]:80", // unspecified
		"[::ffff:127.0.0.1]:80", "[::ffff:10.0.0.1]:80", "[::ffff:100.64.0.1]:80", // IPv4-mapped
	} {
		if err := refuseNonPublic("tcp", addr, nil); !errors.Is(err, errNonPublicAddr) {
			t.Errorf("%s: %v, want refused", addr, err)
		}
	}
	for _, addr := range []string{"8.8.8.8:443", "1.1.1.1:443", "100.128.0.1:443", "100.63.255.255:443", "172.32.0.1:443", "[2001:4860:4860::8888]:443"} {
		if err := refuseNonPublic("tcp", addr, nil); err != nil {
			t.Errorf("%s: %v, want allowed", addr, err)
		}
	}
}

// The dialer refuses a private address for a domain that is not
// allowed, also when a name resolves to it, and connects for an
// allowed one.
func TestFetcher_PrivateAddress(t *testing.T) {
	port, hits := docServer(t, nil)
	ctx := context.Background()
	for _, domain := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		if _, err := NewFetcher(true, never).Document(ctx, domain); !errors.Is(err, errNonPublicAddr) {
			t.Errorf("%s, not allowed: %v, want refused", domain, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the server was reached %d times", hits.Load())
	}
	for _, domain := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		doc, err := NewFetcher(true, always).Document(ctx, domain)
		if err != nil {
			t.Errorf("%s, allowed: %v", domain, err)
		} else if doc.Domain != domain || len(doc.Keys) != 1 {
			t.Errorf("%s: document %+v", domain, doc)
		}
	}
}

func TestFetcher_RefusesBadResponses(t *testing.T) {
	ctx := context.Background()
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, mutate := range map[string]func(*Document, http.ResponseWriter) bool{
		"redirect": func(_ *Document, w http.ResponseWriter) bool {
			w.Header().Set("Location", "/.well-known/conclave")
			w.WriteHeader(http.StatusFound)
			return false
		},
		"server error": func(_ *Document, w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		},
		"over 64 KiB": func(d *Document, w http.ResponseWriter) bool {
			b, _ := json.Marshal(d)
			_, _ = w.Write(append(b, strings.Repeat(" ", 64<<10)...))
			return false
		},
		"not json":             func(_ *Document, w http.ResponseWriter) bool { _, _ = w.Write([]byte("<html>")); return false },
		"other domain":         func(d *Document, _ http.ResponseWriter) bool { d.Domain = "other.example"; return true },
		"other protocol":       func(d *Document, _ http.ResponseWriter) bool { d.Protocol = "conclave-federation/2"; return true },
		"no keys":              func(d *Document, _ http.ResponseWriter) bool { d.Keys = nil; return true },
		"fingerprint mismatch": func(d *Document, _ http.ResponseWriter) bool { d.Keys[0].Fingerprint = Fingerprint(other); return true },
		"key id mismatch":      func(d *Document, _ http.ResponseWriter) bool { d.Keys[0].KeyID = KeyIDFor(other); return true },
		"short public key":     func(d *Document, _ http.ResponseWriter) bool { d.Keys[0].PublicKey = "AAAA"; return true },
		"other alg":            func(d *Document, _ http.ResponseWriter) bool { d.Keys[0].Alg = "rsa"; return true },
	} {
		port, hits := docServer(t, mutate)
		if _, err := NewFetcher(true, always).Document(ctx, "127.0.0.1:"+port); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if hits.Load() != 1 {
			t.Errorf("%s: %d requests, want 1 (no redirect followed)", name, hits.Load())
		}
	}
}

func TestFetcher_RefusesInvalidDomain(t *testing.T) {
	for _, domain := range []string{"a.example/x", "user@a.example", "A.example", "a.example?x"} {
		if _, err := NewFetcher(true, always).Document(context.Background(), domain); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("%q: %v", domain, err)
		}
	}
}

// At most 10 fetches a minute for domains that are not allowed;
// allowed domains are not limited.
func TestFetcher_Limit(t *testing.T) {
	ctx := context.Background()
	f := NewFetcher(true, never)
	for i := 0; i < 10; i++ {
		if _, err := f.Document(ctx, "127.0.0.1:1"); err == nil || errors.Is(err, ErrFetchLimited) {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
	}
	if _, err := f.Document(ctx, "127.0.0.1:1"); !errors.Is(err, ErrFetchLimited) {
		t.Errorf("fetch 11: %v, want ErrFetchLimited", err)
	}
	g := NewFetcher(true, always)
	for i := 0; i < 15; i++ {
		if _, err := g.Document(ctx, "127.0.0.1:1"); errors.Is(err, ErrFetchLimited) {
			t.Fatalf("allowed fetch %d was limited", i+1)
		}
	}
}
