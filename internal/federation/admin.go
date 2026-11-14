package federation

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Operator lookups (and adds, which look up) have their own budget,
// apart from the fetches for strangers.
const (
	lookupsPerMinute = 30
	lookupBurst      = 10
)

// AdminRoutes mounts the operator endpoints under /v1/federation, and
// GET /v1/rooms/{id}/delivery. The
// caller mounts them behind authentication; every handler also needs
// an admin key.
func (s *Service) AdminRoutes(r chi.Router) {
	r.Route("/federation", func(r chi.Router) {
		r.Use(adminOnly)
		r.Get("/", s.handleStatus)
		r.Post("/keys/rotate", s.handleRotateKey)
		r.Delete("/keys/previous", s.handleForgetOwnKeys)
		r.Get("/peers", s.handleListPeers)
		r.Get("/lookup", s.handleLookup)
		r.Post("/peers", s.handleAddPeer)
		r.Get("/peers/{domain}", s.handleGetPeer)
		r.Patch("/peers/{domain}", s.handlePatchPeer)
		r.Post("/peers/{domain}/block", s.handleBlockPeer)
		r.Delete("/peers/{domain}", s.handleRemovePeer)
		r.Post("/peers/{domain}/ping", s.handlePingPeer)
		r.Post("/peers/{domain}/retry", s.handleRetryPeer)
		r.Post("/peers/{domain}/purge", s.handlePurgePeer)
		r.Delete("/peers/{domain}/keys/history", s.handleForgetKeys)
		r.Get("/blocked-bots", s.handleListBlocked)
		r.Post("/blocked-bots", s.handleBlockBot)
		r.Delete("/blocked-bots/{address}", s.handleUnblockBot)
	})
	r.With(adminOnly).Get("/rooms/{id}/delivery", s.handleRoomDelivery)
}

// handleRoomDelivery lists, per member server, how far the events of a
// room hosted here have been delivered. A replica room, a missing room
// and a room that has never had a member server answer 404.
func (s *Service) handleRoomDelivery(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rows, err := s.db.QueryContext(r.Context(), `SELECT d.peer, d.acked_seq,
		COALESCE((SELECT MAX(e.seq) FROM fed_events e WHERE e.room_id = d.room_id), 0),
		d.attempts, d.next_try_at, d.last_error, d.paused
		FROM fed_delivery d JOIN rooms r ON r.id = d.room_id
		WHERE d.room_id = ? AND r.home_domain IS NULL ORDER BY d.peer`, id)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		Peer      string  `json:"peer"`
		AckedSeq  int64   `json:"acked_seq"`
		LastSeq   int64   `json:"last_seq"`
		Attempts  int     `json:"attempts"`
		NextTryAt *string `json:"next_try_at"`
		LastError string  `json:"last_error"`
		Paused    bool    `json:"paused"`
	}
	out := []row{}
	for rows.Next() {
		var d row
		if err := rows.Scan(&d.Peer, &d.AckedSeq, &d.LastSeq, &d.Attempts, &d.NextTryAt, &d.LastError, &d.Paused); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if len(out) == 0 {
		writeErr(w, r, http.StatusNotFound, "not_found", "no federated delivery for this room")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"delivery": out})
}

// DisabledRoutes answers 404 federation_disabled on /v1/federation and below.
func DisabledRoutes(r chi.Router) {
	h := func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusNotFound, "federation_disabled", "federation is not enabled on this server")
	}
	r.HandleFunc("/federation", h)
	r.HandleFunc("/federation/*", h)
}

