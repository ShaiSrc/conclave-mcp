// Package claims implements path claims for coding agents sharing a
// room (one room per repository). A claim reserves a repo-relative
// file or directory for one key until it expires; the PreToolUse
// handler of `conclave-mcp hook` refuses edits of paths another key
// holds.
//
// Endpoints (GET needs the read right, the others the claim right;
// 404 to non-members; admin keys pass without being members):
//
//	GET  /v1/rooms/{id}/claims            -- active claims
//	POST /v1/rooms/{id}/claims            -- claim paths, all-or-nothing (409 on overlap)
//	POST /v1/rooms/{id}/claims/heartbeat  -- extend the caller's claims
//	POST /v1/rooms/{id}/claims/release    -- release; an admin key may release others'
//
// In a replica room the claims are the host's: each request is
// forwarded to it and the answer Forward rebuilt from the host's
// returned.
package claims

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// TTL bounds; hooks refresh claims while the agent works.
const (
	DefaultTTL = 30 * time.Minute
	MaxTTL     = 24 * time.Hour
)

// Service is the claims HTTP handler.
type Service struct {
	DB  *sql.DB
	Now func() time.Time // tests pin the clock
	// Forward, when set, sends r, a claims request of key author in
	// replica room roomID, to the room's host and returns the host's
	// status and a body rebuilt from its answer (a 2xx or 409); claims
	// of a replica are the host's. A refusal (no handle, host
	// unreachable, any other answer of the host) is a *fedrooms.Error.
	// Set by the server with federation on.
	Forward func(r *http.Request, roomID string, author int64) (int, []byte, error)

	// mu serialises claim writes so the overlap check and the insert
	// of two requests cannot interleave.
	// ponytail: one lock for all rooms; per-room locks if claim
	// traffic ever matters.
	mu sync.Mutex
}

func NewService(db *sql.DB) *Service { return &Service{DB: db, Now: time.Now} }

func (s *Service) Routes(r chi.Router) {
	r.Get("/rooms/{id}/claims", s.list)
	r.Post("/rooms/{id}/claims", s.claim)
	r.Post("/rooms/{id}/claims/heartbeat", s.heartbeat)
	r.Post("/rooms/{id}/claims/release", s.release)
}

// Claim is one active claim as the API returns it.
type Claim struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	KeyID     int64  `json:"key_id"`
	Label     string `json:"label"`
	Auto      bool   `json:"auto"`
	Note      string `json:"note,omitempty"`
	ExpiresAt string `json:"expires_at"`
	// TrustOrigin is the holder key's origin ("internal" or "external").
	TrustOrigin string `json:"trust_origin,omitempty"`
}

var errBadPath = errors.New("paths must be relative to the repository root, without '..' (a trailing / claims a directory)")

var errControlPath = errors.New("paths must not contain control characters (newline, tab, escape, ...)")

// Normalize returns the stored form of a claim path: '/'-separated,
// cleaned and relative, with a trailing '/' for directories.
func Normalize(p string) (string, error) {
	if strings.ContainsFunc(p, unicode.IsControl) { // before TrimSpace, which would drop a trailing "\n"
		return "", errControlPath
	}
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" || strings.HasPrefix(p, "/") {
		return "", errBadPath
	}
	dir := strings.HasSuffix(p, "/")
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", errBadPath
	}
	if dir {
		c += "/"
	}
	return c, nil
}

// Overlaps reports whether two normalised paths collide: they are
// equal, or one is a directory that contains the other.
func Overlaps(a, b string) bool {
	return a == b ||
		strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) ||
		strings.HasSuffix(b, "/") && strings.HasPrefix(a, b)
}

