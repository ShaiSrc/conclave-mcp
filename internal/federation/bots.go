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

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/storage"
)

const (
	maxBotsBody    = 256 << 10 // responses to bot and directory requests
	directoryPage  = 100       // bots per /fed/v1/directory page
	directoryPages = 10        // pages read per peer and refresh
	directoryEvery = 15 * time.Minute
)

// entry is a bot as a lookup or the directory return it: its profile,
// its address and whether it lives on a peer.
type entry struct {
	bots.Profile
	Address string `json:"address"`
	Remote  bool   `json:"remote"`
}

// handleFedBot answers GET /fed/v1/bots/{name}: a peers or public bot.
// Private and unknown names get the same 404.
func (s *Service) handleFedBot(w http.ResponseWriter, r *http.Request) {
	h, err := bots.NormalizeHandle(chi.URLParam(r, "name"))
	var p bots.Profile
	if err == nil {
		p, err = s.botStore.Get(r.Context(), h)
	}
	switch {
	case err == nil && p.Visibility != bots.Private:
		writeJSON(w, http.StatusOK, p)
	case err == nil, errors.Is(err, bots.ErrInvalid), errors.Is(err, bots.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
	default:
		s.fail(w, r, err)
	}
}

// handleFedDirectory answers GET /fed/v1/directory?page=N (from 1): the
// peers and public bots, 100 a page, by handle, and the next page
// number or null.
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
	// ponytail: loads every listed bot for each page; page in SQL if
	// directories grow into the thousands.
	all, err := s.botStore.List(r.Context(), bots.Peers)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, next := []bots.Profile{}, (*int)(nil)
	if pages := (len(all) + directoryPage - 1) / directoryPage; page <= pages {
		out = all[(page-1)*directoryPage : min(page*directoryPage, len(all))]
		if page < pages {
			n := page + 1
			next = &n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": out, "next": next})
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
			a.svc.lookupRemote(w, r, h, d)
			return
		}
	}
	c, _ := auth.FromContext(r.Context())
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
	out := []entry{}
	for _, p := range all {
		if bots.Visible(c, p) {
			out = append(out, a.local(p))
		}
	}
	if a.svc != nil {
		remote, err := a.svc.cachedBots(r.Context())
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, remote...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": out})
}

// lookupRemote asks domain for its bot name, if the policy lets the
// peer in. A domain that is not allowed spends the budget of fetches
// for strangers.
func (s *Service) lookupRemote(w http.ResponseWriter, r *http.Request, name, domain string) {
	peer, err := s.peers.Get(r.Context(), domain)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		s.fail(w, r, err)
		return
	}
	if !s.admits(peer) {
		writeErr(w, r, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
		return
	}
	if peer.Status != statusAllowed && !s.fetch.limit.Allow() {
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", ErrFetchLimited.Error())
		return
	}
	status, body, err := s.getSigned(r.Context(), domain, "/fed/v1/bots/"+name)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusBadGateway, "peer_unreachable", err.Error())
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
// status and the body, read up to 256 KiB.
func (s *Service) getSigned(ctx context.Context, domain, path string) (int, []byte, error) {
	req, err := s.fetch.newRequest(ctx, http.MethodGet, domain, path, nil)
	if err != nil {
		return 0, nil, err
	}
	if err := s.id.SignRequest(req, domain, nil); err != nil {
		return 0, nil, err
	}
	return s.fetch.send(req, s.fetch.isAllowed(domain), maxBotsBody)
}

// cachedBots returns the cached bots of peers, by domain and handle.
func (s *Service) cachedBots(ctx context.Context) ([]entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, profile FROM remote_bots ORDER BY domain, handle`)
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
		out = append(out, entry{Profile: p, Address: p.Handle + "@" + domain, Remote: true})
	}
	return out, rows.Err()
}

// refreshSoon asks Run to refresh domain's directory next. With Run
// busy or not running the request is dropped; the 15-minute refresh
// catches up.
func (s *Service) refreshSoon(domain string) {
	select {
	case s.refresh <- domain:
	default:
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

func (s *Service) refreshLogged(ctx context.Context, domain string) {
	if err := s.refreshDirectory(ctx, domain); err != nil && ctx.Err() == nil {
		s.logger.Warn("federation directory refresh failed", "domain", domain, "err", err)
	}
}

// refreshDirectory reads up to 10 pages of domain's directory and
// replaces its cached bots with them, cleaned. Entries with an invalid
// handle, one naming a server included, are dropped. On an error the
// old rows stay. A peer no longer allowed when the rows are written
// gets none.
func (s *Service) refreshDirectory(ctx context.Context, domain string) error {
	var got []bots.Profile
	for page := 1; page <= directoryPages; page++ {
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
		for _, p := range out.Bots {
			if p, ok := bots.CleanRemote(p); ok {
				got = append(got, p)
			}
		}
		if out.Next == nil {
			break
		}
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