func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.IsAdmin(r.Context()) {
			writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type peerView struct {
	Domain      string   `json:"domain"`
	Status      string   `json:"status"`
	Fingerprint string   `json:"fingerprint"`
	KeyID       string   `json:"key_id"`
	Note        string   `json:"note"`
	AddedAt     string   `json:"added_at"`
	LastSeenAt  *string  `json:"last_seen_at"`
	LastOutAt   *string  `json:"last_out_at"`
	Health      string   `json:"health"` // ok, failing or unreachable
	Delivery    delivery `json:"delivery"`
	Limits      limits   `json:"limits"`
}

// delivery sums a peer's delivery cursors: the rooms, the events they
// have yet to deliver, how many are paused, and the earliest
// failing_since with that cursor's last error.
type delivery struct {
	Rooms        int     `json:"rooms"`
	Pending      int64   `json:"pending"`
	FailingSince *string `json:"failing_since"`
	Paused       int     `json:"paused"`
	LastError    string  `json:"last_error"`
}

// health is unreachable when a cursor is paused, failing when one
// fails, ok otherwise.
func (d delivery) health() string {
	switch {
	case d.Paused > 0:
		return "unreachable"
	case d.FailingSince != nil:
		return "failing"
	}
	return "ok"
}

// rfc3339 formats a nullable time for a view.
func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func viewOf(p Peer, d delivery, o overrides) peerView {
	v := peerView{Domain: p.Domain, Status: p.Status, Fingerprint: p.Fingerprint, Note: p.Note,
		AddedAt: p.AddedAt.UTC().Format(time.RFC3339), LastSeenAt: rfc3339(p.LastSeenAt), LastOutAt: rfc3339(p.LastOutAt),
		Health: d.health(), Delivery: d, Limits: o.effective(p.Status)}
	if len(p.PublicKey) == ed25519.PublicKeySize {
		v.KeyID = KeyIDFor(p.PublicKey)
	}
	return v
}

// views returns the views of ps; domain is their one domain, or "" for
// all peers.
func (s *Service) views(ctx context.Context, domain string, ps ...Peer) ([]peerView, error) {
	ds, err := s.deliveryOf(ctx, domain)
	if err != nil {
		return nil, err
	}
	os, err := s.overridesOf(ctx, domain)
	if err != nil {
		return nil, err
	}
	out := make([]peerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, viewOf(p, ds[p.Domain], os[p.Domain]))
	}
	return out, nil
}