func ttlFrom(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultTTL
	}
	return min(time.Duration(seconds)*time.Second, MaxTTL)
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// active returns the room's unexpired claims, labels included.
func active(ctx context.Context, q queryer, roomID string, now time.Time) ([]Claim, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT c.id, c.path, c.key_id, k.label, c.auto, COALESCE(c.note, ''), c.expires_at, k.trust_origin
		 FROM claims c JOIN api_keys k ON k.id = c.key_id
		 WHERE c.room_id = ? AND c.expires_at > ?
		 ORDER BY c.path, c.key_id`,
		roomID, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Claim{}
	for rows.Next() {
		var c Claim
		var exp int64
		if err := rows.Scan(&c.ID, &c.Path, &c.KeyID, &c.Label, &c.Auto, &c.Note, &exp, &c.TrustOrigin); err != nil {
			return nil, err
		}
		c.ExpiresAt = time.Unix(exp, 0).UTC().Format(time.RFC3339)
		out = append(out, c)
	}
	return out, rows.Err()
}

// access answers 404 (no room, not a member, or archived) or 403
// missing_right itself and returns ok=false. A request of a replica
// room that passes is answered by the room's host, through Forward,
// and ok is false too.
func (s *Service) access(w http.ResponseWriter, r *http.Request, roomID, need string) (rights.Member, bool) {
	mem, ok := rights.Require(w, r, s.DB, roomID, need)
	switch {
	case !ok:
	case mem.Archived:
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
	case mem.Home != "":
		s.forward(w, r, roomID, mem.KeyID)
	default:
		return mem, true
	}
	return mem, false
}

// forward answers r with the answer of the replica's host, as Forward
// rebuilt it: a 2xx or 409 with checked fields, any other answer a
// refusal with the host's status and code and its message marked.
func (s *Service) forward(w http.ResponseWriter, r *http.Request, roomID string, caller int64) {
	if s.Forward == nil {
		writeErr(w, r, http.StatusForbidden, "replica_room", "room is hosted on another server, and federation is off")
		return
	}
	status, body, err := s.Forward(r, roomID, caller)
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		writeErr(w, r, fe.Status, fe.Code, fe.Message)
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "forward claims")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	if _, ok := s.access(w, r, roomID, rights.Read); !ok {
		return
	}
	list, err := active(r.Context(), s.DB, roomID, s.Now())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list claims")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claims": list})
}

type claimRequest struct {
	Paths      []string `json:"paths"`
	TTLSeconds int      `json:"ttl_seconds"`
	Auto       bool     `json:"auto"`
	Note       string   `json:"note"`
}

func (s *Service) claim(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	mem, ok := s.access(w, r, roomID, rights.Claim)
	if !ok {
		return
	}
	caller := mem.KeyID
	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if len(req.Paths) == 0 || len(req.Note) > 1024 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "paths required; note at most 1024 chars")
		return
	}
	paths := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		n, err := Normalize(p)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		paths = append(paths, n)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, now := r.Context(), s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE room_id = ? AND expires_at <= ?`, roomID, now.Unix()); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "expire claims")
		return
	}
	held, err := active(ctx, tx, roomID, now)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list claims")
		return
	}
	mine := map[string]bool{}
	var conflicts []map[string]any
	var reasons []string
	for _, c := range held {
		if c.KeyID == caller {
			mine[c.Path] = true
		}
	}
	for _, p := range paths {
		for _, c := range held {
			if c.KeyID != caller && Overlaps(p, c.Path) {
				conflicts = append(conflicts, map[string]any{
					"path": p, "held_path": c.Path, "key_id": c.KeyID, "label": c.Label, "expires_at": c.ExpiresAt,
					"trust_origin": c.TrustOrigin,
				})
				who := c.Label
				if c.TrustOrigin == "external" {
					who += " (EXTERNAL, untrusted)"
				}
				reasons = append(reasons, fmt.Sprintf("%s is claimed by %s (claim %s, until %s)", p, who, c.Path, c.ExpiresAt))
			}
		}
	}
	if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code": "conflict", "message": strings.Join(reasons, "; "),
				"request_id": r.Header.Get("X-Request-Id"),
			},
			"conflicts": conflicts,
		})
		return
	}
	exp := now.Add(ttlFrom(req.TTLSeconds)).Unix()
	var note any
	if req.Note != "" {
		note = req.Note
	}
	var added []string
	for _, p := range paths {
		if !mine[p] {
			added = append(added, p)
		}
		// An explicit claim upgrades an auto claim on the same path;
		// an auto claim never downgrades an explicit one.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO claims(room_id, key_id, path, auto, note, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(room_id, key_id, path) DO UPDATE SET
			   auto = MIN(claims.auto, excluded.auto),
			   note = COALESCE(excluded.note, claims.note),
			   expires_at = MAX(claims.expires_at, excluded.expires_at)`,
			roomID, caller, p, boolInt(req.Auto), note, now.UTC().Format(time.RFC3339Nano), exp); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert claim")
			return
		}
	}
	// Any claim request is also a heartbeat for the caller's other claims.
	if _, err := tx.ExecContext(ctx,
		`UPDATE claims SET expires_at = MAX(expires_at, ?) WHERE room_id = ? AND key_id = ? AND expires_at > ?`,
		exp, roomID, caller, now.Unix()); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "refresh claims")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	if len(added) > 0 { // refreshes are not worth an audit row each
		_ = audit.Write(ctx, s.DB, caller, "claim.create", "claim", strings.Join(added, ","), roomID, "ok",
			fmt.Sprintf(`{"paths":%s,"auto":%t}`, jsonString(added), req.Auto))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"claims": s.own(ctx, roomID, caller, now)})
}

func (s *Service) own(ctx context.Context, roomID string, keyID int64, now time.Time) []Claim {
	all, _ := active(ctx, s.DB, roomID, now)
	out := []Claim{}
	for _, c := range all {
		if c.KeyID == keyID {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) heartbeat(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	mem, ok := s.access(w, r, roomID, rights.Claim)
	if !ok {
		return
	}
	caller := mem.KeyID
	var req struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	now := s.Now()
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE claims SET expires_at = MAX(expires_at, ?) WHERE room_id = ? AND key_id = ? AND expires_at > ?`,
		now.Add(ttlFrom(req.TTLSeconds)).Unix(), roomID, caller, now.Unix())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "refresh claims")
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": n})
}

func (s *Service) release(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	mem, ok := s.access(w, r, roomID, rights.Claim)
	if !ok {
		return
	}
	caller := mem.KeyID
	var req struct {
		Paths []string `json:"paths"`
		KeyID int64    `json:"key_id"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	target := caller
	if req.KeyID != 0 && req.KeyID != caller {
		if !mem.Admin {
			writeErr(w, r, http.StatusForbidden, "forbidden", "only an admin key can release other keys' claims")
			return
		}
		target = req.KeyID
	}
	want := map[string]bool{}
	for _, p := range req.Paths {
		n, err := Normalize(p)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		want[n] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	held, err := active(ctx, s.DB, roomID, s.Now())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list claims")
		return
	}
	released := []string{}
	for _, c := range held {
		if c.KeyID != target || len(want) > 0 && !want[c.Path] {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM claims WHERE id = ?`, c.ID); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "release claim")
			return
		}
		released = append(released, c.Path)
	}
	if len(released) > 0 {
		_ = audit.Write(ctx, s.DB, caller, "claim.release", "claim", strings.Join(released, ","), roomID, "ok",
			fmt.Sprintf(`{"paths":%s,"key_id":%d}`, jsonString(released), target))
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": released})
}

// decodeOptional decodes a JSON body that may be empty; it answers
// 400 itself on malformed JSON.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return false
	}
	return true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": msg, "request_id": r.Header.Get("X-Request-Id")},
	})
}
