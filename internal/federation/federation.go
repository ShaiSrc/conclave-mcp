// Package federation lets Conclave servers peer: each has an identity
// (a domain and an Ed25519 key) and serves it on a separate federation
// listener. See docs/superpowers/specs/2026-11-13-federation-foundation-design.md.
package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/bots"
)

// Options configure a Service.
type Options struct {
	Domain       string // this server's federation domain
	Mode         string // allowlist or open
	InsecureHTTP bool   // contact peers over http:// instead of https://
	Logger       *slog.Logger
}

// Service is the federation side of a server. It holds the database,
// the server's identity (and with it the domain), the federation
// settings, the peer store, the outgoing client and the local bots.
type Service struct {
	db           *sql.DB
	id           *Identity
	mode         string
	insecureHTTP bool
	logger       *slog.Logger
	peers        *Peers
	fetch        *Fetcher
	lookups      *rate.Limiter // operator lookups and adds
	botStore     *bots.Store
	refresh      chan string   // peers whose directory the refresh loop fetches next
	deliver      chan struct{} // wakes the delivery worker
	applyMu      sync.Mutex    // one batch of replica events at a time
	runMu        sync.Mutex
	running      map[string]bool   // peers with a delivery run going
	rotation     map[string]string // per peer, the last room id its last run attempted
	runs         sync.WaitGroup    // delivery runs
	slots        chan struct{}     // of delivery runs, maxPeerRuns
	now          func() time.Time
	jitter       func() float64 // in [0, 1)
	budget       time.Duration  // per peer and pass, of delivery and of catch-up

	// Rooms connects federated rooms to the local services.
	Rooms RoomHooks

	mu           sync.Mutex
	limiters     map[string]*rate.Limiter // per verified peer
	dmLimits     map[string]*rate.Limiter // direct invites, per peer
	followLimits map[string]*rate.Limiter // follow requests, per peer
	keyLimiters  map[int64]*rate.Limiter  // remote lookups, per key

	warnLog, keyChecks minuteGate
}

// New returns the federation service for opts.Domain, loading the
// server's key from db or generating it on first use.
func New(db *sql.DB, opts Options) (*Service, error) {
	domain, err := NormalizeDomain(opts.Domain)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, opts.Domain)
	}
	if err := checkKeyHandles(context.Background(), db); err != nil {
		return nil, err
	}
	id, err := LoadOrCreateIdentity(context.Background(), db, domain)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{db: db, id: id, mode: opts.Mode, insecureHTTP: opts.InsecureHTTP, logger: logger,
		peers: &Peers{db: db}, limiters: map[string]*rate.Limiter{}, keyLimiters: map[int64]*rate.Limiter{},
		botStore: bots.NewStore(db), refresh: make(chan string, 16), deliver: make(chan struct{}, 1),
		running: map[string]bool{}, rotation: map[string]string{}, slots: make(chan struct{}, maxPeerRuns),
		now: time.Now, jitter: rand.Float64, budget: peerBudget,
		lookups: rate.NewLimiter(rate.Every(time.Minute/lookupsPerMinute), lookupBurst)}
	s.fetch = NewFetcher(opts.InsecureHTTP, func(domain string) bool {
		p, err := s.peers.Get(context.Background(), domain)
		return err == nil && p.Status == statusAllowed
	})
	return s, nil
}

// keyHandleRE matches the handles that are addresses of keys without a
// handle (key-<id>@), reserved since federated rooms.
var keyHandleRE = regexp.MustCompile(`^key-[0-9]+$`)

// checkKeyHandles refuses federation while a key that is not revoked has
// a handle key-<digits>, which could be set before such handles were
// reserved: its address could be another key's. Renaming is the
// operator's decision, so the error names the keys and the command.
func checkKeyHandles(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT id, handle FROM api_keys WHERE revoked_at IS NULL AND handle LIKE 'key-%' ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id int64
		var handle string
		if err := rows.Scan(&id, &handle); err != nil {
			return err
		}
		if keyHandleRE.MatchString(handle) {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
	}
	if err := rows.Err(); err != nil || ids == nil {
		return err
	}
	return fmt.Errorf("handles of the form key-<digits> name the addresses of keys without a handle (key ids %s); "+
		"rename these handles with: conclave-mcp bots handle <key-id> <new-handle>", strings.Join(ids, ", "))
}

// PublicRoutes mounts the routes of the federation listener, on a
// router without routes yet. Every response, the router's own 404 and
// 405 included, carries the public headers; every /fed/v1 route is
// behind Verify.
func (s *Service) PublicRoutes(r chi.Router) {
	r.Use(publicHeaders)
	s.publicRoutes(r)
	r.Get("/.well-known/conclave", s.handleDocument)
	r.Route("/fed/v1", func(r chi.Router) {
		r.Use(s.Verify)
		r.Post("/ping", s.handlePing)
		r.Get("/bots/{name}", s.handleFedBot)
		r.Get("/directory", s.handleFedDirectory)
		r.Post("/rooms/{id}/messages", s.handleRoomMessage)
		r.Get("/rooms/{id}/events", s.handleRoomEvents)
		r.Post("/rooms/{id}/invite", s.handleRoomInvite)
		r.Post("/rooms/{id}/events", s.handleRoomPush)
		r.Post("/rooms/{id}/leave", s.handleRoomLeave)
		r.Get("/rooms/{id}/claims", s.handleRoomClaims)
		r.Post("/rooms/{id}/claims", s.handleRoomClaims)
		r.Post("/rooms/{id}/claims/heartbeat", s.handleRoomClaims)
		r.Post("/rooms/{id}/claims/release", s.handleRoomClaims)
		r.Post("/follows", s.handleFollow)
		r.Post("/follows/withdraw", s.handleFollowWithdraw)
	})
}

func (s *Service) handleDocument(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.id.Document())
}
