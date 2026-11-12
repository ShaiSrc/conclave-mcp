package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// ceremonyCookie links a ceremony's begin and finish requests.
const ceremonyCookie = "conclave_ui_ceremony"

// maxCeremonies caps the live ceremonies kept in memory.
const maxCeremonies = 64

// operator is the one WebAuthn user: every passkey is the operator's.
type operator struct {
	handle []byte
	creds  []webauthn.Credential
}

func (o operator) WebAuthnID() []byte                         { return o.handle }
func (o operator) WebAuthnName() string                       { return OperatorLabel }
func (o operator) WebAuthnDisplayName() string                { return "Conclave operator" }
func (o operator) WebAuthnCredentials() []webauthn.Credential { return o.creds }

// ceremonyKind tells which finish step a ceremony is for. The zero
// value matches none.
type ceremonyKind int

const (
	kindLogin ceremonyKind = iota + 1
	kindRegister
	kindMessage
)

// ceremony is what a begin step keeps in memory for its finish step.
type ceremony struct {
	kind       ceremonyKind
	data       webauthn.SessionData
	setupToken string // registration with a setup token; "" when logged in
	name       string // passkey name (registration)
	roomID     string // message: the room it goes to
	message    []byte // message: the send body, the only thing finish posts
	session    string // message: hashToken of the session that began it
	expires    time.Time
}

// errOperatorUnusable: the label is taken by a key the UI cannot act as.
// No API renames keys; admin ui-reset renames revoked operator keys.
var errOperatorUnusable = errors.New(`a key labelled "operator" exists that the UI did not create: revoke it if it is active (conclave-mcp keys revoke <id>), stop the service, run conclave-mcp admin ui-reset, start the service and run conclave-mcp ui setup again`)

// metaOperatorKey is the meta key that records the id of the operator
// key the UI created. The label alone proves nothing: an admin key made
// by hand may carry it, and its secret is known.
const metaOperatorKey = "ui_operator_key_id"

// operatorKeyID returns the id of the operator key the UI created,
// provided it is still an active admin key.
func operatorKeyID(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT k.id FROM meta m JOIN api_keys k ON k.id = CAST(m.value AS INTEGER)
		 WHERE m.key = ? AND k.is_admin = 1 AND k.revoked_at IS NULL`, metaOperatorKey).Scan(&id)
	return id, err
}

// ensureOperatorKey returns the operator key, creating it on first use:
// an admin key whose secret is random and never shown or stored in
// plain text, so only UI sessions can act as it.
func ensureOperatorKey(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	id, err := operatorKeyID(ctx, db)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	plaintext, phc, err := auth.GenerateToken()
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, is_admin) VALUES (?, ?, ?, ?, 'internal', 1)`,
		OperatorLabel, phc, plaintext[3:11], now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			// A parallel setup may have created the recorded key just now.
			if id, rerr := operatorKeyID(ctx, db); rerr == nil {
				return id, nil
			}
			return 0, errOperatorUnusable
		}
		return 0, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaOperatorKey, strconv.FormatInt(id, 10)); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// credID is the stored form of a credential id.
func credID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// userHandle is the operator's WebAuthn user handle: 32 random bytes,
// created once and kept in meta (ui_user_handle).
func (s *store) userHandle(ctx context.Context) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('ui_user_handle', ?) ON CONFLICT(key) DO NOTHING`,
		base64.RawURLEncoding.EncodeToString(b)); err != nil {
		return nil, err
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'ui_user_handle'`).Scan(&v); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(v)
}

