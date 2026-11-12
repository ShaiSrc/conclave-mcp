// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Names the browser, the server and the CLI agree on.
const (
	CookieName    = "conclave_ui"   // the session cookie
	HeaderName    = "X-Conclave-UI" // /v1 takes the cookie only with this header set to 1
	OperatorLabel = "operator"      // the admin key UI sessions act as
)

// Timeouts.
const (
	SetupTokenTTL = 10 * time.Minute // a setup token from ui setup
	CeremonyTTL   = 5 * time.Minute  // between begin and finish of a passkey ceremony
	IdleTimeout   = time.Hour        // a session unused this long ends
	MaxSession    = 12 * time.Hour   // a session ends this long after login
)

// Service serves /ui: the passkey ceremonies and sessions.
type Service struct {
	store
	wa     *webauthn.WebAuthn
	origin string    // http://localhost:<port>, the only origin the UI runs on
	hosts  [2]string // the Host headers /ui and cookie sessions accept: localhost:<port>, 127.0.0.1:<port>

	mu         sync.Mutex
	ceremonies map[string]ceremony // by hashToken(ceremony cookie)
}

// New returns the UI of a server listening on a loopback address at
// port. The relying party is localhost: an IP address cannot be one.
func New(db *sql.DB, port int) (*Service, error) {
	origin := fmt.Sprintf("http://localhost:%d", port)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  "localhost",
		RPDisplayName:         "Conclave",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: CeremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{
		store:      store{db: db, now: time.Now},
		wa:         wa,
		origin:     origin,
		hosts:      [2]string{fmt.Sprintf("localhost:%d", port), fmt.Sprintf("127.0.0.1:%d", port)},
		ceremonies: map[string]ceremony{},
	}, nil
}

// Loopback reports whether the listen host is a loopback address. The
// UI and cookie sessions exist only then: passkeys need a secure
// context, and plain HTTP is one only on localhost.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// uiHost reports whether host (a request's Host header) is the UI's.
func (s *Service) uiHost(host string) bool { return host == s.hosts[0] || host == s.hosts[1] }

//go:embed assets
var assets embed.FS

// csp allows the page's own files only: no inline script or style, no
// framing.
const csp = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// Routes mounts /ui on r: the pages under /ui/ and the passkey and
// session endpoints under /ui/auth/, all behind guard.
func (s *Service) Routes(r chi.Router) {
	files, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	pages := http.StripPrefix("/ui/", http.FileServer(http.FS(files)))
	r.Route("/ui", func(r chi.Router) {
		r.Use(s.guard)
		// Unmatched requests are answered behind the guard too.
		r.NotFound(func(w http.ResponseWriter, req *http.Request) {
			writeErr(w, req, http.StatusNotFound, "not_found", "not found")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
			writeErr(w, req, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		})
		r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/ui" {
				http.Redirect(w, req, "/ui/", http.StatusFound)
				return
			}
			pages.ServeHTTP(w, req)
		})
		r.Post("/auth/register/begin", s.registerBegin)
		r.Post("/auth/register/finish", s.registerFinish)
		r.Post("/auth/login/begin", s.loginBegin)
		r.Post("/auth/login/finish", s.loginFinish)
		r.Post("/auth/logout", s.logout)
		r.Get("/auth/state", s.state)
		r.Get("/auth/credentials", s.listCredentials)
		r.Delete("/auth/credentials/{id}", s.removeCredential)
	})
}

// guard runs before every /ui request: the security headers, the Host
// check (DNS rebinding), the redirect from 127.0.0.1 to localhost (the
// passkeys' relying party), the Origin check for writes (CSRF) and a
// body limit.
func (s *Service) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if !s.uiHost(r.Host) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "host not allowed")
			return
		}
		read := r.Method == http.MethodGet
		if read && r.Host == s.hosts[1] {
			http.Redirect(w, r, s.origin+r.URL.RequestURI(), http.StatusFound)
			return
		}
		if !read && r.Header.Get("Origin") != s.origin {
			writeErr(w, r, http.StatusForbidden, "forbidden", "origin not allowed")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}
