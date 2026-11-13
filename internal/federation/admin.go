package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
)

// Operator lookups (and adds, which look up) have their own budget,
// apart from the fetches for strangers.
const (
	lookupsPerMinute = 30
	lookupBurst      = 10
)

// AdminRoutes mounts the operator endpoints under /v1/federation. The
// caller mounts them behind authentication; every handler also needs
// an admin key.
func (s *Service) AdminRoutes(r chi.Router) {
	r.Route("/federation", func(r chi.Router) {
		r.Use(adminOnly)
		r.Get("/", s.handleStatus)
		r.Get("/peers", s.handleListPeers)
		r.Get("/lookup", s.handleLookup)
		r.Post("/peers", s.handleAddPeer)
		r.Post("/peers/{domain}/block", s.handleBlockPeer)
		r.Delete("/peers/{domain}", s.handleRemovePeer)
		r.Post("/peers/{domain}/ping", s.handlePingPeer)
	})
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
	Domain      string  `json:"domain"`
	Status      string  `json:"status"`
	Fingerprint string  `json:"fingerprint"`
	KeyID       string  `json:"key_id"`
	Note        string  `json:"note"`
	AddedAt     string  `json:"added_at"`
	LastSeenAt  *string `json:"last_seen_at"`
}

func viewOf(p Peer) peerView {
	v := peerView{Domain: p.Domain, Status: p.Status, Fingerprint: p.Fingerprint, Note: p.Note,
		AddedAt: p.AddedAt.UTC().Format(time.RFC3339)}
	if len(p.PublicKey) == ed25519.PublicKeySize {
		v.KeyID = KeyIDFor(p.PublicKey)
	}
	if p.LastSeenAt != nil {
		t := p.LastSeenAt.UTC().Format(time.RFC3339)
		v.LastSeenAt = &t
	}
	return v
}

// audit writes an entry about the peer domain. The actor is the
// caller's key, or 0 on the federation listener.
func (s *Service) audit(ctx context.Context, action, domain, result, detail string) {
	var actor int64
	if c, ok := auth.FromContext(ctx); ok {
		actor = c.KeyID
	}
	if err := audit.Write(ctx, s.db, actor, action, "federation_peer", domain, "", result, detail); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{"domain": s.id.Domain, "mode": s.mode,
		"fingerprint": s.id.Fingerprint(), "key_id": s.id.KeyID, "peers": n})
}

func (s *Service) handleListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.peers.List(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := make([]peerView, 0, len(peers))
	for _, p := range peers {
		out = append(out, viewOf(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

// peerDomain returns the canonical form of raw, or answers 400 for an
// invalid domain or this server's own.
func (s *Service) peerDomain(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	domain, err := NormalizeDomain(raw)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
		return "", false
	}
	if domain == s.id.Domain {
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
		s.keyChanged(r.Context(), domain, doc.Keys[0].Fingerprint, p.Fingerprint, "ok")
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
	detail, _ := json.Marshal(map[string]string{"fingerprint": in.Fingerprint})
	s.audit(r.Context(), "federation.peer.add", domain, "ok", string(detail))
	s.refreshSoon(domain)
	p, err := s.peers.Get(r.Context(), domain)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(p))
}

func (s *Service) handleBlockPeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, chi.URLParam(r, "domain"))
	if !ok {
		return
	}
	if err := s.peers.Block(r.Context(), domain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.audit(r.Context(), "federation.peer.block", domain, "ok", "")
	p, err := s.peers.Get(r.Context(), domain)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, viewOf(p))
}

func (s *Service) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, chi.URLParam(r, "domain"))
	if !ok {
		return
	}
	switch err := s.peers.Remove(r.Context(), domain); {
	case errors.Is(err, ErrPeerNotFound):
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		s.audit(r.Context(), "federation.peer.remove", domain, "ok", "")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Service) handlePingPeer(w http.ResponseWriter, r *http.Request) {
	domain, ok := s.peerDomain(w, r, chi.URLParam(r, "domain"))
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
