package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// needsConfirm refuses a change made with a session alone.
const needsConfirm = "changes made with a UI session need a passkey confirmation: make them in the web UI"

// confirmedKey marks a request that confirmFinish runs. The type is
// unexported and the mark lives in the context, so no request can carry
// it: only this package sets it.
type confirmedKey struct{}

// confirmed reports whether r is a change confirmed with a passkey.
func confirmed(r *http.Request) bool { return r.Context().Value(confirmedKey{}) != nil }

// changePath reports whether a confirmation may run a request to target:
// a /v1 endpoint or one of the UI's own changes, never a login, a logout
// or another ceremony's step.
func changePath(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || strings.Contains(target, "#") {
		return false
	}
	p := u.Path
	if p != path.Clean(p) {
		return false
	}
	return strings.HasPrefix(p, "/v1/") || p == "/ui/agents" || p == "/ui/auth/register/begin" ||
		path.Dir(p) == "/ui/auth/credentials"
}

// confirmBegin starts the passkey confirmation of one change: it keeps
// the request (method, path, body) for finish, which runs that request
// and nothing the finish request carries.
func (s *Service) confirmBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	var req struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "method must be POST, PUT, PATCH or DELETE")
		return
	}
	if !changePath(req.Path) {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "path is not a change the web UI can confirm")
		return
	}
	body := []byte(req.Body)
	if string(body) == "null" {
		body = nil
	}
	assertion, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin confirmation")
		return
	}
	if !s.startCeremony(w, r, ceremony{kind: kindConfirm, data: *data, method: req.Method, path: req.Path, body: body,
		session: hashToken(sessionToken(r))}) {
		return
	}
	writeJSON(w, http.StatusOK, assertion.Response)
}

// confirmFinish runs the change stored at begin, once the passkey
// confirmed it in the session that began it, and answers its response.
func (s *Service) confirmFinish(w http.ResponseWriter, r *http.Request) {
	c, ok := s.finishCeremony(w, r, kindConfirm, "ui.confirm", "confirmation")
	if !ok {
		return
	}
	s.run(w, r, c.method, c.path, c.body)
}

// finishCeremony takes the request's ceremony of the given kind and
// checks it: begun in this session, which is still live, and confirmed
// by a valid assertion. Refusals are audited as action and answered;
// what names the ceremony in the "not in progress" error.
func (s *Service) finishCeremony(w http.ResponseWriter, r *http.Request, kind ceremonyKind, action, what string) (ceremony, bool) {
	ctx := r.Context()
	// Taken first: a refused finish uses the ceremony up too.
	c, ok := s.takeCeremony(w, r, kind)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no "+what+" in progress, or it took longer than 5 minutes")
		return c, false
	}
	token := sessionToken(r)
	live, err := s.session(ctx, token)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return c, false
	}
	if !live {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "session expired: log in again")
		return c, false
	}
	if hashToken(token) != c.session {
		s.auditFail(ctx, action, "error:other_session", "finish from another session than begin")
		writeErr(w, r, http.StatusForbidden, "forbidden", "this confirmation was started in another session")
		return c, false
	}
	if _, ok := s.verifyAssertion(w, r, c, action); !ok {
		return c, false
	}
	return c, true
}

// run serves a change through the server's router as confirmed in r's
// session: r's context without chi's routing state (the router routes
// it afresh) plus the mark, r's Host and session token, the UI Origin.
func (s *Service) run(w http.ResponseWriter, r *http.Request, method, target string, body []byte) {
	if s.Router == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "unavailable", "confirmations are not available")
		return
	}
	ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, nil), confirmedKey{}, true)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "build request")
		return
	}
	req.Host, req.RemoteAddr = r.Host, r.RemoteAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", s.origin)
	req.Header.Set(HeaderName, sessionToken(r))
	s.Router.ServeHTTP(w, req)
}

// record runs a confirmed sub-request (see run) and returns its response.
// ponytail: httptest's recorder is the stdlib's in-memory ResponseWriter.
func (s *Service) record(r *http.Request, method, target string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.run(rec, r, method, target, body)
	return rec
}

// copyResponse writes a recorded response to w.
func copyResponse(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// createAgent answers POST /ui/agents, confirmed only: a key and its
// invitation to a room as one change, like conclave-mcp agent add. Both
// /v1 calls run as confirmed sub-requests, so their rules and audit
// apply; a failed invite revokes the key, so none is left that can reach
// nothing. It answers what POST /v1/keys answers.
func (s *Service) createAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	if !confirmed(r) {
		writeErr(w, r, http.StatusForbidden, "forbidden", needsConfirm)
		return
	}
	var req struct {
		Label       string   `json:"label"`
		TrustOrigin string   `json:"trust_origin"`
		RoomID      string   `json:"room_id"`
		Rights      []string `json:"rights"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if req.RoomID == "" {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "room_id is required")
		return
	}
	if len(req.Rights) == 0 { // a key with no rights can neither read nor send in its room
		writeErr(w, r, http.StatusBadRequest, "bad_request", "pick at least one right for the agent")
		return
	}
	keyBody, _ := json.Marshal(map[string]string{"label": req.Label, "trust_origin": req.TrustOrigin})
	key := s.record(r, http.MethodPost, "/v1/keys", keyBody)
	var created struct {
		ID int64 `json:"id"`
	}
	if key.Code != http.StatusCreated || json.Unmarshal(key.Body.Bytes(), &created) != nil {
		copyResponse(w, key)
		return
	}
	inviteBody, _ := json.Marshal(map[string]any{"key_id": created.ID, "rights": req.Rights})
	invite := s.record(r, http.MethodPost, "/v1/rooms/"+url.PathEscape(req.RoomID)+"/invite", inviteBody)
	if invite.Code/100 == 2 {
		copyResponse(w, key)
		return
	}
	var failed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(invite.Body.Bytes(), &failed)
	// The revoke runs even if the client has gone meanwhile.
	if rev := s.record(r.WithContext(context.WithoutCancel(r.Context())), http.MethodDelete, fmt.Sprintf("/v1/keys/%d", created.ID), nil); rev.Code/100 != 2 {
		writeErr(w, r, http.StatusInternalServerError, "internal_error",
			fmt.Sprintf("invite failed (%s); revoking key %d failed too: revoke it on the Keys page", failed.Error.Message, created.ID))
		return
	}
	writeErr(w, r, invite.Code, failed.Error.Code,
		failed.Error.Message+" The key was revoked; its label stays taken, so a retry needs another label.")
}