// credentials returns the stored passkeys in the library's form.
func (s *store) credentials(ctx context.Context) ([]webauthn.Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_key, sign_count, transports, flags FROM ui_credentials ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var (
			id, transports string
			pk             []byte
			count          int64
			flags          int
		)
		if err := rows.Scan(&id, &pk, &count, &transports, &flags); err != nil {
			return nil, err
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil {
			return nil, err
		}
		c := webauthn.Credential{
			ID:            raw,
			PublicKey:     pk,
			Flags:         webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(flags)),
			Authenticator: webauthn.Authenticator{SignCount: uint32(count)},
		}
		for _, t := range strings.Split(transports, ",") {
			if t != "" {
				c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// credentialCount is the number of registered passkeys.
func (s *store) credentialCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_credentials`).Scan(&n)
	return n, err
}

// addCredential stores a new passkey. With first set it inserts only
// while no passkey exists (the setup-token path) and reports false
// otherwise.
func (s *store) addCredential(ctx context.Context, c *webauthn.Credential, name string, first bool) (bool, error) {
	transports := make([]string, len(c.Transport))
	for i, t := range c.Transport {
		transports[i] = string(t)
	}
	q := `INSERT INTO ui_credentials (id, public_key, sign_count, transports, flags, name, created_at) SELECT ?, ?, ?, ?, ?, ?, ?`
	if first {
		q += ` WHERE NOT EXISTS (SELECT 1 FROM ui_credentials)`
	}
	res, err := s.db.ExecContext(ctx, q, credID(c.ID), c.PublicKey, c.Authenticator.SignCount,
		strings.Join(transports, ","), int(c.Flags.ProtocolValue()), name, storage.FormatTS(s.now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// operator loads the WebAuthn user with its passkeys.
func (s *Service) operator(ctx context.Context) (operator, error) {
	h, err := s.userHandle(ctx)
	if err != nil {
		return operator{}, err
	}
	creds, err := s.credentials(ctx)
	return operator{handle: h, creds: creds}, err
}

// sessionToken is the session token r carries in X-Conclave-UI. No
// cookie is read: browsers send localhost cookies to every port, so any
// local process could collect one.
func sessionToken(r *http.Request) string { return r.Header.Get(HeaderName) }

// loggedIn reports whether r carries a live session token.
func (s *Service) loggedIn(r *http.Request) (bool, error) {
	return s.session(r.Context(), sessionToken(r))
}

// requireSession answers 401 unless r is logged in.
func (s *Service) requireSession(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return false
	}
	if !ok {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "log in first")
	}
	return ok
}

// auditFail records a refused passkey attempt: the reason stays on the
// server, the client only sees a fixed message.
func (s *Service) auditFail(ctx context.Context, action, result, reason string) {
	keyID, _ := operatorKeyID(ctx, s.db)
	detail, _ := json.Marshal(map[string]string{"reason": reason})
	_ = audit.Write(ctx, s.db, keyID, action, "passkey", "", "", result, string(detail))
}

// startCeremony keeps c for the finish step under a fresh cookie. It
// writes the error response itself and reports false on failure.
func (s *Service) startCeremony(w http.ResponseWriter, r *http.Request, c ceremony) bool {
	token, hash, err := newToken()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "start ceremony")
		return false
	}
	now := s.now()
	c.expires = now.Add(CeremonyTTL)
	s.mu.Lock()
	// ponytail: at most maxCeremonies live entries and an O(n) sweep per
	// begin (n <= 64); a flood is refused with 503 until entries expire.
	for k, v := range s.ceremonies {
		if !now.Before(v.expires) {
			delete(s.ceremonies, k)
		}
	}
	if len(s.ceremonies) >= maxCeremonies {
		s.mu.Unlock()
		writeErr(w, r, http.StatusServiceUnavailable, "unavailable", "too many passkey ceremonies in progress: try again in a few minutes")
		return false
	}
	s.ceremonies[hash] = c
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: token, Path: "/ui/auth/",
		MaxAge: int(CeremonyTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return true
}

// takeCeremony returns the request's ceremony of the given kind and
// forgets it: every ceremony finishes at most once.
func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, kind ceremonyKind) (ceremony, bool) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Path: "/ui/auth/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	ck, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	hash := hashToken(ck.Value)
	s.mu.Lock()
	c, ok := s.ceremonies[hash]
	delete(s.ceremonies, hash)
	s.mu.Unlock()
	return c, ok && c.kind == kind && s.now().Before(c.expires)
}

// registerGate decides who may register a passkey: a logged-in
// operator, or anyone with a valid setup token while no passkey exists.
// It returns the setup token to use up ("" when logged in).
func (s *Service) registerGate(w http.ResponseWriter, r *http.Request, setupToken string, passkeys int) (string, bool) {
	in, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return "", false
	}
	if in {
		return "", true
	}
	if passkeys > 0 {
		writeErr(w, r, http.StatusForbidden, "forbidden", "a passkey is registered: log in to add another")
		return "", false
	}
	valid, err := s.setupTokenValid(r.Context(), setupToken)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "setup token lookup")
		return "", false
	}
	if !valid {
		writeErr(w, r, http.StatusForbidden, "forbidden", "setup token invalid, used or expired: run conclave-mcp ui setup again")
		return "", false
	}
	return setupToken, true
}

func (s *Service) registerBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupToken string `json:"setup_token"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "passkey"
	}
	if len([]rune(name)) > 64 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "name must be at most 64 characters")
		return
	}
	op, err := s.operator(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return
	}
	token, ok := s.registerGate(w, r, req.SetupToken, len(op.creds))
	if !ok {
		return
	}
	creation, data, err := s.wa.BeginRegistration(op,
		webauthn.WithExclusions(webauthn.Credentials(op.creds).CredentialDescriptors()))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin registration")
		return
	}
	if !s.startCeremony(w, r, ceremony{kind: kindRegister, data: *data, setupToken: token, name: name}) {
		return
	}
	writeJSON(w, http.StatusOK, creation.Response)
}

