package ui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/go-chi/chi/v5"
)

// The tests run the UI as if the server listened on port 7432.
const (
	testHost   = "localhost:7432"
	testOrigin = "http://localhost:7432"
)

// newTestUI serves the UI's routes on a test server and returns the
// service, its clock and the server URL. extra mounts more routes.
func newTestUI(t *testing.T, extra ...func(s *Service, r chi.Router)) (*Service, *clock, string) {
	t.Helper()
	s, err := New(testDB(t), 7432)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	s.now = c.now
	r := chi.NewRouter()
	s.Routes(r)
	for _, f := range extra {
		f(s, r)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return s, c, srv.URL
}

// browser is a test client that acts like the UI page: it sends the UI's
// Host, its Origin on writes and the X-Conclave-UI header, and keeps cookies.
type browser struct {
	t       *testing.T
	base    string
	host    string
	origin  string
	cookies map[string]*http.Cookie
}

func newBrowser(t *testing.T, base string) *browser {
	return &browser{t: t, base: base, host: testHost, origin: testOrigin, cookies: map[string]*http.Cookie{}}
}

// noRedirect returns redirects as responses.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request; body is sent as is when it is a string, else as JSON.
func (b *browser) do(method, path string, body any) (int, []byte) {
	b.t.Helper()
	resp := b.send(method, path, body, nil)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// send is do with extra headers ("" deletes one) and the whole response.
func (b *browser) send(method, path string, body any, hdr map[string]string) *http.Response {
	b.t.Helper()
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		j, err := json.Marshal(v)
		if err != nil {
			b.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Host = b.host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderName, "1")
	if method != http.MethodGet {
		req.Header.Set("Origin", b.origin)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	for _, c := range b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return resp
}

// passkey is a software authenticator with one credential, like a
// synced platform passkey (backup eligible and backed up).
type passkey struct {
	rp   virtualwebauthn.RelyingParty
	auth virtualwebauthn.Authenticator
	cred virtualwebauthn.Credential
}

func newPasskey(origin string) *passkey {
	return &passkey{
		rp:   virtualwebauthn.RelyingParty{ID: "localhost", Name: "Conclave", Origin: origin},
		auth: virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true}),
		cred: virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
	}
}

// beginRegistration returns the creation options, or nil and the error response.
func (p *passkey) beginRegistration(b *browser, setupToken string) (*virtualwebauthn.AttestationOptions, int, []byte) {
	b.t.Helper()
	status, body := b.do("POST", "/ui/auth/register/begin", map[string]string{"setup_token": setupToken, "name": "test key"})
	if status != http.StatusOK {
		return nil, status, body
	}
	opts, err := virtualwebauthn.ParseAttestationOptions(string(body))
	if err != nil {
		b.t.Fatalf("creation options: %v\n%s", err, body)
	}
	return opts, status, body
}

func (p *passkey) finishRegistration(b *browser, opts *virtualwebauthn.AttestationOptions) (int, []byte) {
	b.t.Helper()
	p.auth.Options.UserHandle = []byte(opts.UserID) // a discoverable credential keeps the user handle
	return b.do("POST", "/ui/auth/register/finish", virtualwebauthn.CreateAttestationResponse(p.rp, p.auth, p.cred, *opts))
}

// register runs a registration ceremony and returns the last response.
func (p *passkey) register(b *browser, setupToken string) (int, []byte) {
	b.t.Helper()
	opts, status, body := p.beginRegistration(b, setupToken)
	if opts == nil {
		return status, body
	}
	return p.finishRegistration(b, opts)
}

// login runs a discoverable login and returns the last response.
func (p *passkey) login(b *browser) (int, []byte) {
	b.t.Helper()
	status, body := b.do("POST", "/ui/auth/login/begin", nil)
	if status != http.StatusOK {
		return status, body
	}
	opts, err := virtualwebauthn.ParseAssertionOptions(string(body))
	if err != nil {
		b.t.Fatalf("request options: %v\n%s", err, body)
	}
	return b.do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(p.rp, p.auth, p.cred, *opts))
}

// loggedIn sets the UI up with a setup token and returns the logged-in
// browser and its passkey.
func loggedIn(t *testing.T, s *Service, base string) (*browser, *passkey) {
	t.Helper()
	tok, _, err := s.createSetupToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, pk := newBrowser(t, base), newPasskey(testOrigin)
	if status, body := pk.register(b, tok); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	return b, pk
}

// auditActions lists the audit log's actions, oldest first.
func auditActions(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action FROM audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// hasAll reports whether got contains every one of want.
func hasAll(got []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}