// deliveryOf sums the delivery cursors of domain, or of every peer for
// "", by peer.
func (s *Service) deliveryOf(ctx context.Context, domain string) (map[string]delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer, failing_since, paused, last_error,
		MAX(COALESCE((SELECT MAX(e.seq) FROM fed_events e WHERE e.room_id = d.room_id), 0) - acked_seq, 0)
		FROM fed_delivery d WHERE ? IN ('', peer)`, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]delivery{}
	for rows.Next() {
		var peer, lastErr string
		var failing sql.NullString
		var paused bool
		var pending int64
		if err := rows.Scan(&peer, &failing, &paused, &lastErr, &pending); err != nil {
			return nil, err
		}
		d := out[peer]
		d.Rooms++
		d.Pending += pending
		if paused {
			d.Paused++
		}
		if t := rfc3339(parseTS(failing)); t != nil && (d.FailingSince == nil || *t < *d.FailingSince) {
			d.FailingSince, d.LastError = t, lastErr
		}
		out[peer] = d
	}
	return out, rows.Err()
}

// writePeer answers domain's view with status, or 404 peer_not_found.
func (s *Service) writePeer(w http.ResponseWriter, r *http.Request, domain string, status int) {
	p, err := s.peers.Get(r.Context(), domain)
	if errors.Is(err, ErrPeerNotFound) {
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
		return
	}
	var vs []peerView
	if err == nil {
		vs, err = s.views(r.Context(), domain, p)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, status, vs[0])
}

// audit writes an entry about the peer domain. The actor is the
// caller's key, or 0 on the federation listener.
func (s *Service) audit(ctx context.Context, action, domain, result, detail string) {
	s.auditRoom(ctx, action, domain, "", result, detail)
}

// auditRoom is audit for an entry about the peer domain in room roomID.
func (s *Service) auditRoom(ctx context.Context, action, domain, roomID, result, detail string) {
	s.auditTarget(ctx, action, "federation_peer", domain, roomID, result, detail)
}

// auditTarget writes an entry about the target of targetType.
func (s *Service) auditTarget(ctx context.Context, action, targetType, targetID, roomID, result, detail string) {
	var actor int64
	if c, ok := auth.FromContext(ctx); ok {
		actor = c.KeyID
	}
	if err := audit.Write(ctx, s.db, actor, action, targetType, targetID, roomID, result, detail); err != nil {
		s.logger.Error("federation audit write failed", "action", action, "err", err)
	}
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	peers, err := s.peers.List(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	n := map[string]int{statusAllowed: 0, statusBlocked: 0, statusSeen: 0}
	for _, p := range peers {
		n[p.Status]++
	}
	id := s.id()
	writeJSON(w, http.StatusOK, map[string]any{"domain": id.Domain, "mode": s.mode,
		"fingerprint": id.Fingerprint(), "key_id": id.KeyID, "peers": n, "keys": id.Document(s.now()).Keys})
}

// handleRotateKey makes a new key current (Rotate) and answers it.
func (s *Service) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	prev := s.id()
	id, err := Rotate(r.Context(), s.db, prev, s.now())
	switch {
	case errors.Is(err, ErrRotationTooSoon):
		writeErr(w, r, http.StatusConflict, "rotation_too_soon", err.Error())
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.ident.Store(id)
	detail, _ := json.Marshal(struct {
		Previous string `json:"previous"`
		Current  string `json:"current"`
	}{prev.Fingerprint(), id.Fingerprint()})
	s.auditTarget(r.Context(), "federation.key.rotate", "federation_key", id.KeyID, "", "ok", string(detail))
	writeJSON(w, http.StatusOK, map[string]any{"key_id": id.KeyID, "fingerprint": id.Fingerprint(), "keys": id.Document(s.now()).Keys})
}

// handleForgetOwnKeys deletes this server's keys other than the current
// one, private keys included (ForgetOldKeys): after a key leak, the
// operator rotates and then forgets. It answers the keys now and how
// many were dropped; audit federation.key.forget.
func (s *Service) handleForgetOwnKeys(w http.ResponseWriter, r *http.Request) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	id, dropped, err := ForgetOldKeys(r.Context(), s.db, s.id())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.ident.Store(id)
	detail, _ := json.Marshal(struct {
		Dropped int      `json:"dropped"`
		KeyIDs  []string `json:"key_ids"`
	}{len(dropped), append([]string{}, dropped...)})
	s.auditTarget(r.Context(), "federation.key.forget", "federation_key", id.KeyID, "", "ok", string(detail))
	writeJSON(w, http.StatusOK, map[string]any{"key_id": id.KeyID, "fingerprint": id.Fingerprint(),
		"keys": id.Document(s.now()).Keys, "dropped": len(dropped)})
}

func (s *Service) handleListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.peers.List(r.Context())
	var out []peerView
	if err == nil {
		out, err = s.views(r.Context(), "", peers...)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

func (s *Service) handleGetPeer(w http.ResponseWriter, r *http.Request) {
	if domain, ok := s.peerDomain(w, r, pathParam(r, "domain")); ok {
		s.writePeer(w, r, domain, http.StatusOK)
	}
}

// handleRetryPeer unpauses an allowed peer's delivery cursors and makes
// them due now (409 for a peer that is not allowed), and answers its
// view.
func (s *Service) handleRetryPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	p, err := s.peers.Get(ctx, domain)
	switch {
	case errors.Is(err, ErrPeerNotFound):
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	case p.Status != statusAllowed:
		writeErr(w, r, http.StatusConflict, "peer_not_allowed", "delivery goes to allowed peers only")
		return
	}
	res, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET paused = 0, attempts = 0, next_try_at = ? WHERE peer = ?`+peerAllowed,
		storage.FormatTS(s.now()), domain)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	s.DeliverSoon()
	s.audit(ctx, "federation.delivery.retry", domain, "ok", fmt.Sprintf(`{"cursors":%d}`, n))
	s.writePeer(w, r, domain, http.StatusOK)
}

// pathParam is r's path parameter key, decoded: the web UI encodes a
// domain's port (%3A) and an address's @ (%40), and chi leaves a
// parameter as the request spelled it. An invalid escape is "", which
// the caller refuses as invalid.
func pathParam(r *http.Request, key string) string {
	v, _ := url.PathUnescape(chi.URLParam(r, key))
	return v
}

