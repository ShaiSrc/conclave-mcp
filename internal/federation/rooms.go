package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// Host side of federated rooms (docs/superpowers/specs/2026-11-13-federated-rooms-design.md):
// inviting a peer's bot, and the signed endpoints through which member
// servers post, claim, leave and read a room's event log. The member
// server's bots act as their stand-in keys, through the same send and
// claims paths as local keys.

const maxRoomEvents = 100 // events per answer of /fed/v1/rooms/{id}/events

// RoomHooks connect the federation listener to the local services.
// The server sets them with federation on, before the listener serves.
type RoomHooks struct {
	// Send is the messages service's send path for the stand-in key in
	// r's claims; it stores originSig and nonce with the message.
	Send func(w http.ResponseWriter, r *http.Request, roomID, originSig, nonce string)
	// Claims serves the claims routes (/rooms/{id}/claims...) without
	// authentication: r's claims say who calls.
	Claims http.Handler
	// MaxMessageBytes is the message cap. A message proposal's body may
	// be this much over the 64 KiB of other federation requests.
	MaxMessageBytes int
	// Replicate is the messages service's store for a message of a
	// replica room, published to subscribers like a sent one.
	Replicate func(ctx context.Context, roomID string, m fedrooms.ReplicaMessage) (int64, error)
	// Follow takes the follow request of follower, a bot of the signing
	// server, toward our bot handle and returns the follow's status as the
	// follower sees it; a refusal is a *fedrooms.Error (internal/follow).
	Follow func(ctx context.Context, follower, handle string) (string, error)
	// Withdraw deletes the pending or rejected follow request of follower,
	// a bot of the signing server, toward our bot handle, and reports
	// whether there was one (internal/follow).
	Withdraw func(ctx context.Context, follower, handle string) (bool, error)
}

// Domain returns this server's federation domain.
func (s *Service) Domain() string { return s.id.Domain }

// MessageSig is what the author's server signs for a message in a
// federated room. ContentType and MessageType are as stored: text/plain
// and system when the sender gave none.
type MessageSig struct {
	Room        string // the room address, room-id@host
	Author      string // the author address, name@domain
	ContentType string
	MessageType string
	Content     string
	Metadata    []byte // the wire form, fedrooms.SplitMetadata
	ReplyTo     *int64 // the host's message id
	// Nonce is 16 random bytes, unpadded base64url, new for each
	// message: two messages with the same fields have two signatures,
	// so a signature names one message, and a replay of it is found.
	Nonce string
}

// SigningString returns the lines conclave-msg-v1, the room address,
// the author address, the content type, the message type, the lowercase
// hex SHA-256 of the content and of the metadata, the host's reply_to
// (an empty line without one) and the nonce, joined by \n.
func (m MessageSig) SigningString() []byte {
	content, meta := sha256.Sum256([]byte(m.Content)), sha256.Sum256(m.Metadata)
	reply := ""
	if m.ReplyTo != nil {
		reply = strconv.FormatInt(*m.ReplyTo, 10)
	}
	return []byte(strings.Join([]string{"conclave-msg-v1", m.Room, m.Author, m.ContentType, m.MessageType,
		hex.EncodeToString(content[:]), hex.EncodeToString(meta[:]), reply, m.Nonce}, "\n"))
}

// SignMessage returns the identity's signature of m, unpadded base64url.
// m.Nonce must be a fresh newNonce().
func (id *Identity) SignMessage(m MessageSig) string {
	return b64.EncodeToString(id.Sign(m.SigningString()))
}

// VerifyMessage reports whether sig is pub's signature of m, whose
// nonce must be 16 bytes of unpadded base64url. Both must be in their
// one spelling: a signature is stored once per room as it is spelled.
func VerifyMessage(pub ed25519.PublicKey, m MessageSig, sig string) bool {
	n, nerr := decodeB64(m.Nonce)
	b, err := decodeB64(sig)
	return nerr == nil && len(n) == 16 && err == nil && len(b) == ed25519.SignatureSize && len(pub) == ed25519.PublicKeySize &&
		ed25519.Verify(pub, m.SigningString(), b)
}

