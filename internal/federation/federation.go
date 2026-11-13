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
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"
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
// settings, the peer store and the outgoing client.
type Service struct {
	db           *sql.DB
	id           *Identity
	mode         string
	insecureHTTP bool
	logger       *slog.Logger
	peers        *Peers
	fetch        *Fetcher
	lookups      *rate.Limiter // operator lookups and adds

	mu       sync.Mutex
	limiters map[string]*rate.Limiter // per verified peer

	warnLog, keyChecks minuteGate
}

// New returns the federation service for opts.Domain, loading the
// server's key from db or generating it on first use.
func New(db *sql.DB, opts Options) (*Service, error) {
	domain, err := NormalizeDomain(opts.Domain)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", err, opts.Domain)
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
		peers: &Peers{db: db}, limiters: map[string]*rate.Limiter{},
		lookups: rate.NewLimiter(rate.Every(time.Minute/lookupsPerMinute), lookupBurst)}
	s.fetch = NewFetcher(opts.InsecureHTTP, func(domain string) bool {
		p, err := s.peers.Get(context.Background(), domain)
		return err == nil && p.Status == statusAllowed
	})
	return s, nil
}

// PublicRoutes mounts the routes of the federation listener. Every
// /fed/v1 route is behind Verify.
func (s *Service) PublicRoutes(r chi.Router) {
	r.Get("/.well-known/conclave", s.handleDocument)
	r.Route("/fed/v1", func(r chi.Router) {
		r.Use(s.Verify)
		r.Post("/ping", s.handlePing)
	})
}

func (s *Service) handleDocument(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.id.Document())
}