func (s *Service) registerFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, kindRegister)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no passkey registration in progress, or it took longer than 5 minutes")
		return
	}
	if c.setupToken == "" && !s.requireSession(w, r) { // the session may have ended meanwhile
		return
	}
	op, err := s.operator(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return
	}
	cred, err := s.wa.FinishRegistration(op, c.data, r)
	if err != nil {
		s.auditFail(ctx, "ui.passkey.add", "error:rejected", err.Error())
		writeErr(w, r, http.StatusBadRequest, "bad_request", "passkey not accepted")
		return
	}
	var keyID int64
	if c.setupToken != "" {
		var used bool
		used, err = s.useSetupToken(ctx, c.setupToken)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "setup token lookup")
			return
		}
		if !used {
			writeErr(w, r, http.StatusForbidden, "forbidden", "setup token invalid, used or expired: run conclave-mcp ui setup again")
			return
		}
		keyID, err = ensureOperatorKey(ctx, s.db, s.now())
		if errors.Is(err, errOperatorUnusable) {
			writeErr(w, r, http.StatusConflict, "conflict", err.Error())
			return
		}
	} else {
		keyID, err = operatorKeyID(ctx, s.db)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "operator key")
		return
	}
	added, err := s.addCredential(ctx, cred, c.name, c.setupToken != "")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "store passkey")
		return
	}
	if !added {
		writeErr(w, r, http.StatusConflict, "conflict", "another passkey was registered meanwhile: log in with it")
		return
	}
	out := map[string]string{"id": credID(cred.ID), "name": c.name}
	via := "session"
	if c.setupToken != "" { // the first passkey logs the operator in
		if out["session"], err = s.createSession(ctx); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "start session")
			return
		}
		via = "setup_token"
	}
	detail, _ := json.Marshal(map[string]string{"name": c.name, "via": via})
	_ = audit.Write(ctx, s.db, keyID, "ui.passkey.add", "passkey", credID(cred.ID), "", "ok", string(detail))
	writeJSON(w, http.StatusCreated, out)
}

