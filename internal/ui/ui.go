// Package ui is the web admin UI: the embedded pages, passkey login,
// and the cookie sessions that let those pages call /v1 as the
// operator key. See docs/web-ui.md.
package ui

import "time"

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
