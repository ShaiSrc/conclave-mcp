package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/storage"
)

const (
	maxSkew       = 300 * time.Second
	peerPerMinute = 120
	peerBurst     = 30
	modeOpen      = "open"
)

type originKey struct{}

// OriginFrom returns the origin domain that Verify put on ctx, or "".
func OriginFrom(ctx context.Context) string {
	s, _ := ctx.Value(originKey{}).(string)
	return s
}

var errUnknownKey = errors.New("the origin's document does not list the key")

// Verify admits only requests signed by a peer the policy lets in. It
// runs the spec's checks in order; nonces and seen peers are stored
// only once the signature has verified. The handler gets the origin
// (OriginFrom) and the body.
func (s *Service) Verify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// 1. Parse the header.
		p, err := ParseSigHeader(r.Header.Get("Authorization"))
		if err != nil {
			s.refuse(w, r, "", http.StatusUnauthorized, "bad_signature", "malformed signature header")
			return
		}
		// 2. Origin syntax, in canonical form.
		if d, err := NormalizeDomain(p.Origin); err != nil || d != p.Origin {
			s.refuse(w, r, "", http.StatusUnauthorized, "bad_signature", "invalid origin domain")
			return
		}
		origin := p.Origin

		// 3. Peer policy.
		peer, err := s.peers.Get(ctx, origin)
		known := err == nil
		if err != nil && !errors.Is(err, ErrPeerNotFound) {
			s.fail(w, r, err)
			return
		}
		if !s.admits(peer) {
			s.refuse(w, r, origin, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
			return
		}

		// 4. A known peer's key is its stored key, used without a fetch.
		// 3, open mode: an unknown origin's key comes from its document
		// (TOFU).
		pub := peer.PublicKey
		if known {
			if len(pub) != ed25519.PublicKeySize || p.KeyID != KeyIDFor(pub) {
				s.checkKeyChange(ctx, peer, p.KeyID)
				s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "not the peer's pinned key")
				return
			}
		} else if pub, err = s.firstContactKey(ctx, origin, p.KeyID); err != nil {
			switch {
			case errors.Is(err, errUnknownKey):
				s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", err.Error())
			case errors.Is(err, ErrFetchLimited):
				w.Header().Set("Retry-After", strconv.Itoa(int((time.Minute / fetchesPerMinute).Seconds())))
				s.refuse(w, r, origin, http.StatusTooManyRequests, "rate_limited", "too many unknown origins, try again later")
			default:
				s.refuse(w, r, origin, http.StatusBadGateway, "peer_unreachable", "cannot fetch the origin's document")
			}
			return
		}

		// 5. Destination.
		if d, err := NormalizeDomain(r.Host); err != nil || d != s.id.Domain {
			s.refuse(w, r, origin, http.StatusMisdirectedRequest, "wrong_destination", "request is for another server")
			return
		}
		// 6. Timestamp.
		if !inWindow(p.TS) {
			s.refuse(w, r, origin, http.StatusUnauthorized, "clock_skew", "timestamp outside ±300 s")
			return
		}
		// 7. Body and signature.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
				s.refuse(w, r, origin, http.StatusRequestEntityTooLarge, "too_large", "body over 64 KiB")
			} else {
				s.refuse(w, r, origin, http.StatusBadRequest, "bad_request", "cannot read body")
			}
			return
		}
		if !ed25519.Verify(pub, SigningString(r.Method, r.RequestURI, s.id.Domain, origin, p.TS, p.Nonce, body), p.Sig) {
			s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "signature does not verify")
			return
		}
		// 8. Nonce. The window is checked again first: a slow body
		// could otherwise outlast the stored nonce of an earlier copy.
		if !inWindow(p.TS) {
			s.refuse(w, r, origin, http.StatusUnauthorized, "clock_skew", "timestamp outside ±300 s")
			return
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO federation_nonces(origin, nonce, expires_at)
			VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, origin, p.Nonce, storage.FormatTS(time.Unix(p.TS, 0).Add(maxSkew)))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "nonce already used")
			return
		}
		// 9. Per-peer rate limit, on verified requests only.
		if lim := s.limiter(origin); !lim.Allow() {
			rsv := lim.Reserve()
			delay := rsv.Delay()
			rsv.Cancel()
			if delay < time.Second {
				delay = time.Second
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(delay.Seconds())))
			s.refuse(w, r, origin, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
			return
		}
		// 10. Record a first contact; update last seen.
		if !known {
			inserted, err := s.peers.RecordSeen(ctx, origin, pub)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			if inserted {
				detail, _ := json.Marshal(map[string]string{"fingerprint": Fingerprint(pub)})
				s.audit(ctx, "federation.peer.seen", origin, "ok", string(detail))
			} else {
				// A row appeared since step 3 (another first request, an
				// operator); it decides.
				row, err := s.peers.Get(ctx, origin)
				if err != nil {
					s.fail(w, r, err)
					return
				}
				if !s.admits(row) {
					s.refuse(w, r, origin, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
					return
				}
				if row.Fingerprint != Fingerprint(pub) {
					s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "not the peer's pinned key")
					return
				}
			}
		}
		if err := s.peers.Touch(ctx, origin); err != nil {
			s.fail(w, r, err)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, originKey{}, origin)))
	})
}

