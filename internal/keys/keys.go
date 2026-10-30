// Package keys implements the API-key endpoints from SPEC-0003 §4.
//
// Endpoints:
//
//	POST   /v1/keys       -- create; returns plaintext once
//	GET    /v1/keys       -- list (id, label, prefix, created_at,
//	                         last_used_at, revoked_at)
//	DELETE /v1/keys/{id}  -- soft-revoke (set revoked_at), admin only
//	GET    /v1/whoami     -- the caller's own key
//
// The plaintext key is returned EXACTLY ONCE in the POST response.
// Subsequent reads return only the prefix. Hash and salt are never
// returned over the API.
package keys

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
)

// Service is the keys HTTP handler.
type Service struct {
	DB *sql.DB
}

// NewService returns a Service. DB is required. The returned
// Service also satisfies auth.KeyLookup so it can be passed to
// auth.NewVerifier directly.
func NewService(db *sql.DB) *Service {
	return &Service{DB: db}
}

// ScopeAdmin is the only scope so far: admin keys may create and
// revoke keys (migration 0005).
const ScopeAdmin = "admin"

// LookupByPrefix implements auth.KeyLookup. It returns the key id,
// scopes ([ScopeAdmin] for admin keys, nil otherwise), and the
// stored argon2id PHC string. The hash and salt are never returned
// to the caller of the HTTP API; this method is for the in-process
// verifier only.
func (s *Service) LookupByPrefix(ctx context.Context, prefix string) (int64, []string, string, error) {
	var (
		id      int64
		phc     string
		isAdmin bool
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, hash, is_admin FROM api_keys WHERE prefix = ? AND revoked_at IS NULL`,
		prefix,
	).Scan(&id, &phc, &isAdmin)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, "", auth.ErrNotFound
		}
		return 0, nil, "", err
	}
	if isAdmin {
		return id, []string{ScopeAdmin}, phc, nil
	}
	return id, nil, phc, nil
}

// requireAdmin writes 403 and returns false unless the caller
// holds ScopeAdmin.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if c, ok := auth.FromContext(r.Context()); ok && slices.Contains(c.Scopes, ScopeAdmin) {
		return true
	}
	writeJSONError(w, r, http.StatusForbidden, "forbidden", "admin key required")
	return false
}

// Routes mounts the keys endpoints on r.
func (s *Service) Routes(r chi.Router) {
	r.Post("/keys", s.create)
	r.Get("/keys", s.list)
	r.Delete("/keys/{id}", s.revoke)
	r.Get("/whoami", s.whoami)
}

// whoami returns the caller's own key: {id, label, trust_origin,
// admin}. Agents use it to skip their own messages.
func (s *Service) whoami(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.FromContext(r.Context())
	var label, trust string
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT label, trust_origin FROM api_keys WHERE id = ?`, c.KeyID,
	).Scan(&label, &trust)
	if err != nil {
		writeJSONError(w, r, http.StatusNotFound, "not_found", "key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": c.KeyID, "label": label, "trust_origin": trust,
		"admin": slices.Contains(c.Scopes, ScopeAdmin),
	})
}

// TrustOrigin values (SPEC-0001 Q11, ADR-0008 §1).
const (
	TrustOriginInternal = "internal"
	TrustOriginExternal = "external"
)

// AllowedTrustOrigins is the closed set for the trust_origin
// column. Used by the create handler to validate input before
// the SQL CHECK constraint fires (so we return 400, not 500).
var AllowedTrustOrigins = map[string]bool{
	TrustOriginInternal: true,
	TrustOriginExternal: true,
}

