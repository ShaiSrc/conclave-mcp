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
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/audit"
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

var (
	errUnknownKey   = errors.New("the origin's document does not list the key")
	errKeyFetchWait = errors.New("peer key fetched less than a minute ago")
)

// keyChangedError: a peer's document no longer lists its pinned key.
type keyChangedError struct{ presented string }

func (e *keyChangedError) Error() string { return "the peer's document lists another key" }

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
		if peer.Status == statusBlocked || s.mode != modeOpen && peer.Status != statusAllowed {
			s.refuse(w, r, origin, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
			return
		}

		// 3 (open mode, unknown: TOFU) and 4 (the pinned or recorded key).
		var pub ed25519.PublicKey
		if known {
			if want := keyIDOf(peer.Fingerprint); want == "" || p.KeyID != want {
				s.keyChanged(ctx, origin, p.KeyID, peer.Fingerprint)
				s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "not the peer's pinned key")
				return
			}
			pub, err = s.peerKey(ctx, origin, peer.Fingerprint)
		} else {
			var doc Document
			if doc, err = s.fetch.Document(ctx, origin); err == nil {
				if pub = keyIn(doc, p.KeyID); pub == nil || KeyIDFor(pub) != p.KeyID {
					err = errUnknownKey
				}
			}
		}
		if err != nil {
			var changed *keyChangedError
			switch {
			case errors.As(err, &changed):
				s.keyChanged(ctx, origin, changed.presented, peer.Fingerprint)
				s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "the peer's document lists another key")
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
			fp := Fingerprint(pub)
			inserted, err := s.peers.RecordSeen(ctx, origin, fp)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			if inserted {
				detail, _ := json.Marshal(map[string]string{"fingerprint": fp})
				_ = audit.Write(ctx, s.db, 0, "federation.peer.seen", "peer", origin, "", "ok", string(detail))
			}
			s.cacheKey(pub)
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

// keyIDOf returns the key ID that belongs to a fingerprint (KeyIDFor
// derives it from the key), or "" for a malformed one.
func keyIDOf(fingerprint string) string {
	fp, ok := strings.CutPrefix(fingerprint, "SHA256:")
	if !ok || len(fp) < 16 {
		return ""
	}
	return "ed25519:" + fp[:16]
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

// peerKey returns the key with fingerprint fp, from the cache or from
// domain's document.
func (s *Service) peerKey(ctx context.Context, domain, fp string) (ed25519.PublicKey, error) {
	s.mu.Lock()
	pub := s.keys[fp]
	s.mu.Unlock()
	if pub != nil {
		return pub, nil
	}
	// ponytail: one fetch per peer a minute while its key is not cached,
	// so forged requests naming a peer cannot make us hammer it. A second
	// concurrent first request gets 502; add singleflight if that matters.
	if !s.keyFetches.allow(domain) {
		return nil, errKeyFetchWait
	}
	doc, err := s.fetch.Document(ctx, domain)
	if err != nil {
		return nil, err
	}
	if pub = keyIn(doc, fp); pub == nil {
		return nil, &keyChangedError{presented: doc.Keys[0].KeyID}
	}
	s.cacheKey(pub)
	return pub, nil
}

// cacheKey remembers a peer key by its fingerprint; a fingerprint
// names one key, so entries never go stale.
func (s *Service) cacheKey(pub ed25519.PublicKey) {
	s.mu.Lock()
	s.keys[Fingerprint(pub)] = pub
	s.mu.Unlock()
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

// keyChanged audits a refused key for a known peer. Step 4 runs before
// any signature is verified, so anyone can trigger it: at most one
// entry per peer a minute.
func (s *Service) keyChanged(ctx context.Context, domain, presented, expected string) {
	if !s.keyAudits.allow(domain) {
		return
	}
	detail, _ := json.Marshal(struct {
		Presented string `json:"presented"`
		Expected  string `json:"expected"`
	}{presented, expected})
	_ = audit.Write(ctx, s.db, 0, "federation.key_changed", "peer", domain, "", "error:bad_signature", string(detail))
}

// refuse answers with an error and logs it at warn, at most one line
// per origin a minute.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, origin string, status int, code, msg string) {
	if s.warnLog.allow(origin) {
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
