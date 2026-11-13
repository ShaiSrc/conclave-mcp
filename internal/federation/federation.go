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

	"github.com/go-chi/chi/v5"
)

// Options configure a Service.
type Options struct {
	Domain       string // this server's federation domain
	Mode         string // allowlist or open
	InsecureHTTP bool   // contact peers over http:// instead of https://
	Logger       *slog.Logger
}

// Service is the federation side of a server. It holds the database,
// the server's identity (and with it the domain) and the federation
// settings.
type Service struct {
	db           *sql.DB
	id           *Identity
	mode         string
	insecureHTTP bool
	logger       *slog.Logger
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
	return &Service{db: db, id: id, mode: opts.Mode, insecureHTTP: opts.InsecureHTTP, logger: logger}, nil
}

// PublicRoutes mounts the routes of the federation listener.
func (s *Service) PublicRoutes(r chi.Router) {
	r.Get("/.well-known/conclave", s.handleDocument)
}

func (s *Service) handleDocument(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.id.Document())
}
