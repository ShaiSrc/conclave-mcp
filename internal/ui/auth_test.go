package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
)

type uiState struct {
	SetupNeeded bool `json:"setup_needed"`
	LoggedIn    bool `json:"logged_in"`
}

func state(t *testing.T, b *browser) uiState {
	t.Helper()
	var st uiState
	status, body := b.do("GET", "/ui/auth/state", nil)
	if status != http.StatusOK || json.Unmarshal(body, &st) != nil {
		t.Fatalf("state: %d %s", status, body)
	}
	return st
}

// Setup token -> first passkey (creates the operator key, logs in) ->
// login from a second browser -> logout.
func TestCeremonies_SetupLoginLogout(t *testing.T) {
	s, _, base := newTestUI(t)
	b := newBrowser(t, base)
	if st := state(t, b); !st.SetupNeeded || st.LoggedIn {
		t.Fatalf("fresh state %+v, want setup_needed and not logged_in", st)
	}
	tok, _, err := s.createSetupToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pk := newPasskey(testOrigin)
	if status, body := pk.register(b, tok); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	c := b.cookies[CookieName]
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Fatalf("session cookie %+v: want HttpOnly, SameSite=Strict, Path=/, not Secure", c)
	}
	if st := state(t, b); st.SetupNeeded || !st.LoggedIn {
		t.Errorf("state after setup %+v, want logged in", st)
	}
	var trust string
	var admin int
	if err := s.db.QueryRow(`SELECT trust_origin, is_admin FROM api_keys WHERE label = 'operator' AND revoked_at IS NULL`).Scan(&trust, &admin); err != nil || trust != "internal" || admin != 1 {
		t.Errorf("operator key: trust %q admin %d (%v), want an internal admin key", trust, admin, err)
	}

	b2 := newBrowser(t, base)
	if status, body := pk.login(b2); status != http.StatusNoContent || b2.cookies[CookieName] == nil {
		t.Fatalf("login: %d %s, cookie %v", status, body, b2.cookies[CookieName])
	}
	old := *b2.cookies[CookieName]
	if status, _ := b2.do("POST", "/ui/auth/logout", nil); status != http.StatusNoContent || b2.cookies[CookieName] != nil {
		t.Fatalf("logout: %d, cookie %v left", status, b2.cookies[CookieName])
	}
	b2.cookies[CookieName] = &old // the old cookie is dead on the server too
	if st := state(t, b2); st.LoggedIn {
		t.Error("session still live after logout")
	}
	if got, want := auditActions(t, s.db), []string{"ui.passkey.add", "ui.login", "ui.logout"}; !slices.Equal(got, want) {
		t.Errorf("audit %v, want %v", got, want)
	}
}

func TestRegisterGate(t *testing.T) {
	s, clk, base := newTestUI(t)
	ctx := context.Background()
	if status, _ := newPasskey(testOrigin).register(newBrowser(t, base), "made-up"); status != http.StatusForbidden {
		t.Errorf("unknown setup token: %d, want 403", status)
	}
	expired, _, _ := s.createSetupToken(ctx)
	clk.advance(11 * time.Minute)
	if status, _ := newPasskey(testOrigin).register(newBrowser(t, base), expired); status != http.StatusForbidden {
		t.Errorf("expired setup token: %d, want 403", status)
	}

	// Two browsers start with one token: only the first finish uses it.
	tok, _, _ := s.createSetupToken(ctx)
	b1, b2 := newBrowser(t, base), newBrowser(t, base)
	p1, p2 := newPasskey(testOrigin), newPasskey(testOrigin)
	o1, st1, body1 := p1.beginRegistration(b1, tok)
	o2, st2, body2 := p2.beginRegistration(b2, tok)
	if o1 == nil || o2 == nil {
		t.Fatalf("begin: %d %s / %d %s", st1, body1, st2, body2)
	}
	if status, body := p1.finishRegistration(b1, o1); status != http.StatusCreated {
		t.Fatalf("first finish: %d %s", status, body)
	}
	if status, _ := p2.finishRegistration(b2, o2); status != http.StatusForbidden {
		t.Errorf("second finish with the same token: %d, want 403", status)
	}

	// A passkey exists: a new token no longer registers one.
	tok2, _, _ := s.createSetupToken(ctx)
	if status, body := newPasskey(testOrigin).register(newBrowser(t, base), tok2); status != http.StatusForbidden || !strings.Contains(string(body), "log in to add another") {
		t.Errorf("token after setup: %d %s, want 403", status, body)
	}
	// The logged-in operator adds one without a token.
	if status, body := newPasskey(testOrigin).register(b1, ""); status != http.StatusCreated {
		t.Errorf("second passkey from a session: %d %s", status, body)
	}
	if n, _ := s.credentialCount(ctx); n != 2 {
		t.Errorf("%d passkeys, want 2", n)
	}
}

func TestPasskeys_ListAndRemoveNotTheLast(t *testing.T) {
	s, _, base := newTestUI(t)
	b, pk := loggedIn(t, s, base)
	list := func() []string {
		var l struct {
			Credentials []struct {
				ID string `json:"id"`
			} `json:"credentials"`
		}
		status, body := b.do("GET", "/ui/auth/credentials", nil)
		if status != http.StatusOK || json.Unmarshal(body, &l) != nil {
			t.Fatalf("list: %d %s", status, body)
		}
		var ids []string
		for _, c := range l.Credentials {
			ids = append(ids, c.ID)
		}
		return ids
	}
	first := list()
	if len(first) != 1 {
		t.Fatalf("passkeys %v, want 1", first)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusConflict {
		t.Errorf("remove the last passkey: %d, want 409", status)
	}
	if status, body := newPasskey(testOrigin).register(b, ""); status != http.StatusCreated {
		t.Fatalf("second passkey: %d %s", status, body)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNoContent {
		t.Errorf("remove the first of two: %d, want 204", status)
	}
	if status, _ := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNotFound {
		t.Errorf("remove it again: %d, want 404", status)
	}
	if ids := list(); len(ids) != 1 || ids[0] == first[0] {
		t.Errorf("passkeys after removal %v", ids)
	}
	if status, _ := pk.login(newBrowser(t, base)); status != http.StatusUnauthorized {
		t.Errorf("login with the removed passkey: %d, want 401", status)
	}
	if status, _ := newBrowser(t, base).do("GET", "/ui/auth/credentials", nil); status != http.StatusUnauthorized {
		t.Errorf("list without a session: %d, want 401", status)
	}
	if !hasAll(auditActions(t, s.db), "ui.passkey.remove") {
		t.Error("no ui.passkey.remove in the audit log")
	}
}

func TestLogin_WrongOriginAndLateFinish(t *testing.T) {
	s, clk, base := newTestUI(t)
	_, pk := loggedIn(t, s, base)
	evil := *pk
	evil.rp.Origin = "http://evil.example"
	if status, _ := evil.login(newBrowser(t, base)); status != http.StatusUnauthorized {
		t.Errorf("assertion for another origin: %d, want 401", status)
	}
	b := newBrowser(t, base)
	status, body := b.do("POST", "/ui/auth/login/begin", nil)
	if status != http.StatusOK {
		t.Fatalf("begin: %d %s", status, body)
	}
	opts, err := virtualwebauthn.ParseAssertionOptions(string(body))
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(CeremonyTTL + time.Second)
	if status, _ := b.do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(pk.rp, pk.auth, pk.cred, *opts)); status != http.StatusBadRequest {
		t.Errorf("finish after 5 minutes: %d, want 400", status)
	}
}