// authorSigned reports whether sig on m verifies with the stored key of
// the author's server. An unknown server's is false.
func (s *Service) authorSigned(ctx context.Context, m MessageSig, sig string) (bool, error) {
	_, domain, _ := strings.Cut(m.Author, "@")
	p, err := s.peers.Get(ctx, domain)
	if errors.Is(err, ErrPeerNotFound) {
		return false, nil
	}
	return err == nil && VerifyMessage(p.PublicKey, m, sig), err
}

// InviteRemote asks the server of address (name@domain, lowercase) to
// add its bot to room roomID, named name, with rights set, and returns
// what that server reads a push up to (its answer's max_events_bytes,
// 0 when it does not say), which the room's delivery cursor keeps. The
// rooms service calls it before it adds the bot's stand-in. Only
// allowed peers are asked. The invite names the room's kind: direct,
// feed for a feed, else room; an answer that does not name it back is
// a refusal (403). A feed's invite carries its bot's address too, which
// its name may hold cut. A refusal is a *fedrooms.Error.
func (s *Service) InviteRemote(ctx context.Context, roomID, name, address string, set []string) (int, error) {
	handle, domain, _ := strings.Cut(address, "@")
	h, herr := bots.NormalizeHandle(handle)
	d, derr := NormalizeDomain(domain)
	switch {
	case herr != nil || derr != nil || h+"@"+d != address:
		return 0, &fedrooms.Error{Status: http.StatusBadRequest, Code: "invalid_address", Message: "address must be name@domain, name a bot's handle"}
	case d == s.id.Domain:
		return 0, &fedrooms.Error{Status: http.StatusBadRequest, Code: "own_domain", Message: "a bot of this server is invited by its key_id"}
	}
	peer, err := s.peers.Get(ctx, d)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		return 0, err
	}
	if peer.Status != statusAllowed {
		return 0, &fedrooms.Error{Status: http.StatusForbidden, Code: "peer_not_allowed", Message: d + " is not an allowed peer"}
	}
	var kind, feed string
	if err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN feed IS NOT NULL THEN 'feed' ELSE kind END, COALESCE(feed, '') FROM rooms WHERE id = ?`,
		roomID).Scan(&kind, &feed); err != nil {
		return 0, err
	}
	body, _ := json.Marshal(struct {
		Name   string   `json:"name"`
		Handle string   `json:"handle"`
		Rights []string `json:"rights"`
		Kind   string   `json:"kind"`
		Feed   string   `json:"feed,omitempty"`
	}{name, h, set, kind, feed})
	status, out, err := s.sendSigned(ctx, http.MethodPost, d, "/fed/v1/rooms/"+roomID+"/invite", body, maxBody)
	var refusal *fedrooms.Error
	switch {
	case err != nil:
		s.logger.Warn("federation room invite failed", "domain", d, "err", err)
		refusal = &fedrooms.Error{Status: http.StatusBadGateway, Code: "peer_unreachable", Message: "peer did not answer"}
	case (status == http.StatusOK || status == http.StatusCreated) && kind == kindDirect && answerKind(out) != kindDirect:
		// A peer before direct conversations takes it as a room.
		refusal = &fedrooms.Error{Status: http.StatusForbidden, Code: "dm_refused", Message: "the peer does not support direct conversations"}
	case (status == http.StatusOK || status == http.StatusCreated) && kind == kindFeed && answerKind(out) != kindFeed:
		refusal = &fedrooms.Error{Status: http.StatusForbidden, Code: "invite_refused", Message: "the peer does not support feeds"}
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusNotFound:
		refusal = &fedrooms.Error{Status: http.StatusNotFound, Code: "not_found", Message: "no bot " + h + " on " + d}
	case kind == kindDirect && (status == http.StatusForbidden || status == http.StatusTooManyRequests):
		// The peer's status and code reach the sender; its text does not.
		refusal = &fedrooms.Error{Status: status, Code: errCode(out, "dm_refused"), Message: d + " refused the conversation"}
	case status == http.StatusForbidden:
		refusal = &fedrooms.Error{Status: http.StatusForbidden, Code: "invite_refused", Message: d + " refused the invite"}
	case status == http.StatusConflict:
		refusal = &fedrooms.Error{Status: http.StatusConflict, Code: errCode(out, "conflict"), Message: d + " refused the invite: a conflict there"}
	default:
		refusal = &fedrooms.Error{Status: http.StatusBadGateway, Code: "peer_unreachable", Message: fmt.Sprintf("%s answered with status %d", d, status)}
	}
	detail, _ := json.Marshal(map[string]any{"address": address, "rights": set})
	if refusal != nil {
		s.auditRoom(ctx, "federation.room.invite", d, roomID, "error:"+refusal.Code, string(detail))
		return 0, refusal
	}
	s.auditRoom(ctx, "federation.room.invite", d, roomID, "ok", string(detail))
	// Its answer is contact: delivery paused while it was away or
	// blocked resumes.
	if err := s.resumeDelivery(ctx, d); err != nil {
		s.logger.Warn("federation delivery resume failed", "domain", d, "err", err)
	}
	var answer struct {
		Max int `json:"max_events_bytes"`
	}
	_ = json.Unmarshal(out, &answer)
	return max(answer.Max, 0), nil
}

// answerKind is the kind an invite's answer names, "" for none.
func answerKind(out []byte) string {
	var a struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(out, &a)
	return a.Kind
}

// sendSigned sends a signed room request to path on domain, with body
// unless it is nil, within roomTimeout, and returns the status and the
// answer, read up to limit bytes.
func (s *Service) sendSigned(ctx context.Context, method, domain, path string, body []byte, limit int) (int, []byte, error) {
	return s.signed(ctx, s.fetch.roomClient, method, domain, path, body, limit)
}

// signed sends a signed request with client.
func (s *Service) signed(ctx context.Context, client *http.Client, method, domain, path string, body []byte, limit int) (int, []byte, error) {
	req, err := s.fetch.newRequest(ctx, method, domain, path, body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := s.id.SignRequest(req, domain, body); err != nil {
		return 0, nil, err
	}
	return s.fetch.send(client, req, s.fetch.isAllowed(domain), limit)
}

// roomPeer checks a room request of the verified origin and returns the
// room's id: the origin must be an allowed peer (open mode lets seen
// ones reach /fed/v1, not rooms) with a bot in a room hosted here. It
// answers the request itself otherwise.
func (s *Service) roomPeer(w http.ResponseWriter, r *http.Request) (string, bool) {
	ctx, origin, roomID := r.Context(), OriginFrom(r.Context()), chi.URLParam(r, "id")
	if !s.allowedOrigin(w, r) {
		return "", false
	}
	var member bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rooms r
		JOIN room_members m ON m.room_id = r.id JOIN api_keys k ON k.id = m.key_id
		WHERE r.id = ? AND r.home_domain IS NULL AND k.remote_domain = ?)`, roomID, origin).Scan(&member); err != nil {
		s.fail(w, r, err)
		return "", false
	}
	if !member {
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
		return "", false
	}
	return roomID, true
}

