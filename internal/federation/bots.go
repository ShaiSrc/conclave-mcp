package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/storage"
)

const (
	maxBotsBody    = 2 << 20 // responses to bot and directory requests
	directoryPage  = 50      // bots per /fed/v1/directory page, and kept of a fetched one
	directoryPages = 20      // pages read per peer and refresh
	maxPeerBots    = 1000    // cached bots per peer
	maxDirectory   = 5000    // entries of /v1/directory
	directoryEvery = 15 * time.Minute
	refreshTimeout = 30 * time.Second // per peer and refresh
	lookupsPerKey  = 60               // remote lookups per key and minute
	lookupKeyBurst = 10
)

// entry is a bot as a lookup or the directory return it: its profile,
// its address and whether it lives on a peer.
type entry struct {
	bots.Profile
	Address string `json:"address"`
	Remote  bool   `json:"remote"`
}

// shared returns the least visibility the verified origin reads: peers
// for an allowed peer, public for a seen one (open mode).
func (s *Service) shared(ctx context.Context) (string, error) {
	p, err := s.peers.Get(ctx, OriginFrom(ctx))
	switch {
	case err != nil && !errors.Is(err, ErrPeerNotFound):
		return "", err
	case p.Status == statusAllowed:
		return bots.Peers, nil
	}
	return bots.Public, nil
}

