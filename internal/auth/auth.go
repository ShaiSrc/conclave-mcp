// Package auth provides bearer-token authentication for the
// Conclave MCP HTTP control plane (SPEC-0003 §3, ADR-0003).
//
// Token shape (SPEC-0003 §3):
//
//	"tk_" + 32 random bytes, base64url-encoded (43 chars after prefix).
//
// The token is sent in the Authorization header as
//
//	"Bearer <token>".
//
// On the server, only an argon2id PHC string is stored. The
// incoming bearer token is hashed with the parameters from the PHC
// and compared in constant time. This package does NOT do the
// hashing for new keys (see cmd/keys or task #20); it only verifies.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters per ADR-0003 (OWASP 2024 minimums):
// m=19 MiB, t=2, p=1. Encoded as constants so all callers (verifier
// and CLI creator) agree on a single source of truth.
const (
	ArgonMemoryKiB = 19 * 1024 // 19 MiB
	ArgonTime      = 2
	ArgonThreads   = 1
	SaltBytes      = 16
)

// KeyLookup is the minimal interface the verifier needs from the
// storage layer: given a token prefix, return the key's id, scopes,
// and stored argon2id PHC string, or ErrNotFound.
type KeyLookup interface {
	LookupByPrefix(ctx context.Context, prefix string) (keyID int64, scopes []string, phc string, err error)
}

// ErrNotFound is returned by KeyLookup when no key matches the prefix.
var ErrNotFound = errors.New("key not found")

// Verifier holds the shared state for token verification.
type Verifier struct {
	lookup KeyLookup
	// sem caps concurrent argon2 verifications (each costs ~38 MiB + CPU).
	sem chan struct{}
}

// NewVerifier returns a Verifier with the lookup interface and the
// concurrent-verify cap (per SPEC-0003 §6: default 32).
func NewVerifier(lookup KeyLookup, maxConcurrent int) *Verifier {
	if maxConcurrent <= 0 {
		maxConcurrent = 32
	}
	return &Verifier{
		lookup: lookup,
		sem:    make(chan struct{}, maxConcurrent),
	}
}

// Verify checks a bearer token. Returns the key id and scopes on
// success. Returns ErrInvalid on any mismatch, missing header, or
// malformed token — callers should NOT distinguish among these to
// the client.
func (v *Verifier) Verify(ctx context.Context, authzHeader string) (keyID int64, scopes []string, err error) {
	if authzHeader == "" {
		return 0, nil, ErrInvalid
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authzHeader, prefix) {
		return 0, nil, ErrInvalid
	}
	token := strings.TrimSpace(authzHeader[len(prefix):])
	if !strings.HasPrefix(token, "tk_") {
		return 0, nil, ErrInvalid
	}
	// Token shape: "tk_" + base64url(32 random bytes) = 3 + 43 = 46 chars
	if len(token) != 46 {
		return 0, nil, ErrInvalid
	}

	// Use the 8-char prefix (after "tk_") as the lookup key. This
	// is what the keys service stores in api_keys.prefix per
	// migration 0002.
	const prefixLen = 8
	lookupKey := token[3 : 3+prefixLen] // skip "tk_", take 8 chars

	// Cap concurrent argon2 verifications.
	select {
	case v.sem <- struct{}{}:
		defer func() { <-v.sem }()
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}

	keyID, scopes, phc, err := v.lookup.LookupByPrefix(ctx, lookupKey)
	if err != nil {
		// Any lookup failure (including not-found) is a generic
		// "invalid" to the client. The internal error is logged
		// by the middleware, not surfaced.
		return 0, nil, ErrInvalid
	}

	ok, verifyErr := VerifyPHC(token, phc)
	if verifyErr != nil || !ok {
		return 0, nil, ErrInvalid
	}
	return keyID, scopes, nil
}

// ErrInvalid is the generic "token is wrong" error returned to
// callers. Internal errors (DB down, etc.) are logged but produce
// the same outcome to the client.
var ErrInvalid = errors.New("invalid bearer token")

// GenerateToken returns a fresh "tk_..." bearer token and its
// argon2id PHC string. The plaintext token is returned to the
// caller exactly once; the PHC is what gets stored.
//
// tokens are 32 random bytes, base64url-encoded, no padding.
func GenerateToken() (plaintext, phc string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	tok := "tk_" + base64.RawURLEncoding.EncodeToString(raw[:])

	salt := make([]byte, SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", "", fmt.Errorf("rand salt: %w", err)
	}
	phcStr := argon2.IDKey([]byte(tok), salt, ArgonTime, ArgonMemoryKiB, ArgonThreads, 32)
	// We return the raw hash as hex for storage. The verifier does
	// not parse PHC format strings — it just compares against the
	// stored hash with the same parameters. Storing the params
	// alongside is the caller's responsibility (or we encode them
	// into a PHC string here).
	phc = fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		ArgonMemoryKiB, ArgonTime, ArgonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(phcStr),
	)
	return tok, phc, nil
}

// VerifyPHC recomputes the argon2id hash of plaintext and
// constant-time compares against the PHC string. Returns false
// (not an error) on a mismatch, error on parse failure.
func VerifyPHC(plaintext, phc string) (bool, error) {
	// Parse the PHC we generate: $argon2id$v=19$m=N,t=N,p=N$salt$hash
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("invalid PHC format")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("parse params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}
	got := argon2.IDKey([]byte(plaintext), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// claimsKey is the context key under which authenticated key
// claims are stored.
type ClaimsKey struct{}

// Claims carries the authenticated caller's identity through the
// request context. Set by Middleware, read by handlers.
type Claims struct {
	KeyID  int64
	Scopes []string
}

// Middleware returns an http.Handler middleware that enforces
// bearer auth on every request. On failure it writes the
// SPEC-0003 §5 error envelope and aborts the request.
func (v *Verifier) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Look up the request id from the existing context.
			authz := r.Header.Get("Authorization")
			keyID, scopes, err := v.Verify(r.Context(), authz)
			if err != nil {
				// 401 with the standard envelope.
				writeAuthError(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), ClaimsKey{}, Claims{KeyID: keyID, Scopes: scopes})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext returns the auth claims, or zero-value+false if
// the request is not authenticated.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ClaimsKey{}).(Claims)
	return c, ok
}

// writeAuthError writes a 401 with the SPEC-0003 envelope. We import
// the writeError helper from the server package via a small bridge
// to keep this package standalone-testable.
func writeAuthError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Minimal envelope; the server middleware will normally render
	// the full envelope. We keep this here so auth is testable
	// without importing internal/server.
	_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"missing or invalid Authorization header","request_id":""}}`))
}

// unused but reserved for future use; see #25.
var _ = sync.Mutex{}
var _ = time.Now
