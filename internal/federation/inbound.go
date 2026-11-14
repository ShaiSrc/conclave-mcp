package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/storage"
)

const (
	maxSkew  = 300 * time.Second
	modeOpen = "open"
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
// (OriginFrom) and the body, of at most 64 KiB; a message proposal's
// may be the message cap over that.
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

		// 4. A known peer's key is its stored key, used without a fetch,
		// unless it signs with a new key its pinned one endorsed (repin).
		// 3, open mode: an unknown origin's key comes from its document
		// (TOFU).
		pub := peer.PublicKey
		if known {
			if len(pub) != ed25519.PublicKeySize || p.KeyID != KeyIDFor(pub) {
				if pub, err = s.repin(ctx, peer, p.KeyID); err != nil {
					s.fail(w, r, err)
					return
				}
				if pub == nil {
					s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "not the peer's pinned key")
					return
				}
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
		if d, err := NormalizeDomain(r.Host); err != nil || d != s.id().Domain {
			s.refuse(w, r, origin, http.StatusMisdirectedRequest, "wrong_destination", "request is for another server")
			return
		}
		// 6. Timestamp.
		if !inWindow(p.TS) {
			s.refuse(w, r, origin, http.StatusUnauthorized, "clock_skew", "timestamp outside ±300 s")
			return
		}
		// 7. Body and signature.
		limit, tooLargeMsg := int64(maxBody), "body over 64 KiB"
		switch {
		case r.Method != http.MethodPost:
		case roomPath(r.URL.Path, "/messages"):
			limit, tooLargeMsg = limit+int64(s.Rooms.MaxMessageBytes), "body over 64 KiB and the message cap"
		case roomPath(r.URL.Path, "/events"):
			limit, tooLargeMsg = int64(s.eventsLimit()), "body over 64 KiB and twice the message cap"
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
				s.refuse(w, r, origin, http.StatusRequestEntityTooLarge, "too_large", tooLargeMsg)
			} else {
				s.refuse(w, r, origin, http.StatusBadRequest, "bad_request", "cannot read body")
			}
			return
		}
		if !ed25519.Verify(pub, SigningString(r.Method, r.RequestURI, s.id().Domain, origin, p.TS, p.Nonce, body), p.Sig) {
			s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "signature does not verify")
			return
		}
		// 8. Per-peer rate limit, on verified requests only. A replay of
		// a stored nonce is refused first, so replays cannot use up the
		// peer's budget; the limit comes before the INSERT so a refused
		// request writes no row.
		var used int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_nonces WHERE origin = ? AND nonce = ?`, origin, p.Nonce).Scan(&used); err != nil {
			s.fail(w, r, err)
			return
		}
		if used > 0 {
			s.refuse(w, r, origin, http.StatusUnauthorized, "bad_signature", "nonce already used")
			return
		}
		lims, err := s.limitersOf(ctx, origin)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !lims.requests.Allow() {
			w.Header().Set("Retry-After", retryAfter(lims.requests))
			s.refuse(w, r, origin, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
			return
		}
		// 9. Nonce. The window is checked again first: a slow body
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
		// Contact resumes delivery to the peer if it was paused.
		if err := s.resumeDelivery(ctx, origin); err != nil {
			s.fail(w, r, err)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, originKey{}, origin)))
	})
}

// roomPath reports whether p is /fed/v1/rooms/{id} followed by suffix.
func roomPath(p, suffix string) bool {
	rest, ok := strings.CutPrefix(p, "/fed/v1/rooms/")
	id, ok2 := strings.CutSuffix(rest, suffix)
	return ok && ok2 && id != "" && !strings.Contains(id, "/")
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

// retryAfter is the Retry-After value for lim: the wait for its next
// token, at least a second.
func retryAfter(lim *rate.Limiter) string { return retryAfterAt(lim, time.Now()) }

// retryAfterAt is retryAfter at time now, at most an hour: a limit of
// 0 has no next token.
func retryAfterAt(lim *rate.Limiter, now time.Time) string {
	rsv := lim.ReserveN(now, 1)
	delay := rsv.DelayFrom(now)
	rsv.CancelAt(now)
	return strconv.Itoa(int(min(max(delay, time.Second), time.Hour).Seconds()))
}

// repin runs when a request of a known peer names a key other than its
// pinned one, and returns the key to verify the request with, or nil to
// refuse it. Anyone can send such a request, so the peer's document is
// fetched at most once a minute (docs), and every key presented
// meanwhile is judged against that fetch. The presented key is pinned
// when it is the document's current key and adopt takes it. The change
// is audited (federation.key_changed) too when the document cannot be
// fetched, by the request that fetched it; a document whose current key
// is another, the peer's previous key included, is no evidence.
func (s *Service) repin(ctx context.Context, peer Peer, presented string) (ed25519.PublicKey, error) {
	if len(peer.PublicKey) != ed25519.PublicKeySize {
		return nil, nil
	}
	doc, fetched, err := s.docs.get(ctx, peer.Domain, s.fetch.Document)
	if err != nil {
		if fetched {
			s.keyChanged(ctx, peer.Domain, presented, peer.Fingerprint, "error:bad_signature", "unreachable")
		}
		return nil, nil
	}
	if doc.Keys[0].KeyID != presented { // newest first
		return nil, nil
	}
	return s.adopt(ctx, peer, doc.Keys[0])
}

// needDocError is repinAuthor's error when the document of domain must
// be fetched first, outside applyMu (applyBatch).
type needDocError struct{ domain string }

func (e *needDocError) Error() string { return "the document of " + e.domain + " is needed" }

// repinAuthor runs, under applyMu, when the author signature of a bot of
// peer, a third server we may see only through rooms hosted elsewhere,
// verifies with none of its keys: the peer may have rotated without a
// request to us. It adopts the current key of the peer's document from
// the last minute (docs) and returns it, or nil when there is none to
// adopt. It fetches nothing: without such a document it returns
// needDocError, for the caller to fetch one outside the lock and apply the
// event again, at most repinsPerMinute times a minute for each host;
// past that the signature is wrong.
func (s *Service) repinAuthor(ctx context.Context, host string, peer Peer) (ed25519.PublicKey, error) {
	doc, ok, err := s.docs.peek(peer.Domain)
	if !ok {
		lims, err := s.limitersOf(ctx, host)
		if err != nil || !lims.repins.Allow() {
			return nil, err
		}
		return nil, &needDocError{peer.Domain}
	}
	if err != nil || doc.Keys[0].KeyID == KeyIDFor(peer.PublicKey) {
		return nil, nil // unreachable, or no rotation: the signature is wrong
	}
	return s.adopt(ctx, peer, doc.Keys[0]) // newest first
}

// adopt pins k, a key of peer's document, when it is endorsed by the
// pinned key with a valid signature and the peer never had it before;
// the pinned key goes to the peer's history (federation.key_rotated).
// Otherwise it audits why not (federation.key_changed, once per key a
// minute) and returns nil.
func (s *Service) adopt(ctx context.Context, peer Peer, k DocKey) (ed25519.PublicKey, error) {
	pub, _ := b64.DecodeString(k.PublicKey) // checked by the Fetcher
	pinned := KeyIDFor(peer.PublicKey)
	var sig []byte
	if k.EndorsedBy != nil {
		sig, _ = decodeB64(k.EndorsedBy.Sig)
	}
	history, err := s.peers.History(ctx, peer.Domain)
	if err != nil {
		return nil, err
	}
	reason := ""
	switch {
	case k.EndorsedBy == nil || k.EndorsedBy.KeyID != pinned:
		reason = "not_endorsed"
	case len(sig) != ed25519.SignatureSize ||
		!ed25519.Verify(peer.PublicKey, EndorsementString(peer.Domain, pinned, k.KeyID, b64.EncodeToString(pub)), sig):
		reason = "bad_endorsement"
	case slices.ContainsFunc(history, func(h ed25519.PublicKey) bool { return KeyIDFor(h) == k.KeyID }):
		reason = "retired_key"
	}
	if reason != "" {
		if s.keyAudits.allow(peer.Domain + " " + k.KeyID) {
			s.keyChanged(ctx, peer.Domain, Fingerprint(pub), peer.Fingerprint, "error:bad_signature", reason)
		}
		return nil, nil
	}
	if ok, err := s.peers.Repin(ctx, peer.Domain, peer.PublicKey, pub); err != nil {
		return nil, err
	} else if !ok {
		// The pin changed meanwhile: a concurrent request re-pinned the
		// peer, maybe to this key, or the operator did.
		switch now, err := s.peers.Get(ctx, peer.Domain); {
		case errors.Is(err, ErrPeerNotFound):
			return nil, nil
		case err != nil:
			return nil, err
		case !bytes.Equal(now.PublicKey, pub):
			return nil, nil
		}
		return pub, nil
	}
	detail, _ := json.Marshal(struct {
		Previous string `json:"previous"`
		Current  string `json:"current"`
	}{peer.Fingerprint, Fingerprint(pub)})
	s.audit(ctx, "federation.key_rotated", peer.Domain, "ok", string(detail))
	return pub, nil
}

// keyChanged audits evidence that domain serves a key other than its
// pinned one, and why it was not re-pinned (none for an operator's
// lookup).
func (s *Service) keyChanged(ctx context.Context, domain, presented, expected, result, reason string) {
	detail, _ := json.Marshal(struct {
		Presented string `json:"presented"`
		Expected  string `json:"expected"`
		Reason    string `json:"reason,omitempty"`
	}{presented, expected, reason})
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

// docCache keeps the documents of known peers fetched for repin and
// repinAuthor, a failed fetch too, each for a minute: anyone can
// present a key in a peer's name, so the document is fetched at most
// once a minute and every key presented meanwhile is judged against it.
// Concurrent callers share one fetch. It holds at most maxGateKeys
// documents, dropping stale ones when full.
type docCache struct {
	mu   sync.Mutex
	docs map[string]*cachedDoc
}

type cachedDoc struct {
	at   time.Time     // when the fetch started
	done chan struct{} // closed once doc and err are set
	doc  Document
	err  error
}

var errDocCacheFull = errors.New("too many peer documents cached")

// get returns domain's document, fetching it with fetch unless one
// from the last minute is cached; fetched reports whether this call
// fetched it. The fetch does not end with ctx: a caller that goes away
// must not leave a failure for the minute.
func (c *docCache) get(ctx context.Context, domain string, fetch func(context.Context, string) (Document, error)) (doc Document, fetched bool, err error) {
	now := time.Now()
	c.mu.Lock()
	e, ok := c.docs[domain]
	if !ok || now.Sub(e.at) >= time.Minute {
		if c.docs == nil {
			c.docs = make(map[string]*cachedDoc)
		}
		if len(c.docs) >= maxGateKeys {
			for k, e := range c.docs {
				if now.Sub(e.at) >= time.Minute {
					delete(c.docs, k)
				}
			}
			if len(c.docs) >= maxGateKeys {
				c.mu.Unlock()
				return Document{}, false, errDocCacheFull
			}
		}
		e = &cachedDoc{at: now, done: make(chan struct{})}
		c.docs[domain] = e
		c.mu.Unlock()
		e.doc, e.err = fetch(context.WithoutCancel(ctx), domain)
		close(e.done)
		return e.doc, true, e.err
	}
	c.mu.Unlock()
	select {
	case <-e.done:
		return e.doc, false, e.err
	case <-ctx.Done():
		return Document{}, false, ctx.Err()
	}
}

// peek returns domain's document from the last minute, or its fetch's
// error, without fetching; ok is false when there is none yet.
func (c *docCache) peek(domain string) (doc Document, ok bool, err error) {
	c.mu.Lock()
	e, found := c.docs[domain]
	c.mu.Unlock()
	if !found || time.Since(e.at) >= time.Minute {
		return Document{}, false, nil
	}
	select {
	case <-e.done:
		return e.doc, true, e.err
	default:
		return Document{}, false, nil
	}
}

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
	writeJSON(w, http.StatusOK, map[string]string{"domain": s.id().Domain})
}

// pruneNonces deletes nonces that expired over a minute before now. The
// minute covers a request that passed the window check just before its
// nonce's expiry and stores the nonce a moment later.
func (s *Service) pruneNonces(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM federation_nonces WHERE expires_at < ?`,
		storage.FormatTS(now.Add(-time.Minute)))
	return err
}

// Run deletes expired nonces every minute and runs the directory
// refresh loop, the delivery worker and the replicas' catch-up beside
// it, each in its own goroutine, until ctx is done; it returns once all
// have stopped.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){s.refreshLoop, s.deliveryLoop, s.catchupLoop} {
		wg.Add(1)
		go func() { defer wg.Done(); loop(ctx) }()
	}
	defer wg.Wait()
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