// handleFedBot answers GET /fed/v1/bots/{name}: a public bot, or a
// peers one for an allowed peer. Anything else gets the 404 of an
// unknown name.
func (s *Service) handleFedBot(w http.ResponseWriter, r *http.Request) {
	least, err := s.shared(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h, err := bots.NormalizeHandle(chi.URLParam(r, "name"))
	var p bots.Profile
	if err == nil {
		p, err = s.botStore.Get(r.Context(), h)
	}
	switch {
	case err == nil && (p.Visibility == bots.Public || p.Visibility == least):
		p.TrustOrigin = ""
		writeBotJSON(w, p)
	case err == nil, errors.Is(err, bots.ErrInvalid), errors.Is(err, bots.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
	default:
		s.fail(w, r, err)
	}
}

// handleFedDirectory answers GET /fed/v1/directory?page=N (from 1): the
// public bots, and the peers ones for an allowed peer, 50 a page, by
// handle, and the next page number or null.
func (s *Service) handleFedDirectory(w http.ResponseWriter, r *http.Request) {
	page := 1
	if q := r.URL.Query().Get("page"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "page must be a number from 1")
			return
		}
		page = n
	}
	least, err := s.shared(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// ponytail: loads every listed bot for each page; page in SQL if
	// directories grow into the thousands.
	all, err := s.botStore.List(r.Context(), least)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range all {
		all[i].TrustOrigin = ""
	}
	out, next := []bots.Profile{}, (*int)(nil)
	if pages := (len(all) + directoryPage - 1) / directoryPage; page <= pages {
		out = all[(page-1)*directoryPage : min(page*directoryPage, len(all))]
		if page < pages {
			n := page + 1
			next = &n
		}
	}
	writeBotJSON(w, map[string]any{"bots": out, "next": next})
}

// writeBotJSON writes a 200 without escaping <, > and &, which would
// take 6 bytes each and push a full page over the peer's fetch cap.
func writeBotJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// BotRoutes mounts on the /v1 router, which authenticates every
// request:
//
//	GET /bots/lookup?handle=name[@domain]  any key: one bot, local or on a peer
//	GET /directory                         any key: the visible local bots, then the cached ones of peers
//
// svc is nil with federation off: then a bare handle is looked up
// locally, any domain answers 404 federation_disabled, and the
// directory lists local bots only, addressed by their bare handle.
func BotRoutes(r chi.Router, store *bots.Store, svc *Service) {
	a := botAPI{store: store, svc: svc}
	r.Get("/bots/lookup", a.lookup)
	r.Get("/directory", a.directory)
}

type botAPI struct {
	store *bots.Store
	svc   *Service
}

// local returns the entry of a local bot.
func (a botAPI) local(p bots.Profile) entry {
	e := entry{Profile: p, Address: p.Handle}
	if a.svc != nil {
		e.Address += "@" + a.svc.id.Domain
	}
	return e
}

func (a botAPI) lookup(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.FromContext(r.Context())
	name, domain, qualified := strings.Cut(r.URL.Query().Get("handle"), "@")
	h, err := bots.NormalizeHandle(name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if qualified {
		if a.svc == nil {
			writeErr(w, r, http.StatusNotFound, "federation_disabled", "federation is not enabled on this server")
			return
		}
		d, err := NormalizeDomain(domain)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
			return
		}
		if d != a.svc.id.Domain {
			a.svc.lookupRemote(w, r, c.KeyID, h, d)
			return
		}
	}
	p, err := a.store.Get(r.Context(), h)
	switch {
	case err == nil && bots.Visible(c, p):
		writeJSON(w, http.StatusOK, a.local(p))
	case err == nil, errors.Is(err, bots.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
	default:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a botAPI) directory(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.FromContext(r.Context())
	all, err := a.store.List(r.Context(), bots.Private)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	// ponytail: one answer of at most 5000 entries, the rest left out;
	// page it (?page=N, like /fed/v1/directory) if directories outgrow it.
	out := []entry{}
	for _, p := range all {
		if bots.Visible(c, p) && len(out) < maxDirectory {
			out = append(out, a.local(p))
		}
	}
	if a.svc != nil && len(out) < maxDirectory {
		remote, err := a.svc.cachedBots(r.Context(), maxDirectory-len(out))
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, remote...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": out})
}

// lookupRemote asks domain for its bot name, for key keyID. Only an
// allowed peer, or a seen one in open mode, is asked: a lookup cannot
// make the server contact any other host. Each key has its own limit.
func (s *Service) lookupRemote(w http.ResponseWriter, r *http.Request, keyID int64, name, domain string) {
	if lim := s.lookupLimiter(keyID); !lim.Allow() {
		w.Header().Set("Retry-After", retryAfter(lim))
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many remote lookups, try again later")
		return
	}
	peer, err := s.peers.Get(r.Context(), domain)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		s.fail(w, r, err)
		return
	}
	if peer.Status != statusAllowed && (s.mode != modeOpen || peer.Status != statusSeen) {
		writeErr(w, r, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
		return
	}
	status, body, err := s.getSigned(r.Context(), domain, "/fed/v1/bots/"+name)
	switch {
	case err != nil:
		s.logger.Warn("federation bot lookup failed", "domain", domain, "err", err)
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", "peer did not answer")
		return
	case status == http.StatusNotFound:
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot on "+domain)
		return
	case status != http.StatusOK:
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", fmt.Sprintf("%s answered with status %d", domain, status))
		return
	}
	var p bots.Profile
	err = json.Unmarshal(body, &p)
	p, ok := bots.CleanRemote(p)
	if err != nil || !ok || p.Handle != name {
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", domain+" did not answer with the profile of "+name)
		return
	}
	writeJSON(w, http.StatusOK, entry{Profile: p, Address: name + "@" + domain, Remote: true})
}

// getSigned sends a signed GET of path to domain and returns the
// status and the body, read up to 2 MiB.
func (s *Service) getSigned(ctx context.Context, domain, path string) (int, []byte, error) {
	return s.sendSigned(ctx, http.MethodGet, domain, path, nil, maxBotsBody)
}

// lookupLimiter returns the remote-lookup limiter of key keyID.
// ponytail: one per key, never evicted; keys are bounded by the table.
func (s *Service) lookupLimiter(keyID int64) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	lim := s.keyLimiters[keyID]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(time.Minute/lookupsPerKey), lookupKeyBurst)
		s.keyLimiters[keyID] = lim
	}
	return lim
}

// cachedBots returns up to limit cached bots of peers, by domain and
// handle.
func (s *Service) cachedBots(ctx context.Context, limit int) ([]entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, profile FROM remote_bots ORDER BY domain, handle LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var domain, profile string
		if err := rows.Scan(&domain, &profile); err != nil {
			return nil, err
		}
		var p bots.Profile
		if err := json.Unmarshal([]byte(profile), &p); err != nil {
			return nil, err
		}
		p.TrustOrigin = bots.External
		out = append(out, entry{Profile: p, Address: p.Handle + "@" + domain, Remote: true})
	}
	return out, rows.Err()
}

