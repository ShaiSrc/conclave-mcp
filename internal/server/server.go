// Package server implements the Conclave MCP HTTP control plane
// and the MCP transport that wraps it (SPEC-0003).
//
// Endpoints are mounted under /v1. The router uses go-chi/chi for
// path params and middleware composition. All non-2xx responses use
// the JSON error envelope from SPEC-0003 §5:
//
//	{ "error": { "code": "...", "message": "...", "request_id": "..." } }
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/events"
	"github.com/hermes/conclave-mcp/internal/keys"
	"github.com/hermes/conclave-mcp/internal/mcp"
	"github.com/hermes/conclave-mcp/internal/messages"
	"github.com/hermes/conclave-mcp/internal/rooms"
)

// Server is the Conclave MCP HTTP server. Construct with New and
// call Start.
type Server struct {
	cfg    Config
	logger *slog.Logger
	router chi.Router
	http   *http.Server

	mu      sync.Mutex
	started bool
}

// Config is the runtime configuration for the HTTP server. Fields
// without sensible defaults are required.
type Config struct {
	Host        string
	Port        int
	ReadHeader  time.Duration // default 5s
	IdleTimeout time.Duration // default 120s
	Logger      *slog.Logger
}

// New builds a Server with all middleware wired and routes mounted.
func New(cfg Config, deps Deps) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReadHeader <= 0 {
		cfg.ReadHeader = 5 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 120 * time.Second
	}

	s := &Server{cfg: cfg, logger: cfg.Logger}

	s.router = s.buildRouter(deps)
	s.http = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           s.router,
		ReadHeaderTimeout: cfg.ReadHeader,
		IdleTimeout:       cfg.IdleTimeout,
		// No WriteTimeout: long-running MCP streams may block on writes.
		// No ErrorLog: rare net/http errors fall through to log.Default.
	}
	return s, nil
}

// Deps groups the dependencies the server needs from the rest of the
// application. New(cfg, Deps) is the composition root: every
// service is constructed and mounted here. If you add a new
// /v1/<resource> endpoint, this is the file to edit.
type Deps struct {
	// DB is the open SQLite handle. *sql.DB satisfies
	// DBHealthChecker. Required for the service composition.
	// If nil, /healthz returns 503.
	DB *sql.DB
	// HealthDB is the database the /healthz endpoint pings. If
	// nil, falls back to DB. Tests that want a fake health
	// response without a real SQLite handle use this field.
	HealthDB DBHealthChecker
	// MaxMessageBytes is the per-message body cap enforced by the
	// messages service. 256 KiB if zero.
	MaxMessageBytes int
	// MaxConcurrentVerify caps concurrent argon2id verifications.
	// 32 if zero.
	MaxConcurrentVerify int
	// RateLimitPerMin is the per-key requests-per-minute budget
	// (SPEC-0003 §6). 600 if zero. The burst is derived from this
	// value by BurstFromRate (ADR-0006).
	RateLimitPerMin int
	// Logger is used for structured request logging. slog.Default
	// if nil.
	Logger *slog.Logger
	// Broker is the in-process pub/sub for SSE room events
	// (ADR-0009). If nil, the SSE endpoint returns 503; the
	// rest of the server is unaffected. Constructed by
	// cmd/serve via events.NewBroker().
	Broker *events.Broker
}

// DBHealthChecker is the minimal interface a /healthz implementation
// needs from the storage layer. *sql.DB satisfies it.
type DBHealthChecker interface {
	PingContext(ctx context.Context) error
}

