package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testIdentity(t *testing.T, domain string) *Identity {
	t.Helper()
	id, err := LoadOrCreateIdentity(context.Background(), testDB(t), domain)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSignRequest_RoundTrip(t *testing.T) {
	id := testIdentity(t, "a.example")
	body := []byte(`{"x":1}`)
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/ping?q=1", bytes.NewReader(body))
	if err := id.SignRequest(req, "b.example", body); err != nil {
		t.Fatal(err)
	}
	h := req.Header.Get("Authorization")
	if !regexp.MustCompile(`^Conclave-Sig-v1 origin="a\.example",key="ed25519:[A-Za-z0-9_-]{16}",ts="\d+",nonce="[A-Za-z0-9_-]{22}",sig="[A-Za-z0-9_-]{86}"$`).MatchString(h) {
		t.Fatalf("header: %s", h)
	}
	p, err := ParseSigHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if p.Origin != "a.example" || p.KeyID != id.KeyID {
		t.Errorf("params: %+v", p)
	}
	if d := time.Now().Unix() - p.TS; d < -2 || d > 2 {
		t.Errorf("ts %d is not now", p.TS)
	}
	msg := SigningString(http.MethodPost, "/fed/v1/ping?q=1", "b.example", "a.example", p.TS, p.Nonce, body)
	if !ed25519.Verify(id.Public, msg, p.Sig) {
		t.Error("signature does not verify")
	}

	again := httptest.NewRequest(http.MethodPost, "/fed/v1/ping?q=1", bytes.NewReader(body))
	if err := id.SignRequest(again, "b.example", body); err != nil {
		t.Fatal(err)
	}
	if p2, _ := ParseSigHeader(again.Header.Get("Authorization")); p2.Nonce == p.Nonce {
		t.Error("two signatures share a nonce")
	}
}

func TestSigningString_Format(t *testing.T) {
	got := string(SigningString("POST", "/fed/v1/ping?q=1", "b.example", "a.example", 1700000000, "AAAAAAAAAAAAAAAAAAAAAA", []byte("{}")))
	want := "conclave-sig-v1\nPOST\n/fed/v1/ping?q=1\nb.example\na.example\n1700000000\nAAAAAAAAAAAAAAAAAAAAAA\n" +
		"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got != want {
		t.Errorf("signing string:\n got %q\nwant %q", got, want)
	}
}

// A change to any signed field makes the signature fail.
func TestSigningString_EachFieldIsSigned(t *testing.T) {
	id := testIdentity(t, "a.example")
	type fields struct {
		method, path, dest, origin string
		ts                         int64
		nonce                      string
		body                       string
	}
	base := fields{"POST", "/fed/v1/ping?q=1", "b.example", "a.example", 1700000000, "AAAAAAAAAAAAAAAAAAAAAA", "{}"}
	ss := func(f fields) []byte {
		return SigningString(f.method, f.path, f.dest, f.origin, f.ts, f.nonce, []byte(f.body))
	}
	sig := id.Sign(ss(base))
	if !ed25519.Verify(id.Public, ss(base), sig) {
		t.Fatal("base does not verify")
	}
	for name, change := range map[string]func(*fields){
		"method":      func(f *fields) { f.method = "PUT" },
		"path":        func(f *fields) { f.path = "/fed/v1/pong?q=1" },
		"query":       func(f *fields) { f.path = "/fed/v1/ping?q=2" },
		"destination": func(f *fields) { f.dest = "c.example" },
		"origin":      func(f *fields) { f.origin = "c.example" },
		"ts":          func(f *fields) { f.ts++ },
		"nonce":       func(f *fields) { f.nonce = "BAAAAAAAAAAAAAAAAAAAAA" },
		"body":        func(f *fields) { f.body = "{ }" },
	} {
		f := base
		change(&f)
		if ed25519.Verify(id.Public, ss(f), sig) {
			t.Errorf("a changed %s still verifies", name)
		}
	}
}

func TestParseSigHeader(t *testing.T) {
	sig := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	nonce := "AAAAAAAAAAAAAAAAAAAAAA"
	hdr := func(params ...string) string { return "Conclave-Sig-v1 " + strings.Join(params, ",") }
	origin, key, ts := `origin="a.example"`, `key="ed25519:abc"`, `ts="1700000000"`
	n, s := `nonce="`+nonce+`"`, `sig="`+sig+`"`

	p, err := ParseSigHeader(hdr(origin, key, ts, n, s))
	if err != nil {
		t.Fatal(err)
	}
	if p.Origin != "a.example" || p.KeyID != "ed25519:abc" || p.TS != 1700000000 || p.Nonce != nonce || len(p.Sig) != 64 {
		t.Errorf("parsed: %+v", p)
	}
	// Parameter order does not matter.
	if _, err := ParseSigHeader(hdr(s, n, ts, key, origin)); err != nil {
		t.Errorf("reordered: %v", err)
	}

	for name, h := range map[string]string{
		"empty":             "",
		"other scheme":      "Bearer abc",
		"lowercase scheme":  "conclave-sig-v1 " + strings.Join([]string{origin, key, ts, n, s}, ","),
		"no parameters":     "Conclave-Sig-v1 ",
		"two spaces":        "Conclave-Sig-v1  " + strings.Join([]string{origin, key, ts, n, s}, ","),
		"missing sig":       hdr(origin, key, ts, n),
		"duplicate origin":  hdr(origin, origin, key, ts, n, s),
		"unknown parameter": hdr(origin, key, ts, n, s, `extra="x"`),
		"oversized value":   hdr(`origin="`+strings.Repeat("a", 513)+`"`, key, ts, n, s),
		"bad base64 sig":    hdr(origin, key, ts, n, `sig="!!!"`),
		"short sig":         hdr(origin, key, ts, n, `sig="`+base64.RawURLEncoding.EncodeToString(make([]byte, 63))+`"`),
		"short nonce":       hdr(origin, key, ts, `nonce="AAAAAAAAAAAAAAAAAAAAA"`, s),
		"bad base64 nonce":  hdr(origin, key, ts, `nonce="AAAAAAAAAAAAAAAAAAAA*A"`, s),
		"ts not a number":   hdr(origin, key, `ts="soon"`, n, s),
		"ts with plus":      hdr(origin, key, `ts="+1700000000"`, n, s),
		"ts leading zero":   hdr(origin, key, `ts="01700000000"`, n, s),
		"escape in quotes":  hdr(`origin="a\.example"`, key, ts, n, s),
		"unquoted value":    hdr(`origin=a.example`, key, ts, n, s),
		"trailing comma":    hdr(origin, key, ts, n, s) + ",",
		"space after comma": strings.Join([]string{"Conclave-Sig-v1 " + origin, key, ts, n, s}, ", "),
		"unclosed quote":    hdr(origin, key, ts, n, `sig="`+sig),
		// The decoder skips CR and LF; the nonce is stored per origin,
		// so each value must have its one spelling.
		"nonce with CR": hdr(origin, key, ts, `nonce="`+nonce+"\r"+`"`, s),
		"nonce with LF": hdr(origin, key, ts, `nonce="`+nonce[:10]+"\n"+nonce[10:]+`"`, s),
		"sig with CR":   hdr(origin, key, ts, n, `sig="`+sig+"\r"+`"`),
	} {
		if _, err := ParseSigHeader(h); err == nil {
			t.Errorf("%s: accepted %q", name, h)
		}
	}
}