// peerDomain returns the canonical form of raw, or answers 400 for an
// invalid domain or this server's own.
func (s *Service) peerDomain(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	domain, err := NormalizeDomain(raw)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
		return "", false
	}
	if domain == s.id().Domain {
		writeErr(w, r, http.StatusBadRequest, "own_domain", "this is the server's own domain, not a peer")
		return "", false
	}
	return domain, true
}

// lookup fetches domain's document. The operator chose the host, so
// the dialer treats it as allowed; lookups have their own limit. A
// document without a known peer's pinned key is audited as
// federation.key_changed.
func (s *Service) lookup(w http.ResponseWriter, r *http.Request, domain string) (Document, bool) {
	if !s.lookups.Allow() {
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many lookups, try again later")
		return Document{}, false
	}
	doc, err := s.fetch.document(r.Context(), domain, true)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", err.Error())
		return Document{}, false
	}
	if p, err := s.peers.Get(r.Context(), domain); err == nil && p.Fingerprint != "" && keyIn(doc, p.Fingerprint) == nil {
		s.keyChanged(r.Context(), domain, doc.Keys[0].Fingerprint, p.Fingerprint, "ok", "")
	}
	return doc, true
}

func (s *Service) handleLookup(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, r.URL.Query().Get("domain"))
	if !ok {
		return
	}
	doc, ok := s.lookup(w, r, domain)
	if !ok {
		return
	}
	k := doc.Keys[0]
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain, "fingerprint": k.Fingerprint, "key_id": k.KeyID})
}

func (s *Service) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	var in struct{ Domain, Fingerprint, Note string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	domain, ok := s.peerDomain(w, r, in.Domain)
	if !ok {
		return
	}
	if in.Fingerprint == "" {
		writeErr(w, r, http.StatusBadRequest, "fingerprint_required", "fingerprint is required")
		return
	}
	doc, ok := s.lookup(w, r, domain)
	if !ok {
		return
	}
	pub := keyIn(doc, in.Fingerprint)
	if pub == nil || Fingerprint(pub) != in.Fingerprint {
		writeErr(w, r, http.StatusConflict, "fingerprint_mismatch", "no key of "+domain+" has that fingerprint")
		return
	}
	if err := s.peers.Allow(r.Context(), domain, pub, in.Note); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.dropLimiters(domain)
	detail, _ := json.Marshal(map[string]string{"fingerprint": in.Fingerprint})
	s.audit(r.Context(), "federation.peer.add", domain, "ok", string(detail))
	s.refreshSoon(domain)
	s.writePeer(w, r, domain, http.StatusCreated)
}