// standIn returns the stand-in key of author, which must be a bot of the
// origin that this server knows. It answers the request itself
// otherwise.
func (s *Service) standIn(w http.ResponseWriter, r *http.Request, author string) (int64, bool) {
	name, domain, ok := strings.Cut(author, "@")
	switch {
	case !ok || name == "":
		writeErr(w, r, http.StatusBadRequest, "bad_request", "author must be name@domain")
		return 0, false
	case domain != OriginFrom(r.Context()):
		writeErr(w, r, http.StatusForbidden, "wrong_origin", "author must be a bot of the signing server")
		return 0, false
	}
	var id int64
	err := s.db.QueryRowContext(r.Context(), `SELECT id FROM api_keys WHERE label = ? AND remote_domain = ?`, author, domain).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
		} else {
			s.fail(w, r, err)
		}
		return 0, false
	}
	return id, true
}

// asStandIn returns a copy of r for the local services: authenticated
// as stand-in keyID (no scopes, so no admin key; its trust origin,
// external, is its key's), with path and body, outside the federation
// router.
func asStandIn(r *http.Request, keyID int64, path string, body []byte) *http.Request {
	ctx := context.WithValue(r.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: keyID})
	ctx = context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))
	req := r.Clone(ctx)
	req.URL.Path, req.URL.RawPath, req.URL.RawQuery, req.RequestURI = path, "", "", ""
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.Header.Del("Authorization")
	return req
}

