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
//     without read; admin keys pass), and checks read again every
//     Recheck (5s): the stream ends when it is gone.
//   - Holds at most MaxStreams (16) open streams per key, counting
//     the ones MCP messages.subscribe opens; one more answers 429.
//   - Drops a client that does not read: each write has a deadline of
//     WriteTimeout (10s).
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
	"sync"
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
	DB           DB
	Broker       Subscriber
	Heartbeat    time.Duration // 30s default
	MaxWait      time.Duration // 300s default
	Recheck      time.Duration // how often an open stream checks read again; 5s default
	WriteTimeout time.Duration // a client that takes longer to read one event is dropped; 10s default
	MaxStreams   int           // open streams per key; 16 default

	mu   sync.Mutex
	open map[int64]int // open streams per key id
}

// NewHandler returns a Handler with default timings.
func NewHandler(db DB, broker Subscriber) *Handler {
	return &Handler{
		DB:           db,
		Broker:       broker,
		Heartbeat:    30 * time.Second,
		MaxWait:      300 * time.Second,
		Recheck:      5 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxStreams:   16,
		open:         map[int64]int{},
	}
}

// acquire takes one of key's stream slots; release gives it back.
func (h *Handler) acquire(key int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[key] >= h.MaxStreams {
		return false
	}
	h.open[key]++
	return true
}

func (h *Handler) release(key int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[key]--; h.open[key] <= 0 {
		delete(h.open, key)
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
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "events broker not configured")
		return
	}
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	roomID := chi.URLParam(r, "id")

	// 404 to non-members, 403 missing_right without read.
	mem, ok := rights.Require(w, r, h.DB, roomID, rights.Read)
	if !ok {
		return
	}
	if mem.Archived {
		writeErr(w, http.StatusGone, "not_found", "room is archived")
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

	if _, ok := w.(http.Flusher); !ok {
		writeErr(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}
	if !h.acquire(caller) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("too many open event streams for this key (at most %d)", h.MaxStreams))
		return
	}
	defer h.release(caller)
	// Each write gets a deadline, so a client that does not read is
	// dropped. Cleared on return: the connection may serve another request.
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	send := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(h.WriteTimeout)) // ErrNotSupported only off a real connection
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// SSE headers. We force no-cache and no buffering so
	// reverse proxies (nginx with proxy_buffering off,
	// Caddy with flush_interval -1) actually flush each
	// event. Runbook §4 already documents this.
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
	if !send(": ok\n\n") {
		return
	}

	heartbeat := time.NewTicker(h.Heartbeat)
	defer heartbeat.Stop()
	// ponytail: read is checked on a timer, not per event, so up to
	// Recheck of messages can still reach a key after read is taken.
	// Upgrade path: cancel subscriptions from the rights/remove handlers.
	recheck := time.NewTicker(h.Recheck)
	defer recheck.Stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-recheck.C:
			if cur, ok, err := rights.Lookup(r.Context(), h.DB, roomID); err != nil || !ok || !cur.Rights.Has(rights.Read) {
				return
			}
		case ev, open := <-sub:
			if !open || !send("event: message.created\ndata: %s\n\n", ev.Payload) {
				return
			}
		case <-heartbeat.C:
			if !send("event: ping\ndata: {\"ts\":%q}\n\n", time.Now().UTC().Format(time.RFC3339Nano)) {
				return
			}
		}
	}
}

// writeErr writes the SPEC-0003 §5 error envelope with the request id
// the server's middleware put on the response.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": w.Header().Get("X-Request-Id"),
	}})
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
