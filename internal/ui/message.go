package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// messageBegin starts the passkey confirmation of one operator message.
// It keeps the send body built here; finish posts that body and nothing
// the finish request carries.
func (s *Service) messageBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	var req struct {
		RoomID      string `json:"room_id"`
		Content     string `json:"content"`
		MessageType string `json:"message_type"`
		ReplyTo     *int64 `json:"reply_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "message too large: the web UI sends at most 64 KiB")
			return
		}
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if req.RoomID == "" {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "room_id is required")
		return
	}
	msg, err := json.Marshal(struct {
		Content     string `json:"content"`
		ContentType string `json:"content_type"`
		MessageType string `json:"message_type,omitempty"`
		ReplyTo     *int64 `json:"reply_to,omitempty"`
	}{req.Content, "text/plain", req.MessageType, req.ReplyTo})
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "encode message")
		return
	}
	assertion, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin confirmation")
		return
	}
	if !s.startCeremony(w, r, ceremony{kind: kindMessage, data: *data, roomID: req.RoomID, message: msg,
		session: hashToken(sessionToken(r))}) {
		return
	}
	writeJSON(w, http.StatusOK, assertion.Response)
}

// messageFinish posts the message stored at begin as the operator, once
// the passkey confirmed it in the session that began it.
func (s *Service) messageFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Taken first: a refused finish uses the ceremony up too.
	c, ok := s.takeCeremony(w, r, kindMessage)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "no message confirmation in progress, or it took longer than 5 minutes")
		return
	}
	token := sessionToken(r)
	live, err := s.session(ctx, token)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "session lookup")
		return
	}
	if !live {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "session expired: log in again")
		return
	}
	if hashToken(token) != c.session {
		s.auditFail(ctx, "message.send", "error:other_session", "finish from another session than begin")
		writeErr(w, r, http.StatusForbidden, "forbidden", "this confirmation was started in another session")
		return
	}
	if _, ok := s.verifyAssertion(w, r, c, "message.send"); !ok {
		return
	}
	keyID, err := operatorKeyID(ctx, s.db)
	if err != nil {
		writeErr(w, r, http.StatusForbidden, "forbidden", "the operator key is revoked or missing: run admin ui-reset with the service stopped, then ui setup")
		return
	}
	if s.SendAsOperator == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "unavailable", "sending is not available")
		return
	}
	// Admin scope only: the send runs as the operator key, not as a session.
	req := r.Clone(context.WithValue(ctx, auth.ClaimsKey{}, auth.Claims{KeyID: keyID, Scopes: []string{auth.ScopeAdmin}}))
	req.Body = io.NopCloser(bytes.NewReader(c.message))
	req.ContentLength = int64(len(c.message))
	s.SendAsOperator(w, req, c.roomID)
}