// refreshSoon asks the refresh loop to refresh domain's directory next.
// With the loop busy or not running the request is dropped; the
// 15-minute refresh catches up.
func (s *Service) refreshSoon(domain string) {
	select {
	case s.refresh <- domain:
	default:
	}
}

// refreshLoop refreshes the cached directories of the allowed peers
// every 15 minutes, and of a peer right after it is added, until ctx is
// done.
func (s *Service) refreshLoop(ctx context.Context) {
	t := time.NewTicker(directoryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshDirectories(ctx)
		case domain := <-s.refresh:
			s.refreshLogged(ctx, domain)
		}
	}
}

// refreshDirectories refreshes the cached directory of every allowed peer.
func (s *Service) refreshDirectories(ctx context.Context) {
	peers, err := s.peers.List(ctx)
	if err != nil {
		s.logger.Warn("federation directory refresh failed", "err", err)
		return
	}
	for _, p := range peers {
		if p.Status == statusAllowed {
			s.refreshLogged(ctx, p.Domain)
		}
	}
}

// refreshLogged refreshes domain's directory within 30 seconds and logs
// a failure, unless ctx ended.
func (s *Service) refreshLogged(ctx context.Context, domain string) {
	peerCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := s.refreshDirectory(peerCtx, domain); err != nil && ctx.Err() == nil {
		s.logger.Warn("federation directory refresh failed", "domain", domain, "err", err)
	}
}

// refreshDirectory reads up to 20 pages of domain's directory and
// replaces its cached bots with them, cleaned: at most 50 of a page and
// 1000 in all. Entries with an invalid handle, one naming a server
// included, are dropped. On an error the old rows stay. A peer no longer
// allowed when the rows are written gets none.
func (s *Service) refreshDirectory(ctx context.Context, domain string) error {
	var got []bots.Profile
	cut := false
	for page := 1; page <= directoryPages && len(got) < maxPeerBots; page++ {
		status, body, err := s.getSigned(ctx, domain, "/fed/v1/directory?page="+strconv.Itoa(page))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("directory of %s: status %d", domain, status)
		}
		var out struct {
			Bots []bots.Profile `json:"bots"`
			Next *int           `json:"next"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return fmt.Errorf("directory of %s: %w", domain, err)
		}
		if len(out.Bots) > directoryPage {
			out.Bots, cut = out.Bots[:directoryPage], true
		}
		for _, p := range out.Bots {
			if p, ok := bots.CleanRemote(p); ok && len(got) < maxPeerBots {
				got = append(got, p)
			}
		}
		if out.Next == nil {
			break
		}
	}
	if cut {
		s.logger.Warn("federation directory page over 50 bots, the rest dropped", "domain", domain)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM remote_bots WHERE domain = ?`, domain); err != nil {
		return err
	}
	// Checked after the delete has taken the write lock: a block or
	// remove either came before (no rows) or deletes these rows after.
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM federation_peers WHERE domain = ?`, domain).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if status == statusAllowed {
		now := storage.FormatTS(time.Now())
		for _, p := range got {
			profile, _ := json.Marshal(p)
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO remote_bots(domain, handle, profile, fetched_at)
				VALUES (?, ?, ?, ?)`, domain, p.Handle, string(profile), now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