// handleRoomMessage answers POST /fed/v1/rooms/{id}/messages: a message
// of the origin's bot, with its server's author signature. It runs the
// local send path as the bot's stand-in, so membership, rights, types,
// reply_to, the size cap and the archived check apply, and answers the
// message's event.
func (s *Service) handleRoomMessage(w http.ResponseWriter, r *http.Request) {
	roomID, ok := s.roomPeer(w, r)
	if !ok {
		return
	}
	var p struct {
		Author      string          `json:"author"`
		Content     string          `json:"content"`
		ContentType string          `json:"content_type"`
		MessageType string          `json:"message_type"`
		Metadata    json.RawMessage `json:"metadata"`
		ReplyTo     *int64          `json:"reply_to"`
		Sig         string          `json:"sig"`
		Nonce       string          `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	keyID, ok := s.standIn(w, r, p.Author)
	if !ok {
		return
	}
	wire, _, err := fedrooms.SplitMetadata(p.Metadata)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if p.ContentType == "" {
		p.ContentType = "text/plain"
	}
	if p.MessageType == "" {
		p.MessageType = rights.System
	}
	ctx := r.Context()
	signed, err := s.authorSigned(ctx, MessageSig{Room: roomID + "@" + s.id.Domain, Author: p.Author, ContentType: p.ContentType,
		MessageType: p.MessageType, Content: p.Content, Metadata: wire, ReplyTo: p.ReplyTo, Nonce: p.Nonce}, p.Sig)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !signed {
		writeErr(w, r, http.StatusForbidden, "bad_author_signature", "the author signature does not verify")
		return
	}
	// A signature is stored once per room: a retry gets the stored
	// message. ponytail: two copies racing past this check make the
	// second insert fail (500, a 502 for the member), and its retry
	// lands here.
	var id int64
	switch err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE room_id = ? AND origin_sig = ?`, roomID, p.Sig).Scan(&id); {
	case err == nil:
		s.answerMessage(w, r, roomID, id)
		return
	case !errors.Is(err, sql.ErrNoRows):
		s.fail(w, r, err)
		return
	}
	// The send body carries the signed fields only; metadata in its
	// wire form, so the logged message gives back the signed bytes.
	body := rawJSON(struct {
		Content     string          `json:"content"`
		ContentType string          `json:"content_type"`
		MessageType string          `json:"message_type"`
		Metadata    json.RawMessage `json:"metadata,omitempty"`
		ReplyTo     *int64          `json:"reply_to,omitempty"`
	}{p.Content, p.ContentType, p.MessageType, wire, p.ReplyTo})
	rec := httptest.NewRecorder()
	s.Rooms.Send(rec, asStandIn(r, keyID, r.URL.Path, body), roomID, p.Sig, p.Nonce)
	if rec.Code != http.StatusCreated {
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var stored struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &stored)
	s.answerMessage(w, r, roomID, stored.ID)
}