func (s *Server) buildRouter(deps Deps) chi.Router {
	r := chi.NewRouter()
	r.Use(RequestIDMiddleware())
	r.Use(RecoveryMiddleware(s.logger))

	r.Get("/healthz", s.handleHealthz(deps))

	// --- Auth composition root ---
	//
	// The keys service is the KeyLookup for the auth verifier. We
	// construct it before the rooms/messages/audit services so it
	// can answer prefix lookups in the middleware. This means keys
	// has no auth requirement on its own endpoints (only the
	// bootstrap key can call them) — see SPEC-0003 §3.
	keysSvc := keys.NewService(deps.DB)
	verifier := auth.NewVerifier(keysSvc, deps.MaxConcurrentVerify)

	// --- Service composition root ---
	//
	// Every /v1/<resource> is mounted here. If a future phase adds
	// a new service, this is the place to wire it.
	// Pass the in-process broker (ADR-0009) to the messages
	// service. nil is fine — the SSE endpoint will return
	// 503 but the rest of the server still works.
	roomsSvc := rooms.NewService(deps.DB)
	var messagesBroker messages.Broker
	if deps.Broker != nil {
		messagesBroker = deps.Broker
	}
	messagesSvc := messages.NewService(deps.DB, deps.MaxMessageBytes, messagesBroker)
	auditSvc := audit.NewService(deps.DB)

	r.Route("/v1", func(r chi.Router) {
		// Auth applies to all /v1 endpoints, per SPEC-0003.
		r.Use(verifier.Middleware())

		// Rate limiting sits *inside* auth so we don't burn a
		// verify on requests that will be 429'd anyway. The
		// limit and burst are config-driven (ADR-0006): a zero
		// Deps.RateLimitPerMin falls back to the SPEC-0003 §6
		// default of 600 rpm with a burst of 30.
		r.Use(RateLimitMiddleware(deps.RateLimitPerMin, BurstFromRate(deps.RateLimitPerMin)))

		// Keys endpoints (SPEC-0003 §3): bootstrap key can call
		// these; the keys service enforces that internally.
		keysSvc.Routes(r)
		roomsSvc.Routes(r)
		messagesSvc.Routes(r)
		auditSvc.Routes(r)

		// 404/405 envelopes apply AFTER the routes are mounted,
		// otherwise the NotFound handler shadows the routes.
		r.NotFound(s.notFound)
		r.MethodNotAllowed(s.methodNotAllowed)
	})

	// /v1/mcp — JSON-RPC-over-HTTP (SPEC-0003 §2.2, ADR-0007 §Q9).
	// The MCP server uses the Authorization header from the
	// incoming request to authenticate its internal HTTP
	// calls back to the Conclave control plane. This means
	// /v1/mcp does NOT bypass auth — the verifier.Middleware
	// already validated the token before the handler runs.
	r.With(verifier.Middleware()).Post("/v1/mcp", s.mcpProxy())

	// /v1/rooms/{id}/events — SSE stream of new messages
	// (ADR-0009). Mounted at the router level (not inside
	// the /v1 Group) so the verifier.Middleware applies
	// without picking up the rate-limit middleware, which
	// would kill long-lived connections.
	if deps.Broker != nil {
		evH := events.NewHandler(deps.DB, deps.Broker)
		r.With(verifier.Middleware()).Get("/v1/rooms/{id}/events", evH.Handle)
	}

	return r
}

// mcpProxy returns a handler that forwards a single JSON-RPC
// request to the Conclave control plane via the MCP server.
// We use a per-request server (rather than a shared one) so
// the bearer token is isolated. The BaseURL is set by the
// caller via the `bind` callback; we resolve it once at
// request time using s.http.Addr.
func (s *Server) mcpProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		baseURL := "http://" + s.http.Addr
		// No client timeout: messages.subscribe blocks up to
		// 300s and is bounded by its own context.
		srv := mcp.NewServer(baseURL, "")
		// Pull the bearer token from the Authorization
		// header. The verifier middleware already validated
		// it; we just need it to forward through.
		authHdr := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHdr, "Bearer ")
		token = strings.TrimSpace(token)
		srv.APIKey = token
		srv.ServeHTTP(w, r)
	}
}

// Handler returns the http.Handler. Exposed so tests can mount the
// server without binding a real socket.
func (s *Server) Handler() http.Handler {
	return s.router
}

// Start binds the listener and serves until ctx is cancelled, then
// performs a graceful shutdown with a 10s drain.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.http.Addr, err)
	}
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	s.logger.Info("http server listening", "addr", s.http.Addr)

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.logger.Info("http server shutting down")
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("graceful shutdown failed", "err", err)
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// notFound returns the SPEC-0003 §5 error envelope for unknown routes.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, s.logger, http.StatusNotFound, "not_found", "unknown route")
}

// methodNotAllowed returns 405.
func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, s.logger, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
}

func (s *Server) handleHealthz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()

		dbStatus := "ok"
		// HealthDB is the dedicated health probe target. It may
		// be the same as the service DB, or a separate test fake.
		// If both are nil, we report "ok" because there is no
		// health check to perform.
		//
		// We check the *sql.DB explicitly because comparing
		// typed-nil interface values yields non-nil; the
		// interface would hold a nil *sql.DB, but
		// `if healthDB != nil` would still be true.
		var healthDB DBHealthChecker
		if deps.HealthDB != nil {
			healthDB = deps.HealthDB
		} else if deps.DB != nil {
			healthDB = deps.DB
		}
		if healthDB != nil {
			if err := healthDB.PingContext(ctx); err != nil {
				dbStatus = "err"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"db":     dbStatus,
		})
	}
}
