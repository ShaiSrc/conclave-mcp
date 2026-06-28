// Package rooms implements the rooms endpoints from SPEC-0003 §4.
//
// Endpoints:
//
//	POST   /v1/rooms                       -- create (caller becomes owner)
//	GET    /v1/rooms                       -- list rooms caller is a member of
//	GET    /v1/rooms/{id}                  -- details (members only; 404 otherwise)
//	POST   /v1/rooms/{id}/archive          -- soft-delete (owner only)
//	POST   /v1/rooms/{id}/members          -- invite (owner only)
//	DELETE /v1/rooms/{id}/members/{keyid}  -- remove member (owner only)
//
// All mutations are intended to be audit-logged; that hook is
// wired by the server in a follow-up task.
package rooms

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
)

// Service is the rooms HTTP handler.
type Service struct {
	DB *sql.DB
}

func NewService(db *sql.DB) *Service { return &Service{DB: db} }

func (s *Service) Routes(r chi.Router) {
	r.Post("/rooms", s.create)
	r.Get("/rooms", s.list)
	r.Get("/rooms/{id}", s.get)
	r.Post("/rooms/{id}/archive", s.archive)
	// Both /members and /invite are accepted (SPEC-0003 §4.1
	// says "invite"; the original implementation used
	// /members, which is the internal column name. We
	// register /invite first so the chi router matches it
	// when both are requested).
	r.Post("/rooms/{id}/invite", s.invite)
	r.Post("/rooms/{id}/members", s.invite)
	r.Delete("/rooms/{id}/members/{keyid}", s.removeMember)
}

type roomView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	OwnerKeyID  int64  `json:"owner_key_id"`
	CreatedAt   string `json:"created_at"`
	ArchivedAt  string `json:"archived_at,omitempty"`
	// Visibility (SPEC-0001 Q6) is always present in API
	// responses so clients never have to default.
	Visibility string `json:"visibility"`
}

type createRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"` // SPEC-0001 Q6
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if len(req.Name) < 1 || len(req.Name) > 128 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "name must be 1..128 chars")
		return
	}
	if len(req.Description) > 1024 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "description <= 1024 chars")
		return
	}
	// Default visibility to 'private' when the field is
	// missing or empty. The SQL trigger in migration 0003
	// rejects values other than 'private' / 'public', so we
	// normalise here as well for a friendlier 400.
	visibility := req.Visibility
	if visibility == "" {
		visibility = "private"
	}
	if visibility != "private" && visibility != "public" {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "visibility must be private or public")
		return
	}

	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO rooms(id, name, owner_key_id, created_at, description, visibility) VALUES (?, ?, ?, ?, ?, ?)`,
		id, req.Name, caller, now, nullableString(req.Description), visibility,
	); err != nil {
		if isUniqueViolation(err) {
			writeErr(w, r, http.StatusConflict, "conflict", "name already exists")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert room")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES (?, ?, 'owner', ?)`,
		id, caller, now,
	); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert owner member")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	// Audit hook (SPEC-0001 §7: every mutation in the audit
	// log). Failure to write the audit row does not fail the
	// user request; the mutation has already committed. We
	_ = audit.Write(r.Context(), s.DB, caller, "room.create", "room", id, id, "ok",
		`{"name":`+strconvQuote(req.Name)+`,"visibility":`+strconvQuote(visibility)+`}`)
	writeJSON(w, http.StatusCreated, roomView{
		ID:          id,
		Name:        req.Name,
		Description: req.Description,
		OwnerKeyID:  caller,
		CreatedAt:   now,
		Visibility:  visibility,
	})
}

