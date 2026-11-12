// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import (
	"database/sql"
	"fmt"
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
	origin string // http://localhost:<port>, the only origin the UI runs on

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
		ceremonies: map[string]ceremony{},
	}, nil
}

// Routes mounts the /ui endpoints on r.
func (s *Service) Routes(r chi.Router) {
	r.Post("/ui/auth/register/begin", s.registerBegin)
	r.Post("/ui/auth/register/finish", s.registerFinish)
	r.Post("/ui/auth/login/begin", s.loginBegin)
	r.Post("/ui/auth/login/finish", s.loginFinish)
	r.Post("/ui/auth/logout", s.logout)
	r.Get("/ui/auth/state", s.state)
	r.Get("/ui/auth/credentials", s.listCredentials)
	r.Delete("/ui/auth/credentials/{id}", s.removeCredential)
}
