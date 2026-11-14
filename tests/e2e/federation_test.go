package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/server"
)

// fedEnv is a test server with federation on, in allowlist mode. Its
// domain is its federation listener's own address, over insecure HTTP.
type fedEnv struct {
	*testEnv
	domain string
}

func newFedEnv(t *testing.T) *fedEnv {
	t.Helper()
	var domain string
	env := newTestEnvCfg(t, t.TempDir(), func(c *server.Config) {
		// Picked while the main port is still held, so the two differ.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		domain = ln.Addr().String()
		_ = ln.Close()
		c.FederationDomain = domain
		c.FederationHost = "127.0.0.1"
		c.FederationPort = portFromAddr(domain)
		c.FederationMode = "allowlist"
		c.FederationInsecureHTTP = true
	})
	for i := 0; i < 200; i++ {
		resp, err := http.Get("http://" + domain + "/.well-known/conclave")
		if err == nil {
			resp.Body.Close()
			return &fedEnv{env, domain}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("federation listener on %s did not come up", domain)
	return nil
}

// admin calls the admin API with the env's admin key.
func (f *fedEnv) admin(method, path string, body any) httpResp {
	f.t.Helper()
	return f.callAs(method, path, f.keyA, body)
}

func (f *fedEnv) fingerprint() string {
	f.t.Helper()
	r := f.admin("GET", "/v1/federation", nil)
	var st struct{ Domain, Fingerprint string }
	if r.Status != 200 || json.Unmarshal(r.Body, &st) != nil || st.Domain != f.domain {
		f.t.Fatalf("status: %d %s", r.Status, r.Body)
	}
	return st.Fingerprint
}

// peer adds other as a peer the way an operator does: look up, compare
// the fingerprint with the one the other operator gave, then add.
func (f *fedEnv) peer(other *fedEnv) {
	f.t.Helper()
	r := f.admin("GET", "/v1/federation/lookup?domain="+other.domain, nil)
	var lk struct{ Fingerprint string }
	if r.Status != 200 || json.Unmarshal(r.Body, &lk) != nil {
		f.t.Fatalf("lookup: %d %s", r.Status, r.Body)
	}
	if lk.Fingerprint != other.fingerprint() {
		f.t.Fatalf("lookup fingerprint %q differs from the peer's own", lk.Fingerprint)
	}
	r = f.admin("POST", "/v1/federation/peers", map[string]string{"domain": other.domain, "fingerprint": lk.Fingerprint, "note": "e2e"})
	if r.Status != 201 {
		f.t.Fatalf("add peer: %d %s", r.Status, r.Body)
	}
}

// ping makes f send a signed ping to domain through the admin API.
func (f *fedEnv) ping(domain string) httpResp {
	return f.admin("POST", "/v1/federation/peers/"+domain+"/ping", nil)
}

func (f *fedEnv) auditCount(action string) int {
	f.t.Helper()
	r := f.admin("GET", "/v1/audit?action="+action, nil)
	if r.Status != 200 {
		f.t.Fatalf("audit: %d %s", r.Status, r.Body)
	}
	return strings.Count(string(r.Body), `"`+action+`"`)
}

func TestFederation_TwoServers(t *testing.T) {
	a, b, c := newFedEnv(t), newFedEnv(t), newFedEnv(t)
	refused := func(r httpResp) bool {
		return r.Status == 502 && strings.Contains(string(r.Body), "403 peer_not_allowed")
	}

	// Federation endpoints are admin only.
	if r := a.callAs("GET", "/v1/federation", a.keyB, nil); r.Status != 403 {
		t.Fatalf("non-admin status: %d %s", r.Status, r.Body)
	}
	// A wrong fingerprint adds nothing.
	r := a.admin("POST", "/v1/federation/peers", map[string]string{"domain": b.domain, "fingerprint": c.fingerprint()})
	if r.Status != 409 || !strings.Contains(string(r.Body), "fingerprint_mismatch") {
		t.Fatalf("wrong fingerprint: %d %s", r.Status, r.Body)
	}
	// Not yet peered: B's dialer refuses A, a loopback address of a
	// domain that is not allowed.
	if r := b.ping(a.domain); r.Status != 502 || !strings.Contains(string(r.Body), "non-public address") {
		t.Fatalf("ping before peering: %d %s", r.Status, r.Body)
	}

	a.peer(b)
	b.peer(a)
	for _, p := range []struct{ from, to *fedEnv }{{a, b}, {b, a}} {
		if r := p.from.ping(p.to.domain); r.Status != 200 || !strings.Contains(string(r.Body), `"rtt_ms"`) {
			t.Fatalf("ping %s -> %s: %d %s", p.from.domain, p.to.domain, r.Status, r.Body)
		}
	}
	if n := a.auditCount("federation.peer.add"); n != 1 {
		t.Fatalf("add audit entries on A: %d", n)
	}

	// C allows A, but A does not allow C.
	c.peer(a)
	if r := c.ping(a.domain); !refused(r) {
		t.Fatalf("C ping A: %d %s", r.Status, r.Body)
	}

	// Blocked wins: A blocks B, and B's ping gets 403.
	if r := a.admin("POST", "/v1/federation/peers/"+b.domain+"/block", nil); r.Status != 200 {
		t.Fatalf("block: %d %s", r.Status, r.Body)
	}
	if r := b.ping(a.domain); !refused(r) {
		t.Fatalf("B ping A after block: %d %s", r.Status, r.Body)
	}
	if n := a.auditCount("federation.peer.block"); n != 1 {
		t.Fatalf("block audit entries on A: %d", n)
	}

	// Remove: the peer is gone; a second remove gives 404.
	if r := a.admin("DELETE", "/v1/federation/peers/"+b.domain, nil); r.Status != 204 {
		t.Fatalf("remove: %d %s", r.Status, r.Body)
	}
	if r := a.admin("DELETE", "/v1/federation/peers/"+b.domain, nil); r.Status != 404 {
		t.Fatalf("second remove: %d %s", r.Status, r.Body)
	}
}