// list returns rooms visible to the caller (SPEC-0003 §3.3):
// all public rooms + all rooms the caller is a member of.
// An optional `?visibility=public` filter restricts the result.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	visibilityFilter := r.URL.Query().Get("visibility")
	if visibilityFilter != "" && visibilityFilter != "private" && visibilityFilter != "public" {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "visibility must be private or public")
		return
	}

	// SQL: if the caller wants public only, drop the
	// member-join and return all public, non-archived rooms.
	// Otherwise return the union of public + member rooms.
	var (
		rows *sql.Rows
		err  error
	)
	if visibilityFilter == "public" {
		rows, err = s.DB.QueryContext(r.Context(),
			`SELECT id, name, description, owner_key_id, created_at, archived_at, visibility
			 FROM rooms
			 WHERE visibility = 'public' AND archived_at IS NULL
			 ORDER BY created_at DESC`)
	} else if visibilityFilter == "private" {
		// "private only" means "rooms I am a member of that
		// are private". This is the "personal workspace"
		// view; public rooms are excluded by the user.
		rows, err = s.DB.QueryContext(r.Context(),
			`SELECT r.id, r.name, r.description, r.owner_key_id, r.created_at, r.archived_at, r.visibility
			 FROM rooms r
			 INNER JOIN room_members m ON m.room_id = r.id
			 WHERE m.key_id = ? AND r.visibility = 'private' AND r.archived_at IS NULL
			 ORDER BY r.created_at DESC`,
			caller)
	} else {
		// No filter: public rooms + rooms the caller is a
		// member of. The UNION keeps the result set
		// deduplicated by id.
		rows, err = s.DB.QueryContext(r.Context(),
			`SELECT r.id, r.name, r.description, r.owner_key_id, r.created_at, r.archived_at, r.visibility
			 FROM rooms r
			 WHERE r.visibility = 'public' AND r.archived_at IS NULL
			 UNION
			 SELECT r.id, r.name, r.description, r.owner_key_id, r.created_at, r.archived_at, r.visibility
			 FROM rooms r
			 INNER JOIN room_members m ON m.room_id = r.id
			 WHERE m.key_id = ? AND r.archived_at IS NULL
			 ORDER BY created_at DESC`,
			caller)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
		return
	}
	defer rows.Close()

	var out []roomView
	for rows.Next() {
		var (
			rv       roomView
			desc     sql.NullString
			archived sql.NullString
		)
		if err := rows.Scan(&rv.ID, &rv.Name, &desc, &rv.OwnerKeyID, &rv.CreatedAt, &archived, &rv.Visibility); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		if desc.Valid {
			rv.Description = desc.String
		}
		if archived.Valid {
			rv.ArchivedAt = archived.String
		}
		out = append(out, rv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	var (
		rv       roomView
		desc     sql.NullString
		archived sql.NullString
	)
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT r.id, r.name, r.description, r.owner_key_id, r.created_at, r.archived_at, r.visibility
		 FROM rooms r
		 INNER JOIN room_members m ON m.room_id = r.id
		 WHERE r.id = ? AND m.key_id = ?`,
		id, caller,
	).Scan(&rv.ID, &rv.Name, &desc, &rv.OwnerKeyID, &rv.CreatedAt, &archived, &rv.Visibility)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "get query")
		return
	}
	if desc.Valid {
		rv.Description = desc.String
	}
	if archived.Valid {
		rv.ArchivedAt = archived.String
	}
	writeJSON(w, http.StatusOK, rv)
}

func (s *Service) archive(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE rooms SET archived_at = ?
		 WHERE id = ? AND owner_key_id = ? AND archived_at IS NULL`,
		now, id, caller,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "archive")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found or not owner")
		return
	}
	_ = audit.Write(r.Context(), s.DB, caller, "room.archive", "room", id, id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

type inviteRequest struct {
	KeyID int64  `json:"key_id"`
	Role  string `json:"role"`
}

func (s *Service) invite(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if req.KeyID == 0 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "key_id required")
		return
	}
	switch req.Role {
	case "owner", "writer", "reader":
	default:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "role must be owner|writer|reader")
		return
	}
	var ownerID int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT owner_key_id FROM rooms WHERE id = ?`, id,
	).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "owner lookup")
		return
	}
	if ownerID != caller {
		writeErr(w, r, http.StatusForbidden, "forbidden", "only owner can invite")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.DB.ExecContext(r.Context(),
		`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		id, req.KeyID, req.Role, now,
	); err != nil {
		if isUniqueViolation(err) {
			writeErr(w, r, http.StatusConflict, "conflict", "already a member")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert member")
		return
	}
	_ = audit.Write(r.Context(), s.DB, caller, "room.invite", "member", strconv.FormatInt(req.KeyID, 10), id, "ok",
		`{"room_id":`+strconvQuote(id)+`,"role":`+strconvQuote(req.Role)+`}`)
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) removeMember(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	keyid, err := strconv.ParseInt(chi.URLParam(r, "keyid"), 10, 64)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "keyid must be integer")
		return
	}
	var ownerID int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT owner_key_id FROM rooms WHERE id = ?`, id,
	).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "owner lookup")
		return
	}
	if ownerID != caller {
		writeErr(w, r, http.StatusForbidden, "forbidden", "only owner can remove members")
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM room_members WHERE room_id = ? AND key_id = ?`,
		id, keyid,
	)
	if err != nil {
		if isOwnerProtection(err) {
			writeErr(w, r, http.StatusConflict, "conflict", "room must retain at least one owner")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "delete member")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, r, http.StatusNotFound, "not_found", "member not found")
		return
	}
	_ = audit.Write(r.Context(), s.DB, caller, "room.remove", "member", strconv.FormatInt(keyid, 10), id, "ok",
		`{"room_id":`+strconvQuote(id)+`}`)
	w.WriteHeader(http.StatusNoContent)
}

func callerKeyID(r *http.Request) int64 {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		return 0
	}
	return c.KeyID
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	return err != nil && contains(err.Error(), "UNIQUE constraint failed")
}

func isOwnerProtection(err error) bool {
	return err != nil && contains(err.Error(), "must retain at least one owner")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// strconvQuote is a tiny helper to JSON-stringify a Go string
// without pulling in strconv.Quote (which would add an import
// for the audit detail blobs).
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    msg,
			"request_id": r.Header.Get("X-Request-Id"),
		},
	})
}