func inWindow(ts int64) bool {
	d := time.Since(time.Unix(ts, 0))
	return -maxSkew <= d && d <= maxSkew
}

// admits reports whether the policy lets p in; the zero Peer is an
// unknown origin.
func (s *Service) admits(p Peer) bool {
	return p.Status != statusBlocked && (s.mode == modeOpen || p.Status == statusAllowed)
}

// keyIn returns the key in doc whose key ID or fingerprint is want, or
// nil. The Fetcher has checked that each key matches both.
func keyIn(doc Document, want string) ed25519.PublicKey {
	for _, k := range doc.Keys {
		if k.KeyID == want || k.Fingerprint == want {
			pub, _ := b64.DecodeString(k.PublicKey)
			return pub
		}
	}
	return nil
}

// firstContactKey fetches the document of an unknown origin and returns
// its key with ID keyID.
func (s *Service) firstContactKey(ctx context.Context, origin, keyID string) (ed25519.PublicKey, error) {
	doc, err := s.fetch.Document(ctx, origin)
	if err != nil {
		return nil, err
	}
	if pub := keyIn(doc, keyID); pub != nil && KeyIDFor(pub) == keyID {
		return pub, nil
	}
	return nil, errUnknownKey
}

// limiter returns origin's rate limiter. ponytail: one per verified
// peer, never evicted; peers are bounded by the table and the fetch
// budget.
func (s *Service) limiter(origin string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	lim := s.limiters[origin]
	if lim == nil {
		lim = rate.NewLimiter(rate.Limit(peerPerMinute/60.0), peerBurst)
		s.limiters[origin] = lim
	}
	return lim
}

// checkKeyChange runs when a request for a known peer names a key
// other than its stored one. Anyone can send that, so it is audited
// only on evidence: the peer's own document lists the presented key.
// The document is fetched at most once per peer a minute.
func (s *Service) checkKeyChange(ctx context.Context, peer Peer, presented string) {
	if !s.keyChecks.allow(peer.Domain) {
		return
	}
	doc, err := s.fetch.Document(ctx, peer.Domain)
	if err != nil {
		return
	}
	if pub := keyIn(doc, presented); pub != nil && KeyIDFor(pub) == presented {
		s.keyChanged(ctx, peer.Domain, Fingerprint(pub), peer.Fingerprint, "error:bad_signature")
	}
}

// keyChanged audits evidence that domain serves a key other than its
// pinned one.
func (s *Service) keyChanged(ctx context.Context, domain, presented, expected, result string) {
	detail, _ := json.Marshal(struct {
		Presented string `json:"presented"`
		Expected  string `json:"expected"`
	}{presented, expected})
	s.audit(ctx, "federation.key_changed", domain, result, string(detail))
}

// refuse answers with an error and logs it at warn, at most one line
// per origin and code a minute, so a forged origin cannot silence
// another code.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, origin string, status int, code, msg string) {
	if s.warnLog.allow(origin + " " + code) {
		s.logger.Warn("federation request refused", "origin", origin, "status", status, "code", code, "reason", msg)
	}
	writeErr(w, r, status, code, msg)
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("federation request failed", "err", err)
	writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
}

// maxGateKeys bounds a minuteGate: origins on the log path are chosen
// by strangers.
const maxGateKeys = 1000

// minuteGate lets one event per key through each minute.
type minuteGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (g *minuteGate) allow(key string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.last[key]; ok && now.Sub(t) < time.Minute {
		return false
	}
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	if len(g.last) >= maxGateKeys {
		for k, t := range g.last {
			if now.Sub(t) >= time.Minute {
				delete(g.last, k)
			}
		}
		if len(g.last) >= maxGateKeys {
			return false
		}
	}
	g.last[key] = now
	return true
}

// handlePing answers a verified ping. The body is empty or {}.
func (s *Service) handlePing(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body) // read and capped by Verify
	if len(bytes.TrimSpace(body)) > 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil || m == nil || len(m) > 0 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "ping takes an empty JSON object")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"domain": s.id.Domain})
}

// pruneNonces deletes nonces that expired over a minute before now. The
// minute covers a request that passed the window check just before its
// nonce's expiry and stores the nonce a moment later.
func (s *Service) pruneNonces(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM federation_nonces WHERE expires_at < ?`,
		storage.FormatTS(now.Add(-time.Minute)))
	return err
}

// Run deletes expired nonces every minute until ctx is done.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := s.pruneNonces(ctx, now); err != nil {
				s.logger.Warn("federation nonce pruning failed", "err", err)
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr writes the SPEC-0003 §5 error envelope. The request ID is
// the one the server's middleware put on the response.
func writeErr(w http.ResponseWriter, _ *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": w.Header().Get("X-Request-Id"),
	}})
}
