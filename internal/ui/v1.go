package ui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Auth wraps the bearer middleware of /v1. A request without an
// Authorization header that carries X-Conclave-UI acts as the operator
// key if that header holds a session token (else 401), the request has a
// UI Host (else 403), the UI Origin unless it is a GET (else 403) and
// the session is live (else 401). A session alone only reads: anything
// but a GET needs a passkey confirmation (else 403), which only
// confirmFinish can mark. Every other request goes to bearer unchanged;
// a session cookie counts for nothing.
func (s *Service) Auth(bearer func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		viaBearer := bearer(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" || len(r.Header.Values(HeaderName)) == 0 {
				viaBearer.ServeHTTP(w, r)
				return
			}
			token := sessionToken(r)
			if token == "" || token == "1" {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "X-Conclave-UI must carry the session token from a passkey login")
				return
			}
			if !s.uiHost(r.Host) {
				writeErr(w, r, http.StatusForbidden, "forbidden", "host not allowed")
				return
			}
			if r.Method != http.MethodGet && r.Header.Get("Origin") != s.origin {
				writeErr(w, r, http.StatusForbidden, "forbidden", "origin not allowed")
				return
			}
			live, err := s.session(r.Context(), token)
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
				return
			}
			if !live {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "session expired: log in again")
				return
			}
			id, err := operatorKeyID(r.Context(), s.db)
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "the operator key is revoked or missing")
				return
			}
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "operator key lookup")
				return
			}
			if r.Method != http.MethodGet && !confirmed(r) {
				writeErr(w, r, http.StatusForbidden, "forbidden", needsConfirm)
				return
			}
			ctx := context.WithValue(r.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: id, Scopes: []string{auth.ScopeAdmin, auth.ScopeUI}})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// V1Routes mounts the UI's /v1 endpoint on the /v1 router.
func (s *Service) V1Routes(r chi.Router) {
	r.Post("/ui/setup-tokens", s.newSetupToken)
}

// newSetupToken answers POST /v1/ui/setup-tokens (admin keys): a token
// that registers the first passkey, valid SetupTokenTTL. 409 once a
// passkey exists: more are added from a logged-in session.
func (s *Service) newSetupToken(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAdmin(r.Context()) {
		writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
		return
	}
	ctx := r.Context()
	n, err := s.credentialCount(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "count passkeys")
		return
	}
	if n > 0 {
		writeErr(w, r, http.StatusConflict, "conflict",
			"a passkey is registered: add more on the Passkeys page, or run admin ui-reset (service stopped) to start over")
		return
	}
	token, expires, err := s.createSetupToken(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "create setup token")
		return
	}
	c, _ := auth.FromContext(ctx)
	exp := storage.FormatTS(expires)
	detail, _ := json.Marshal(map[string]string{"expires_at": exp})
	_ = audit.Write(ctx, s.db, c.KeyID, "ui.setup_token", "ui", "setup_token", "", "ok", string(detail))
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "expires_at": exp})
}
