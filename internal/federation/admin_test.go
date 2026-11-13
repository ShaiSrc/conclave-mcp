package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
)

func adminDo(svc *Service, scopes []string, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: 1, Scopes: scopes})
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		if svc != nil {
			svc.AdminRoutes(r)
		} else {
			DisabledRoutes(r)
		}
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

var admin = []string{auth.ScopeAdmin}

func TestAdmin_NonAdminForbidden(t *testing.T) {
	svc := newLocal(t, "allowlist")
	wantErr(t, adminDo(svc, []string{"read"}, "GET", "/v1/federation", ""), 403, "forbidden")
	wantErr(t, adminDo(svc, nil, "POST", "/v1/federation/peers", "{}"), 403, "forbidden")
}

func TestAdmin_Disabled(t *testing.T) {
	for _, c := range [][2]string{{"GET", "/v1/federation"}, {"GET", "/v1/federation/peers"}, {"GET", "/v1/federation/lookup?domain=x.example"},
		{"POST", "/v1/federation/peers"}, {"POST", "/v1/federation/peers/x.example/block"},
		{"DELETE", "/v1/federation/peers/x.example"}, {"POST", "/v1/federation/peers/x.example/ping"}} {
		wantErr(t, adminDo(nil, admin, c[0], c[1], ""), 404, "federation_disabled")
	}
}

func TestAdmin_StatusAndList(t *testing.T) {
	svc := newLocal(t, "allowlist")
	ctx := context.Background()
	_ = svc.peers.Allow(ctx, "a.example", "SHA256:x", "n")
	_ = svc.peers.Block(ctx, "b2.example")
	_, _ = svc.peers.RecordSeen(ctx, "c.example", "SHA256:y")
	var st struct {
		Domain, Mode, Fingerprint string
		KeyID                     string `json:"key_id"`
		Peers                     map[string]int
	}
	rec := adminDo(svc, admin, "GET", "/v1/federation", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || st.Domain != local || st.Mode != "allowlist" || st.Fingerprint != svc.id.Fingerprint() ||
		st.KeyID != svc.id.KeyID || st.Peers["allowed"] != 1 || st.Peers["blocked"] != 1 || st.Peers["seen"] != 1 {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}
	var l struct{ Peers []peerView }
	rec = adminDo(svc, admin, "GET", "/v1/federation/peers", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	if rec.Code != 200 || len(l.Peers) != 3 || l.Peers[0].Domain != "a.example" || l.Peers[0].Note != "n" {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
}

func TestAdmin_LookupAndAdd(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	rec := adminDo(svc, admin, "GET", "/v1/federation/lookup?domain="+rm.domain(), "")
	var lk struct{ Domain, Fingerprint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &lk)
	if rec.Code != 200 || lk.Fingerprint != rm.id.Fingerprint() {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}

	wrong := `{"domain":"` + rm.domain() + `","fingerprint":"SHA256:nope"}`
	wantErr(t, adminDo(svc, admin, "POST", "/v1/federation/peers", wrong), 409, "fingerprint_mismatch")
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peers`); n != 0 {
		t.Errorf("peers after mismatch = %d", n)
	}
	wantErr(t, adminDo(svc, admin, "POST", "/v1/federation/peers", `{"domain":"`+local+`","fingerprint":"SHA256:x"}`), 400, "own_domain")
	wantErr(t, adminDo(svc, admin, "POST", "/v1/federation/peers", `{"domain":"`+rm.domain()+`"}`), 400, "fingerprint_required")

	ok := `{"domain":"` + rm.domain() + `","fingerprint":"` + rm.id.Fingerprint() + `","note":"hi"}`
	rec = adminDo(svc, admin, "POST", "/v1/federation/peers", ok)
	var p peerView
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 201 || p.Status != "allowed" || p.Note != "hi" {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action='federation.peer.add' AND target_id=?`, rm.domain()); n != 1 {
		t.Errorf("add audit rows = %d", n)
	}
}

func TestAdmin_BlockRemovePing(t *testing.T) {
	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	rec := adminDo(svc, admin, "POST", "/v1/federation/peers/"+rm.domain()+"/ping", "")
	var pg struct {
		Domain string
		RTT    *int64 `json:"rtt_ms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pg)
	if rec.Code != 200 || pg.Domain != rm.domain() || pg.RTT == nil {
		t.Errorf("ping: %d %s", rec.Code, rec.Body)
	}
	wantErr(t, adminDo(svc, admin, "POST", "/v1/federation/peers/127.0.0.1:1/ping", ""), 502, "peer_unreachable")

	rec = adminDo(svc, admin, "POST", "/v1/federation/peers/"+rm.domain()+"/block", "")
	var p peerView
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || p.Status != "blocked" {
		t.Errorf("block: %d %s", rec.Code, rec.Body)
	}
	if rec := adminDo(svc, admin, "DELETE", "/v1/federation/peers/"+rm.domain(), ""); rec.Code != 204 {
		t.Errorf("remove: %d", rec.Code)
	}
	wantErr(t, adminDo(svc, admin, "DELETE", "/v1/federation/peers/"+rm.domain(), ""), 404, "peer_not_found")
	for _, a := range []string{"federation.peer.block", "federation.peer.remove"} {
		if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action=? AND target_id=?`, a, rm.domain()); n != 1 {
			t.Errorf("%s rows = %d", a, n)
		}
	}
}
