// Middleware for the Conclave MCP server.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/google/uuid"
)

// Error envelope from SPEC-0003 §5.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// requestIDKey is the context key under which the per-request UUID
// is stored. Set by RequestIDMiddleware, read by error writers and
// downstream handlers.
type requestIDKey struct{}

// RequestIDMiddleware ensures every request has a request id. If
// the incoming X-Request-Id header is set and looks like a UUID,
// it is preserved; otherwise a new uuidv4 is generated. The id
// flows through the request context and is echoed in the response
// header.
func RequestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if !looksLikeUUID(id) {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RecoveryMiddleware turns a panic into a 500 with the standard
// error envelope. The stack is logged but never sent to the client
// (SPEC-0003 §5: server bugs never leak the stack).
func RecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic in handler",
						"err", rec,
						"path", r.URL.Path,
						"method", r.Method,
						"stack", string(debug.Stack()),
					)
					writeError(w, r, logger, http.StatusInternalServerError,
						"internal_error", "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger,
	status int, code, msg string) {
	rid := RequestIDFromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   msg,
		RequestID: rid,
	}})
	logger.Warn("request error",
		"path", r.URL.Path,
		"method", r.Method,
		"status", status,
		"code", code,
		"request_id", rid,
	)
}

// RequestIDFromContext returns the request id stored on ctx, or "".
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

func looksLikeUUID(s string) bool {
	// Quick sanity: 36 chars with hyphens at 8/13/18/23.
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c rune) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f':
		return true
	case c >= 'A' && c <= 'F':
		return true
	}
	return false
}
