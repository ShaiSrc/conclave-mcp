// Package events — HTTP handler for GET /v1/rooms/{id}/events.
//
// Implements the SSE endpoint from SPEC-0003 §2.2 + ADR-0009.
//
// Wire format (text/event-stream):
//
//	event: message.created
//	id: <ts>
//	data: {"id":...,"room_id":"...","ts":"...","content":"..."}
//
//	event: ping
//	data: {"ts":"..."}
//
// The handler:
//
//   - Authenticates the caller and requires the read right (404 to
//     non-members, as the rest of /v1/rooms/{id}*; 403 missing_right
//     without read; admin keys pass), and checks read again before
//     each delivered message.
//   - Streams new messages as they arrive. A subscriber that
//     reconnects with ?since=<ts> starts at the live
//     boundary; the broker does not replay old events (use
//     GET /v1/rooms/{id}/messages?since=<ts> for that).
//   - Emits a heartbeat every 30s to keep the connection
//     alive through reverse proxies.
//   - Closes the connection after `wait` seconds (default
//     60, max 300). Clients that want a long-lived
//     connection just reconnect.
//
// The handler returns 503 if the broker is not configured
// (Deps.Broker is nil). The rest of the server keeps
// working in that mode.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// Subscriber is the subset of *Broker the handler needs.
type Subscriber interface {
	Subscribe(roomID string) (<-chan Event, func())
}

// DB is the storage interface the handler uses for the
// membership check. *sql.DB satisfies it.
type DB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Handler returns the http.Handler for the SSE endpoint.
// The handler is mounted at GET /v1/rooms/{id}/events by
// the server composition root.
type Handler struct {
	DB        DB
	Broker    Subscriber
	Heartbeat time.Duration // 30s default
	MaxWait   time.Duration // 300s default
}

// NewHandler returns a Handler with default timings.
func NewHandler(db DB, broker Subscriber) *Handler {
	return &Handler{
		DB:        db,
		Broker:    broker,
		Heartbeat: 30 * time.Second,
		MaxWait:   300 * time.Second,
	}
}

// Handle is the HTTP handler.
//
// Query parameters:
//
//	wait=<seconds>   how long to keep the connection open
//	                  before closing (default 60, max 300)
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if h.Broker == nil {
		http.Error(w, "events broker not configured", http.StatusServiceUnavailable)
		return
	}
	caller := callerKeyID(r)
	if caller == 0 {
		http.Error(w, `{"error":{"code":"unauthenticated","message":"no auth claims"}}`,
			http.StatusUnauthorized)
		return
	}
	roomID := chi.URLParam(r, "id")

	// 404 to non-members, 403 missing_right without read.
	mem, ok := rights.Require(w, r, h.DB, roomID, rights.Read)
	if !ok {
		return
	}
	if mem.Archived {
		http.Error(w, `{"error":{"code":"not_found","message":"room is archived"}}`,
			http.StatusGone)
		return
	}

	wait := 60 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
			if wait > h.MaxWait {
				wait = h.MaxWait
			}
		}
	}

	// SSE headers. We force no-cache and no buffering so
	// reverse proxies (nginx with proxy_buffering off,
	// Caddy with flush_interval -1) actually flush each
	// event. Runbook §4 already documents this.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE the preamble: ": ok" tells the client the
	// subscription is live, so a client that reads the backlog
	// after seeing it cannot miss a message (messages.subscribe
	// relies on this). It also forces headers through proxies.
	sub, cancel := h.Broker.Subscribe(roomID)
	defer cancel()
	_, _ = fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(h.Heartbeat)
	defer heartbeat.Stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case ev, open := <-sub:
			if !open {
				return
			}
			// ponytail: one indexed query per delivered event; fine at local scale.
			// Upgrade path: cancel subscriptions from the rights/remove handlers.
			if cur, ok, err := rights.Lookup(r.Context(), h.DB, roomID); err != nil || !ok || !cur.Rights.Has(rights.Read) {
				return
			}
			_, _ = fmt.Fprintf(w, "event: message.created\ndata: %s\n\n", ev.Payload)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, "event: ping\ndata: {\"ts\":%q}\n\n",
				time.Now().UTC().Format(time.RFC3339Nano))
			flusher.Flush()
		}
	}
}

// callerKeyID returns the authenticated caller's key id from
// the request context. Returns 0 if no claims are present.
func callerKeyID(r *http.Request) int64 {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		return 0
	}
	return c.KeyID
}

// jsonEvent is a tiny helper for tests that want to assert
// on the wire shape. Production code uses raw bytes.
func jsonEvent(payload any) []byte {
	b, _ := json.Marshal(payload)
	return b
}
