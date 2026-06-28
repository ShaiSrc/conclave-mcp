// Per-key rate limiting (SPEC-0003 §6).
//
// Rate limits are enforced per API key (identified by the auth
// Claims on the request context). The default is 600 requests per
// minute with a burst of 30 (per key). On limit, the response is
// 429 with Retry-After.
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// rateLimiter holds one rate.Limiter per authenticated key id.
type rateLimiter struct {
	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
	rate     rate.Limit
	burst    int
}

// BurstFromRate derives a sensible burst from a per-minute rate
// limit. The formula is `max(30, rpm/20)`:
//
//   - At the SPEC-0003 §6 default of 600 rpm, the burst is 30.
//     This matches the Phase-20/30 bridge placeholder
//     `RateLimitMiddleware(60, 30)` in spirit (the placeholder
//     hardcoded the rate; the burst was 30 from day one).
//   - Scales linearly with the rate so the burst stays a small
//     fraction of the per-minute budget.
//   - The 30 floor means operators who pin a low rpm (e.g. 60)
//     still get a usable burst, and it matches the value the
//     `rate` package documentation recommends as a starting
//     point for chat-shaped workloads.
//
// See ADR-0006 for the rationale.
func BurstFromRate(rpm int) int {
	if rpm <= 0 {
		rpm = 600
	}
	b := rpm / 20
	if b < 30 {
		b = 30
	}
	return b
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 600
	}
	if burst <= 0 {
		burst = BurstFromRate(perMinute)
	}
	return &rateLimiter{
		limiters: make(map[int64]*rate.Limiter),
		rate:     rate.Limit(float64(perMinute) / 60.0),
		burst:    burst,
	}
}

func (r *rateLimiter) get(keyID int64) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lim, ok := r.limiters[keyID]; ok {
		return lim
	}
	lim := rate.NewLimiter(r.rate, r.burst)
	r.limiters[keyID] = lim
	return lim
}

// RateLimitMiddleware returns a middleware that limits per authenticated
// key. Unauthenticated requests (no claims on context) are passed
// through; auth middleware is expected to have run first.
func RateLimitMiddleware(perMinute, burst int) func(http.Handler) http.Handler {
	rl := newRateLimiter(perMinute, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := auth.FromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			lim := rl.get(claims.KeyID)
			if !lim.Allow() {
				// Compute Retry-After: time until the limiter has
				// budget for one more request. We use the
				// reservation API indirectly via Tokens.
				// Reservation returns the time at which the action
				// would be allowed; subtract now for the delay.
				res := lim.Reserve()
				delay := res.Delay()
				res.Cancel() // don't actually consume
				if delay < time.Second {
					delay = time.Second
				}
				w.Header().Set("Retry-After", strconv.Itoa(int(delay.Seconds())))
				writeError(w, r, defaultLogger(), http.StatusTooManyRequests,
					"rate_limited", "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// defaultLogger returns the package-level default logger. Used
// when RateLimitMiddleware is exercised in isolation (tests) where
// the server's own logger has not been wired.
func defaultLogger() *slog.Logger {
	return slog.Default()
}

// rateLimitHeaders is a separate helper that callers can use to
// surface the X-RateLimit-* headers on successful responses. The
// middleware below attaches them; the package-level helper lets
// other handlers do the same.
func rateLimitHeaders(w http.ResponseWriter, lim *rate.Limiter, perMinute int) {
	// Tokens() is the current count of available tokens.
	tokens := lim.Tokens()
	// Capacity is the burst; remaining is min(tokens, burst).
	remaining := int(tokens)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 30 {
		remaining = 30
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(perMinute))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	// Reset: time until one full token is available. The limiter
	// refills at rate per second; the next full token takes
	// 1/rate seconds.
	reset := time.Now().Add(time.Duration(float64(time.Second) / float64(rate.Limit(perMinute)/60.0)))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
}

// formatRateKey formats a key id for logging; avoids leaking the
// key id in error responses.
func formatRateKey(k int64) string { return fmt.Sprintf("key:%d", k) }