func (s *Service) handleBlockPeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	if err := s.peers.Block(r.Context(), domain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.dropLimiters(domain)
	if err := s.dropPeer(r.Context(), domain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.audit(r.Context(), "federation.peer.block", domain, "ok", "")
	s.writePeer(w, r, domain, http.StatusOK)
}

func (s *Service) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	err := s.peers.Remove(r.Context(), domain)
	if err == nil {
		s.dropLimiters(domain)
		err = s.dropPeer(r.Context(), domain)
	}
	switch {
	case errors.Is(err, ErrPeerNotFound):
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		s.audit(r.Context(), "federation.peer.remove", domain, "ok", "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePurgePeer deletes what a blocked peer left here (purgePeer) and
// answers the counts: 404 for an unknown peer, 409 peer_not_blocked
// unless it is blocked. Audit federation.peer.purge with the counts.
func (s *Service) handlePurgePeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	if _, err := s.peers.Get(ctx, domain); errors.Is(err, ErrPeerNotFound) {
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
		return
	} else if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	n, err := s.purgePeer(ctx, domain)
	switch {
	case errors.Is(err, errNotBlocked):
		writeErr(w, r, http.StatusConflict, "peer_not_blocked", err.Error())
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	detail, _ := json.Marshal(n)
	s.audit(ctx, "federation.peer.purge", domain, "ok", string(detail))
	writeJSON(w, http.StatusOK, n)
}

// handleForgetKeys drops domain's key history, the keys it was pinned
// to before: author signatures made with them no longer verify here.
// It is the remedy when a retired key may have leaked. It answers how
// many keys were dropped; audit federation.peer.keys_forget.
func (s *Service) handleForgetKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM federation_peer_keys WHERE domain = ?`, domain)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.audit(ctx, "federation.peer.keys_forget", domain, "ok", fmt.Sprintf(`{"dropped":%d}`, n))
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain, "dropped": n})
}

func (s *Service) handlePingPeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	rtt, err := s.Ping(r.Context(), domain)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain, "rtt_ms": rtt.Milliseconds()})
}

// blockedBot is a row of blocked_bots as the admin API answers it;
// created_by is the admin key that blocked the bot.
type blockedBot struct {
	Address   string `json:"address"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	CreatedBy *int64 `json:"created_by"`
}

// blockedBots returns the blocked bots by address, or only address
// unless it is "".
func (s *Service) blockedBots(ctx context.Context, address string) ([]blockedBot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT address, note, created_at, created_by FROM blocked_bots
		WHERE ? IN ('', address) ORDER BY address`, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []blockedBot{}
	for rows.Next() {
		var b blockedBot
		if err := rows.Scan(&b.Address, &b.Note, &b.CreatedAt, &b.CreatedBy); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, b.CreatedAt); err == nil {
			b.CreatedAt = t.UTC().Format(time.RFC3339)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// botAddress returns the canonical form of raw, the address of a bot of
// another server (name@domain, name a bot's handle, operator or
// key-<id>), or answers 400 itself.
func (s *Service) botAddress(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	name, domain, _ := strings.Cut(raw, "@")
	h, herr := bots.NormalizeRemoteName(name)
	d, derr := NormalizeDomain(domain)
	switch {
	case herr != nil || derr != nil:
		writeErr(w, r, http.StatusBadRequest, "invalid_address", "address must be name@domain, name a bot's handle")
	case d == s.id().Domain:
		writeErr(w, r, http.StatusBadRequest, "own_domain", "a bot of this server is not blocked: revoke its key")
	default:
		return h + "@" + d, true
	}
	return "", false
}

func (s *Service) handleListBlocked(w http.ResponseWriter, r *http.Request) {
	out, err := s.blockedBots(r.Context(), "")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked_bots": out})
}

// handleBlockBot blocks a bot of another server and cuts it off
// (blockBot), and answers its row: 201, or 200 when it was blocked
// already, after the cleanup ran again, which finishes one that failed.
// Audit federation.bot.block with the note and what ended.
func (s *Service) handleBlockBot(w http.ResponseWriter, r *http.Request) {
	var in struct{ Address, Note string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	address, ok := s.botAddress(w, r, in.Address)
	if !ok {
		return
	}
	ctx := r.Context()
	c, _ := auth.FromContext(ctx)
	created, ended, err := s.blockBot(ctx, address, in.Note, c.KeyID)
	var rows []blockedBot
	if err == nil {
		rows, err = s.blockedBots(ctx, address)
	}
	if err == nil && len(rows) == 0 {
		err = errors.New("the block was lifted meanwhile")
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	detail, _ := json.Marshal(struct {
		Note string `json:"note"`
		blockCounts
	}{in.Note, ended})
	s.auditTarget(ctx, "federation.bot.block", "bot", address, "", "ok", string(detail))
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, rows[0])
}

// handleUnblockBot lifts the block of a bot of another server
// (unblockBot): its memberships and follows stay ended. 204, or 404 when
// it is not blocked. Audit federation.bot.unblock.
func (s *Service) handleUnblockBot(w http.ResponseWriter, r *http.Request) {
	address, ok := s.botAddress(w, r, pathParam(r, "address"))
	if !ok {
		return
	}
	switch lifted, err := s.unblockBot(r.Context(), address); {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
	case !lifted:
		writeErr(w, r, http.StatusNotFound, "not_found", address+" is not blocked")
	default:
		s.auditTarget(r.Context(), "federation.bot.unblock", "bot", address, "", "ok", "")
		w.WriteHeader(http.StatusNoContent)
	}
}