// verifyAssertion checks the assertion in r against the ceremony c: one
// of the operator's passkeys, user verified, its signature counter not
// gone backwards. It stores the passkey's new counter and flags and
// returns its id. On refusal it audits action and writes the response.
func (s *Service) verifyAssertion(w http.ResponseWriter, r *http.Request, c ceremony, action string) (string, bool) {
	ctx := r.Context()
	op, err := s.operator(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "load passkeys")
		return "", false
	}
	_, cred, err := s.wa.FinishPasskeyLogin(func(_, handle []byte) (webauthn.User, error) {
		if !bytes.Equal(handle, op.handle) {
			return nil, errors.New("unknown user handle")
		}
		return op, nil
	}, c.data, r)
	if err != nil {
		s.auditFail(ctx, action, "error:rejected", err.Error())
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "passkey not accepted")
		return "", false
	}
	if cred.Authenticator.CloneWarning {
		s.auditFail(ctx, action, "error:clone_warning", "sign counter went backwards")
		writeErr(w, r, http.StatusForbidden, "forbidden", "the passkey's signature counter went backwards: possibly a cloned authenticator")
		return "", false
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ui_credentials SET sign_count = ?, flags = ?, last_used_at = ? WHERE id = ?`,
		cred.Authenticator.SignCount, int(cred.Flags.ProtocolValue()), storage.FormatTS(s.now()), credID(cred.ID)); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "update passkey")
		return "", false
	}
	return credID(cred.ID), true
}

func (s *Service) loginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin login")
		return
	}
	if !s.startCeremony(w, r, ceremony{kind: kindLogin, data: *data}) {
		return
	}
	writeJSON(w, http.StatusOK, assertion.Response)
}

func (s *Service) loginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.takeCeremony(w, r, kindLogin)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no login in progress, or it took longer than 5 minutes")
		return
	}
	id, ok := s.verifyAssertion(w, r, c, "ui.login")
	if !ok {
		return
	}
	keyID, err := operatorKeyID(ctx, s.db)
	if err != nil {
		writeErr(w, r, http.StatusForbidden, "forbidden", "the operator key is revoked or missing: run admin ui-reset with the service stopped, then ui setup")
		return
	}
	token, err := s.createSession(ctx)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "start session")
		return
	}
	_ = audit.Write(ctx, s.db, keyID, "ui.login", "passkey", id, "", "ok", "")
	writeJSON(w, http.StatusOK, map[string]string{"session": token})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := sessionToken(r)
	if live, _ := s.session(ctx, token); live {
		if err := s.deleteSession(ctx, token); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "end session")
			return
		}
		keyID, _ := operatorKeyID(ctx, s.db)
		_ = audit.Write(ctx, s.db, keyID, "ui.logout", "session", "", "", "ok", "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// state tells the start page what to show: {setup_needed, logged_in}.
func (s *Service) state(w http.ResponseWriter, r *http.Request) {
	n, err := s.credentialCount(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "count passkeys")
		return
	}
	in, err := s.loggedIn(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setup_needed": n == 0, "logged_in": in})
}

func (s *Service) listCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, created_at, last_used_at FROM ui_credentials ORDER BY created_at`)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list passkeys")
		return
	}
	defer rows.Close()
	type view struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		LastUsedAt string `json:"last_used_at,omitempty"`
	}
	out := []view{}
	for rows.Next() {
		var (
			v    view
			last sql.NullString
		)
		if err := rows.Scan(&v.ID, &v.Name, &v.CreatedAt, &last); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		v.LastUsedAt = last.String
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list passkeys")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// removeCredential deletes a passkey, never the last one.
func (s *Service) removeCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ui_credentials WHERE id = ? AND (SELECT COUNT(*) FROM ui_credentials) > 1`, id)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "remove passkey")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM ui_credentials WHERE id = ?`, id).Scan(&one) == nil {
			writeErr(w, r, http.StatusConflict, "conflict", "the last passkey cannot be removed")
			return
		}
		writeErr(w, r, http.StatusNotFound, "not_found", "passkey not found")
		return
	}
	// A session started with the removed passkey must not survive.
	if err := s.deleteOtherSessions(ctx, sessionToken(r)); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "end other sessions")
		return
	}
	keyID, _ := operatorKeyID(ctx, s.db)
	_ = audit.Write(ctx, s.db, keyID, "ui.passkey.remove", "passkey", id, "", "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr writes the /v1 error envelope.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": w.Header().Get("X-Request-Id"),
	}})
}