// answerMessage answers a message proposal with the event of message
// id, 201, with <, > and & as stored.
func (s *Service) answerMessage(w http.ResponseWriter, r *http.Request, roomID string, id int64) {
	ev := fedrooms.Event{}
	var payload string
	if err := s.db.QueryRowContext(r.Context(), `SELECT seq, type, payload FROM fed_events
		WHERE room_id = ? AND message_id = ? ORDER BY seq DESC LIMIT 1`,
		roomID, id).Scan(&ev.Seq, &ev.Type, &payload); err != nil {
		s.fail(w, r, err)
		return
	}
	ev.Payload = json.RawMessage(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(rawJSON(ev))
}

// handleRoomClaims answers the claims endpoints under
// /fed/v1/rooms/{id}/claims for the origin's bot named by ?author=, as
// its stand-in, with the rights and conflicts of local keys. Each claim
// and conflict also carries its holder's address, by which the member
// server finds its own key for the holder.
func (s *Service) handleRoomClaims(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.roomPeer(w, r); !ok {
		return
	}
	keyID, ok := s.standIn(w, r, r.URL.Query().Get("author"))
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body) // read and capped by Verify
	rec := httptest.NewRecorder()
	s.Rooms.Claims.ServeHTTP(rec, asStandIn(r, keyID, strings.TrimPrefix(r.URL.Path, "/fed/v1"), body))
	ctx := r.Context()
	out, err := eachClaim(rec.Body.Bytes(), func(c map[string]json.RawMessage) error {
		var id int64
		if json.Unmarshal(c["key_id"], &id) != nil {
			return nil
		}
		addr, err := fedrooms.Address(ctx, s.db, id, s.id.Domain)
		c["address"] = rawJSON(addr)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(out)
}

// eachClaim calls f with each claim and conflict of body, a claims
// answer, and returns body with f's changes; other fields are kept. A
// body without claims or conflicts is returned as it is.
func eachClaim(body []byte, f func(c map[string]json.RawMessage) error) ([]byte, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body, nil
	}
	changed := false
	for _, k := range []string{"claims", "conflicts"} {
		var list []map[string]json.RawMessage
		if json.Unmarshal(top[k], &list) != nil || list == nil {
			continue
		}
		for _, c := range list {
			if err := f(c); err != nil {
				return nil, err
			}
		}
		top[k], changed = rawJSON(list), true
	}
	if !changed {
		return body, nil
	}
	return rawJSON(top), nil
}

// handleRoomLeave answers POST /fed/v1/rooms/{id}/leave: the origin's
// bot leaves the room, and its member.remove event is logged.
func (s *Service) handleRoomLeave(w http.ResponseWriter, r *http.Request) {
	roomID, ok := s.roomPeer(w, r)
	if !ok {
		return
	}
	var in struct {
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	keyID, ok := s.standIn(w, r, in.Author)
	if !ok {
		return
	}
	ctx := r.Context()
	switch removed, err := s.leaveHosted(ctx, roomID, keyID); {
	case err != nil:
		s.fail(w, r, err)
		return
	case !removed:
		writeErr(w, r, http.StatusNotFound, "not_found", "not a member")
		return
	}
	s.DeliverSoon()
	detail, _ := json.Marshal(map[string]string{"address": in.Author})
	s.auditRoom(ctx, "federation.room.leave", OriginFrom(ctx), roomID, "ok", string(detail))
	w.WriteHeader(http.StatusNoContent)
}

// handleRoomEvents answers GET /fed/v1/rooms/{id}/events?since=N&max=M:
// the events of the room's log after seq N, in order, at most 100 and
// within the events budget and M, what the origin reads the answer up
// to (ours without it), and with <, > and & as stored; message events
// are skips unless the origin has a reader in the room, and so is an
// event over M and a feed's member event about another follower.
func (s *Service) handleRoomEvents(w http.ResponseWriter, r *http.Request) {
	roomID, ok := s.roomPeer(w, r)
	if !ok {
		return
	}
	var since int64
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "since must be a seq from 0")
			return
		}
		since = n
	}
	var limit int
	if q := r.URL.Query().Get("max"); q != "" {
		n, err := strconv.ParseInt(q, 10, 32)
		if err != nil || n < 0 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "max must be a byte count from 0")
			return
		}
		limit = int(n)
	}
	out, err := s.loadEvents(r.Context(), roomID, OriginFrom(r.Context()), since, math.MaxInt64, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeBotJSON(w, map[string]any{"events": out})
}
