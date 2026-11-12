package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// withV1 mounts a stand-in /v1: bearer "admin" is an admin key (id 1),
// bearer "agent" a plain key (id 2); /v1/whoami echoes the claims and
// POST /v1/rooms answers 201.
func withV1(s *Service, r chi.Router) {
	bearer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var c auth.Claims
			switch r.Header.Get("Authorization") {
			case "Bearer admin":
				c = auth.Claims{KeyID: 1, Scopes: []string{auth.ScopeAdmin}}
			case "Bearer agent":
				c = auth.Claims{KeyID: 2}
			default:
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "missing or invalid Authorization header")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey{}, c)))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.Auth(bearer))
		s.V1Routes(r)
		r.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
			c, _ := auth.FromContext(r.Context())
			writeJSON(w, http.StatusOK, map[string]any{"id": c.KeyID, "admin": auth.IsAdmin(r.Context())})
		})
		r.Post("/rooms", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	})
}

func TestSessionAuth_HeaderHostOriginSession(t *testing.T) {
	s, clk, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	opID, err := operatorKeyID(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	var me struct {
		ID    int64 `json:"id"`
		Admin bool  `json:"admin"`
	}
	status, body := b.do("GET", "/v1/whoami", nil)
	if status != http.StatusOK || json.Unmarshal(body, &me) != nil || me.ID != opID || !me.Admin {
		t.Fatalf("whoami with the session token: %d %s, want the operator key %d as admin", status, body, opID)
	}
	for _, c := range []struct {
		name   string
		method string
		hdr    map[string]string
		host   string
		want   int
	}{
		{"no X-Conclave-UI header", "GET", map[string]string{HeaderName: ""}, testHost, 401},
		{"the old constant X-Conclave-UI: 1", "GET", map[string]string{HeaderName: "1"}, testHost, 401},
		{"an unknown token", "GET", map[string]string{HeaderName: "made-up"}, testHost, 401},
		{"wrong Host", "GET", nil, "attacker.example:7432", 403},
		{"other port", "GET", nil, "localhost:9999", 403},
		{"127.0.0.1 is a UI host", "GET", nil, "127.0.0.1:7432", 200},
		{"POST from another origin", "POST", map[string]string{"Origin": "http://attacker.example"}, testHost, 403},
		{"POST with Origin null", "POST", map[string]string{"Origin": "null"}, testHost, 403},
		{"POST without Origin", "POST", map[string]string{"Origin": ""}, testHost, 403},
		{"POST from the UI", "POST", nil, testHost, 201},
	} {
		b.host = c.host
		path := "/v1/whoami"
		if c.method == "POST" {
			path = "/v1/rooms"
		}
		resp := b.send(c.method, path, nil, c.hdr)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	b.host = testHost

	// A bearer key still wins over the session token.
	resp := b.send("GET", "/v1/whoami", nil, map[string]string{"Authorization": "Bearer agent"})
	_ = json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if me.ID != 2 || me.Admin {
		t.Errorf("bearer with a session token: acted as %d (admin %v), want the agent key", me.ID, me.Admin)
	}

	clk.advance(IdleTimeout + time.Minute)
	if status, _ := b.do("GET", "/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("idle session: %d, want 401", status)
	}
}

// A live session token sent only as the old conclave_ui cookie counts
// for nothing: browsers send localhost cookies to every port, so another
// local process could have collected it.
func TestSessionAuth_CookieAloneIsIgnored(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	if !state(t, b).LoggedIn {
		t.Fatal("the token itself is not live")
	}
	c := newBrowser(t, base) // no X-Conclave-UI
	c.cookies["conclave_ui"] = &http.Cookie{Name: "conclave_ui", Value: b.session}
	if status, body := c.do("GET", "/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("whoami with only the cookie: %d %s, want 401", status, body)
	}
	if state(t, c).LoggedIn {
		t.Error("/ui/auth/state: logged in with only the cookie")
	}
	if status, body := c.do("POST", "/ui/auth/register/begin", map[string]string{"name": "x"}); status != http.StatusForbidden {
		t.Errorf("register/begin with only the cookie (a passkey exists): %d %s, want 403", status, body)
	}
}

func TestSessionAuth_RevokedOperatorKey(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = '2026-11-12T10:00:00Z' WHERE label = 'operator'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := b.do("GET", "/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("revoked operator key: %d, want 401", status)
	}
}

func TestSetupTokenEndpoint(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b := newBrowser(t, base)
	post := func(key string) (int, []byte) {
		resp := b.send("POST", "/v1/ui/setup-tokens", nil, map[string]string{"Authorization": "Bearer " + key})
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	if status, _ := post("agent"); status != http.StatusForbidden {
		t.Errorf("agent key: %d, want 403", status)
	}
	status, body := post("admin")
	var tok struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &tok) != nil || len(tok.Token) != 43 {
		t.Fatalf("admin key: %d %s", status, body)
	}
	if exp, err := time.Parse(time.RFC3339Nano, tok.ExpiresAt); err != nil || !exp.Equal(s.now().Add(SetupTokenTTL)) {
		t.Errorf("expires_at %q (%v), want now + 10 minutes", tok.ExpiresAt, err)
	}
	if !hasAll(auditActions(t, s.db), "ui.setup_token") {
		t.Error("no ui.setup_token in the audit log")
	}
	if status, body := newPasskey(testOrigin).register(newBrowser(t, base), tok.Token); status != http.StatusCreated {
		t.Fatalf("register with the endpoint's token: %d %s", status, body)
	}
	if status, _ := post("admin"); status != http.StatusConflict {
		t.Errorf("after setup: %d, want 409", status)
	}
}

func TestLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true, "127.0.0.2": false,
		"": false, "::": false, "0.0.0.0": false, "192.168.1.10": false, "example.com": false,
	} {
		if got := Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}
