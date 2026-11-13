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
	if status, body := pk.register(b, tok); status != http.StatusCreated || len(b.session) != 43 {
		t.Fatalf("register: %d %s, want 201 with a session token", status, body)
	}
	if len(b.cookies) != 0 {
		t.Fatalf("cookies after setup %v, want none (the ceremony cookie is cleared, no session cookie)", b.cookies)
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
	if status, body := pk.login(b2); status != http.StatusOK || b2.session == "" || b2.session == b.session || len(b2.cookies) != 0 {
		t.Fatalf("login: %d %s, cookies %v; want 200 with a new session token and no cookie", status, body, b2.cookies)
	}
	if status, _ := b2.do("POST", "/ui/auth/logout", nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	if st := state(t, b2); st.LoggedIn { // the same token, now dead on the server
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
	if status, body := newPasskey(testOrigin).register(b1, ""); status != http.StatusForbidden || !strings.Contains(string(body), needsConfirm) {
		t.Errorf("second passkey from a session without a confirmation: %d %s, want 403", status, body)
	}
	if status, body := newPasskey(testOrigin).add(b1, p1); status != http.StatusCreated {
		t.Errorf("second passkey from a session, confirmed: %d %s", status, body)
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
	if status, _ := pk.confirmChange(b, "DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusConflict {
		t.Errorf("remove the last passkey: %d, want 409", status)
	}
	pk2 := newPasskey(testOrigin)
	if status, body := pk2.add(b, pk); status != http.StatusCreated {
		t.Fatalf("second passkey: %d %s", status, body)
	}
	if status, body := b.do("DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusForbidden || !strings.Contains(string(body), needsConfirm) {
		t.Errorf("remove without a confirmation: %d %s, want 403", status, body)
	}
	if status, _ := pk2.confirmChange(b, "DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNoContent {
		t.Errorf("remove the first of two: %d, want 204", status)
	}
	if status, _ := pk2.confirmChange(b, "DELETE", "/ui/auth/credentials/"+first[0], nil); status != http.StatusNotFound {
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

// Two browsers begin with two different valid setup tokens. Only the
// first finish may add a passkey: the insert itself guards "no passkey yet".
func TestRegister_SecondSetupTokenLosesRace(t *testing.T) {
	s, _, base := newTestUI(t)
	ctx := context.Background()
	t1, _, _ := s.createSetupToken(ctx)
	t2, _, _ := s.createSetupToken(ctx)
	b1, b2 := newBrowser(t, base), newBrowser(t, base)
	p1, p2 := newPasskey(testOrigin), newPasskey(testOrigin)
	o1, _, _ := p1.beginRegistration(b1, t1)
	o2, _, _ := p2.beginRegistration(b2, t2)
	if o1 == nil || o2 == nil {
		t.Fatal("begin failed")
	}
	if st, body := p1.finishRegistration(b1, o1); st != http.StatusCreated {
		t.Fatalf("first finish: %d %s", st, body)
	}
	if st, body := p2.finishRegistration(b2, o2); st != http.StatusConflict {
		t.Errorf("second finish with another token: %d %s, want 409", st, body)
	}
	if n, _ := s.credentialCount(ctx); n != 1 {
		t.Errorf("%d passkeys, want 1", n)
	}
	if b2.session != "" {
		t.Error("the losing browser got a session")
	}
}

// Without user verification neither registration nor login is accepted,
// and the library's reason stays out of the response.
func TestUserVerificationRequired(t *testing.T) {
	s, _, base := newTestUI(t)
	tok, _, _ := s.createSetupToken(context.Background())
	pk := newPasskey(testOrigin)
	pk.auth.Options.UserNotVerified = true
	st, body := pk.register(newBrowser(t, base), tok)
	if st != http.StatusBadRequest || !strings.Contains(string(body), `"passkey not accepted"`) {
		t.Errorf("register without UV: %d %s, want 400 passkey not accepted", st, body)
	}
	_, good := loggedIn(t, s, base)
	good.auth.Options.UserNotVerified = true
	st, body = good.login(newBrowser(t, base))
	if st != http.StatusUnauthorized || !strings.Contains(string(body), `"passkey not accepted"`) {
		t.Errorf("login without UV: %d %s, want 401 passkey not accepted", st, body)
	}
	var result, detail string
	if err := s.db.QueryRow(`SELECT result, detail FROM audit WHERE action = 'ui.login' AND result != 'ok' ORDER BY id DESC LIMIT 1`).Scan(&result, &detail); err != nil || !strings.HasPrefix(result, "error:") || !strings.Contains(detail, "reason") {
		t.Errorf("failed login audit: %q %q (%v), want an error entry with a reason", result, detail, err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'ui.passkey.add' AND result != 'ok'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d failed ui.passkey.add audit entries, want 1", n)
	}
}

// A sign counter that goes backwards is refused without a session (and
// audited); a changed backup-eligible flag is not accepted.
func TestLogin_CloneAndBackupFlags(t *testing.T) {
	s, _, base := newTestUI(t)
	_, pk := loggedIn(t, s, base)
	pk.cred.Counter = 5
	if st, body := pk.login(newBrowser(t, base)); st != http.StatusOK {
		t.Fatalf("login counter 5: %d %s", st, body)
	}
	pk.cred.Counter = 3
	b := newBrowser(t, base)
	if st, body := pk.login(b); st != http.StatusForbidden || b.session != "" {
		t.Errorf("counter regression: %d %s; want 403 and no session", st, body)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'ui.login' AND result = 'error:clone_warning'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d clone warnings audited, want 1", n)
	}
	pk.cred.Counter = 9
	pk.auth.Options.BackupEligible = false
	pk.auth.Options.BackupState = false
	b = newBrowser(t, base)
	if st, _ := pk.login(b); st != http.StatusUnauthorized || b.session != "" {
		t.Errorf("backup-eligible flip: %d, want 401 and no session", st)
	}
}

// A finish cannot be replayed with the same ceremony cookie, nor used
// for the other kind of ceremony.
func TestCeremony_FinishCannotBeReplayed(t *testing.T) {
	s, _, base := newTestUI(t)
	_, pk := loggedIn(t, s, base)
	b := newBrowser(t, base)
	_, body := b.do("POST", "/ui/auth/login/begin", nil)
	ck := *b.cookies[ceremonyCookie]
	opts, err := virtualwebauthn.ParseAssertionOptions(string(body))
	if err != nil {
		t.Fatal(err)
	}
	resp := virtualwebauthn.CreateAssertionResponse(pk.rp, pk.auth, pk.cred, *opts)
	if st, body := b.do("POST", "/ui/auth/login/finish", resp); st != http.StatusOK {
		t.Fatalf("finish: %d %s", st, body)
	}
	replay := newBrowser(t, base)
	replay.cookies[ceremonyCookie] = &ck
	if st, body := replay.keepSession(replay.do("POST", "/ui/auth/login/finish", resp)); st != http.StatusBadRequest || replay.session != "" {
		t.Errorf("replayed finish: %d %s, want 400 and no session", st, body)
	}
	cross := newBrowser(t, base)
	cross.do("POST", "/ui/auth/login/begin", nil)
	if st, _ := cross.do("POST", "/ui/auth/register/finish", "{}"); st != http.StatusBadRequest {
		t.Errorf("register finish with a login ceremony: %d, want 400", st)
	}
}

// The operator key is the one the UI created and recorded in meta; an
// admin key that merely carries the label is never adopted.
func TestSetup_OperatorKeyIsRecordedNotAdoptedByLabel(t *testing.T) {
	s, _, base := newTestUI(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES ('operator', 'x', 'abcdefgh', '2026-01-01T00:00:00Z', 'internal', 1)`); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := s.createSetupToken(ctx)
	if st, body := newPasskey(testOrigin).register(newBrowser(t, base), tok); st != http.StatusConflict {
		t.Fatalf("setup with a foreign operator key: %d %s, want 409", st, body)
	}
	if n, _ := s.credentialCount(ctx); n != 0 {
		t.Errorf("%d passkeys, want 0", n)
	}

	// After ui-reset renames it, setup creates and records its own key.
	s.db.Exec(`UPDATE api_keys SET label = 'operator-revoked-1', revoked_at = '2026-01-02T00:00:00Z'`)
	tok, _, _ = s.createSetupToken(ctx)
	if st, body := newPasskey(testOrigin).register(newBrowser(t, base), tok); st != http.StatusCreated {
		t.Fatalf("setup: %d %s", st, body)
	}
	var recorded, id int64
	s.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'ui_operator_key_id'`).Scan(&recorded)
	s.db.QueryRow(`SELECT id FROM api_keys WHERE label = 'operator' AND revoked_at IS NULL`).Scan(&id)
	if recorded == 0 || recorded != id {
		t.Errorf("meta ui_operator_key_id %d, operator key %d", recorded, id)
	}
	if got, err := operatorKeyID(ctx, s.db); err != nil || got != id {
		t.Errorf("operatorKeyID %d (%v), want %d", got, err, id)
	}
}

// Begins beyond the cap are refused; expired ones free their slots.
func TestCeremonies_Capped(t *testing.T) {
	_, clk, base := newTestUI(t)
	b := newBrowser(t, base)
	for i := 0; i < maxCeremonies; i++ {
		if st, body := b.do("POST", "/ui/auth/login/begin", nil); st != http.StatusOK {
			t.Fatalf("begin %d: %d %s", i, st, body)
		}
	}
	if st, _ := b.do("POST", "/ui/auth/login/begin", nil); st != http.StatusServiceUnavailable {
		t.Errorf("begin over the cap: %d, want 503", st)
	}
	clk.advance(CeremonyTTL + time.Second)
	if st, _ := b.do("POST", "/ui/auth/login/begin", nil); st != http.StatusOK {
		t.Errorf("begin after expiry: %d, want 200", st)
	}
}

// Removing a passkey ends every other session, not the caller's own.
func TestPasskeys_RemoveEndsOtherSessions(t *testing.T) {
	s, _, base := newTestUI(t)
	b1, pk1 := loggedIn(t, s, base)
	pk2 := newPasskey(testOrigin)
	if st, body := pk2.add(b1, pk1); st != http.StatusCreated {
		t.Fatalf("second passkey: %d %s", st, body)
	}
	b2 := newBrowser(t, base)
	if st, body := pk1.login(b2); st != http.StatusOK {
		t.Fatalf("login with the first passkey: %d %s", st, body)
	}
	var l struct {
		Credentials []struct {
			ID string `json:"id"`
		} `json:"credentials"`
	}
	_, body := b1.do("GET", "/ui/auth/credentials", nil)
	if json.Unmarshal(body, &l) != nil || len(l.Credentials) != 2 {
		t.Fatalf("list: %s", body)
	}
	if st, _ := pk2.confirmChange(b1, "DELETE", "/ui/auth/credentials/"+l.Credentials[0].ID, nil); st != http.StatusNoContent {
		t.Fatalf("remove: %d", st)
	}
	if !state(t, b1).LoggedIn {
		t.Error("the caller's own session ended")
	}
	if state(t, b2).LoggedIn {
		t.Error("another session survived the removal")
	}
}