type keyView struct {
	ID          int64  `json:"id"`
	Label       string `json:"label"`
	Prefix      string `json:"prefix"`
	CreatedAt   string `json:"created_at"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	RevokedAt   string `json:"revoked_at,omitempty"`
	TrustOrigin string `json:"trust_origin"`
}

// createKeyResponse is what POST /v1/keys returns. Note: "key" is
// the plaintext, returned exactly once.
type createKeyResponse struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	Label       string `json:"label"`
	TrustOrigin string `json:"trust_origin"`
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Label       string `json:"label"`
		TrustOrigin string `json:"trust_origin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if req.Label == "" || len(req.Label) > 128 {
		writeJSONError(w, r, http.StatusBadRequest, "bad_request", "label must be 1..128 chars")
		return
	}
	// Default trust_origin to 'internal' if missing; reject
	// anything outside the closed set before the SQL CHECK
	// fires (so we return 400, not 500). SPEC-0001 Q11.
	if req.TrustOrigin == "" {
		req.TrustOrigin = TrustOriginInternal
	}
	if !AllowedTrustOrigins[req.TrustOrigin] {
		writeJSONError(w, r, http.StatusBadRequest, "bad_request",
			"trust_origin must be 'internal' or 'external'")
		return
	}

	// Generate token + PHC.
	plaintext, phc, err := auth.GenerateToken()
	if err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, "internal_error", "key generation failed")
		return
	}
	// 8-char lookup prefix derived from the plaintext token (after
	// the "tk_" scheme prefix). Indexed in migration 0002.
	prefix := plaintext[3:11]

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(r.Context(),
		`INSERT INTO api_keys (label, hash, prefix, created_at, revoked_at, last_used_at, trust_origin)
		 VALUES (?, ?, ?, ?, NULL, NULL, ?)`,
		req.Label, phc, prefix, now, req.TrustOrigin,
	)
	if err != nil {
		// UNIQUE constraint on label or prefix -> 409
		if isUniqueViolation(err) {
			writeJSONError(w, r, http.StatusConflict, "conflict", "label or prefix already exists")
			return
		}
		writeJSONError(w, r, http.StatusInternalServerError, "internal_error", "insert failed")
		return
	}
	id, _ := res.LastInsertId()

	// The keys endpoint is callable by the bootstrap key
	// (server start) or by an existing owner (SPEC-0003
	// §3.3). The actor is the caller; the audit row records
	// the new key's id as the target. We do not include the
	// plaintext in the audit row (SPEC-0001 C7/C9). The
	// trust_origin is recorded in the detail JSON so the
	// audit log can later answer "who is external".
	caller := callerKeyID(r)
	_ = audit.Write(r.Context(), s.DB, caller, "key.create", "key", strconv.FormatInt(id, 10), "", "ok",
		`{"label":`+quoteJSON(req.Label)+`,"trust_origin":`+quoteJSON(req.TrustOrigin)+`}`)

	writeJSON(w, http.StatusCreated, createKeyResponse{
		ID:          id,
		Key:         plaintext,
		Label:       req.Label,
		TrustOrigin: req.TrustOrigin,
	})
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT id, label, prefix, created_at, last_used_at, revoked_at, trust_origin
		 FROM api_keys
		 ORDER BY id`)
	if err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, "internal_error", "list failed")
		return
	}
	defer rows.Close()

	var out []keyView
	for rows.Next() {
		var (
			k         keyView
			prefix    string
			lastUsed  sql.NullString
			revoked   sql.NullString
			trustOrig string
		)
		if err := rows.Scan(&k.ID, &k.Label, &prefix, &k.CreatedAt, &lastUsed, &revoked, &trustOrig); err != nil {
			writeJSONError(w, r, http.StatusInternalServerError, "internal_error", "scan failed")
			return
		}
		// Expose the lookup prefix (8 chars after "tk_") — enough
		// for clients to recognize their own keys, not enough to
		// forge a bearer. SPEC-0003 §3: prefix is 8 chars.
		k.Prefix = "tk_" + prefix
		if lastUsed.Valid {
			k.LastUsedAt = lastUsed.String
		}
		if revoked.Valid {
			k.RevokedAt = revoked.String
		}
		k.TrustOrigin = trustOrig
		out = append(out, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Service) revoke(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSONError(w, r, http.StatusBadRequest, "bad_request", "id must be an integer")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE api_keys SET revoked_at = ?
		 WHERE id = ? AND revoked_at IS NULL`,
		now, id,
	)
	if err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, "internal_error", "update failed")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSONError(w, r, http.StatusNotFound, "not_found", "no active key with that id")
		return
	}
	// Audit the revocation. The actor is the caller; the
	// target is the revoked key's id. The plaintext is not
	// included in the audit row.
	caller := callerKeyID(r)
	_ = audit.Write(r.Context(), s.DB, caller, "key.revoke", "key", idStr, "", "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

// callerKeyID returns the authenticated caller's key id from
// the request context. Returns 0 if no claims are present (the
// endpoint should have already returned 401 in that case, but
// we default to 0 for the audit row's actor_key_id which the
// schema allows for system-initiated actions).
func callerKeyID(r *http.Request) int64 {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		return 0
	}
	return c.KeyID
}

// quoteJSON JSON-stringifies a Go string for the audit detail
// blob. Same helper as in internal/rooms/rooms.go and
// internal/messages/messages.go; duplicated to avoid a
// cross-package utility import.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// isUniqueViolation checks if err is a SQLite UNIQUE constraint
// failure. modernc.org/sqlite returns errors with a specific message.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// writeJSON / writeJSONError are tiny helpers that avoid an
// import cycle with internal/server. They produce a SPEC-0003 §5
// envelope on errors.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	// Reuse the server's writeError by constructing the same shape.
	// We can't import server -> keys without a cycle (server uses
	// auth but not keys directly; keys uses server's helper).
	// Solution: keep keys independent and emit the envelope inline.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    msg,
			"request_id": requestID(r),
		},
	})
}

func requestID(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}
