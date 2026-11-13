// Package rooms implements the rooms endpoints from SPEC-0003 §4.
//
// Endpoints ("admin" = admin key only; room access is checked by
// internal/rights):
//
//	POST   /v1/rooms                       -- create (admin)
//	GET    /v1/rooms                       -- the caller's rooms (admin: every room)
//	GET    /v1/rooms/{id}                  -- details + my_rights (members, admin; 404 otherwise)
//	GET    /v1/rooms/{id}/members          -- members (read right, admin)
//	POST   /v1/rooms/{id}/archive          -- soft-delete (admin)
//	POST   /v1/rooms/{id}/members          -- invite with rights (admin; /invite is an alias);
//	                                          a bot of a peer server by its address
//	PATCH  /v1/rooms/{id}/members/{keyid}  -- replace a member's rights (admin)
//	DELETE /v1/rooms/{id}/members/{keyid}  -- remove member (admin); in a
//	                                          replica room only this server's
//	                                          bot, which leaves on the host first
//
// Mutations are audit-logged. With federation on, every change of a
// room with remote members is written to its event log in the change's
// transaction (internal/fedrooms).
package rooms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// Service is the rooms HTTP handler.
type Service struct {
	DB *sql.DB
	// FedDomain is the federation domain, "" with federation off: then
	// nothing is logged and invites by address answer 404.
	FedDomain string
	// InviteRemote asks the server of address (name@domain, lowercase)
	// to add its bot to room roomID, named name, with rights. A refusal
	// is a *fedrooms.Error. Set by the server with federation on.
	InviteRemote func(ctx context.Context, roomID, name, address string, rights []string) error
	// FedNotify, when set, is called after each change of a room's
	// members or state: it wakes the federation delivery worker.
	FedNotify func()
	// Leave tells the host of replica room roomID that keyID, a bot of
	// this server, leaves the room; the bot is removed here after it.
	// A refusal is a *fedrooms.Error. Set by the server with federation
	// on.
	Leave func(ctx context.Context, roomID string, keyID int64) error
}

// changed runs FedNotify after a committed change.
func (s *Service) changed() {
	if s.FedNotify != nil {
		s.FedNotify()
	}
}

func NewService(db *sql.DB) *Service { return &Service{DB: db} }

func (s *Service) Routes(r chi.Router) {
	r.Post("/rooms", s.create)
	r.Get("/rooms", s.list)
	r.Get("/rooms/{id}", s.get)
	r.Post("/rooms/{id}/archive", s.archive)
	// Both /members and /invite are accepted (SPEC-0003 §4.1 says
	// "invite"; the original implementation used /members).
	r.Post("/rooms/{id}/invite", s.invite)
	r.Post("/rooms/{id}/members", s.invite)
	r.Get("/rooms/{id}/members", s.members)
	r.Patch("/rooms/{id}/members/{keyid}", s.setRights)
	r.Delete("/rooms/{id}/members/{keyid}", s.removeMember)
}

// requireAdmin answers 403 unless the caller is an admin key: only the
// operator manages rooms, members and rights.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if auth.IsAdmin(r.Context()) {
		return true
	}
	writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
	return false
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
	Home       string `json:"home,omitempty"` // the host of a replica room
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
	if !requireAdmin(w, r) {
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
	// The creator is an admin key; its row keeps the owner role (the
	// owner trigger needs one) and gets every right.
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'owner', ?, ?)`,
		id, caller, now, rights.All().Storage(),
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

// list returns the rooms the caller is a member of; an admin key gets
// every room. Archived rooms are left out; ?visibility= narrows the set.
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
	admin := 0
	if auth.IsAdmin(r.Context()) {
		admin = 1
	}
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT r.id, r.name, r.description, r.owner_key_id, r.created_at, r.archived_at, r.visibility, COALESCE(r.home_domain, '')
		 FROM rooms r
		 WHERE r.archived_at IS NULL
		   AND (? = 1 OR EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = r.id AND m.key_id = ?))
		   AND (? = '' OR r.visibility = ?)
		 ORDER BY r.created_at DESC`,
		admin, caller, visibilityFilter, visibilityFilter)
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
		if err := rows.Scan(&rv.ID, &rv.Name, &desc, &rv.OwnerKeyID, &rv.CreatedAt, &archived, &rv.Visibility, &rv.Home); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		rv.Description, rv.ArchivedAt = desc.String, archived.String
		out = append(out, rv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// get returns a room to its members (whatever their rights) and to
// admin keys, with the caller's own rights as my_rights.
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	mem, ok := rights.Require(w, r, s.DB, id, "")
	if !ok {
		return
	}
	var (
		rv       roomView
		desc     sql.NullString
		archived sql.NullString
	)
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT id, name, description, owner_key_id, created_at, archived_at, visibility, COALESCE(home_domain, '') FROM rooms WHERE id = ?`, id,
	).Scan(&rv.ID, &rv.Name, &desc, &rv.OwnerKeyID, &rv.CreatedAt, &archived, &rv.Visibility, &rv.Home); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "get query")
		return
	}
	rv.Description, rv.ArchivedAt = desc.String, archived.String
	writeJSON(w, http.StatusOK, struct {
		roomView
		MyRights rights.Set `json:"my_rights"`
	}{rv, mem.Rights})
}

// members lists a room's members to keys holding read and to admin
// keys; only admin keys also see admin keys and each member's rights.
// A bot of another server also has its address.
func (s *Service) members(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	mem, ok := rights.Require(w, r, s.DB, id, rights.Read)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT m.key_id, k.label, k.trust_origin, m.rights, k.remote_domain IS NOT NULL
		 FROM room_members m JOIN api_keys k ON k.id = m.key_id
		 WHERE m.room_id = ? AND (? OR k.is_admin = 0) ORDER BY m.key_id`, id, mem.Admin)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "members query")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			keyID                int64
			label, trust, stored string
			remote               bool
		)
		if err := rows.Scan(&keyID, &label, &trust, &stored, &remote); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		m := map[string]any{"key_id": keyID, "label": label, "trust_origin": trust}
		if remote { // a bot of another server: its stand-in's label is its address
			m["address"] = label
		}
		if mem.Admin {
			m["rights"] = rights.FromStorage(stored)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (s *Service) archive(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	if s.refuseReplica(w, r, id) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE rooms SET archived_at = ? WHERE id = ? AND archived_at IS NULL`,
		now, id,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "archive")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found or already archived")
		return
	}
	if err := fedrooms.LogRoom(r.Context(), tx, id, s.FedDomain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "event log")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	s.changed()
	_ = audit.Write(r.Context(), s.DB, caller, "room.archive", "room", id, id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

type inviteRequest struct {
	KeyID int64 `json:"key_id"`
	// Address invites a bot of a peer server, name@domain, instead.
	Address string `json:"address"`
	// Rights wins over Role. Role is what clients from before rights
	// send: owner/writer become all, reader becomes read.
	Rights         []string `json:"rights"`
	Role           string   `json:"role"`
	AllowMultiRoom bool     `json:"allow_multi_room"`
}

func (s *Service) invite(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	if s.refuseReplica(w, r, id) {
		return
	}
	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if (req.KeyID == 0) == (req.Address == "") {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "key_id or address required, not both")
		return
	}
	set, err := rights.Parse(req.Rights)
	if req.Rights == nil && req.Role != "" {
		set, err = rights.FromRole(req.Role)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Address != "" {
		s.inviteRemote(w, r, id, strings.ToLower(strings.TrimSpace(req.Address)), set)
		return
	}
	var exists int
	if err := s.DB.QueryRowContext(r.Context(), `SELECT 1 FROM rooms WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "room lookup")
		return
	}
	// One room per key: a key that is not an admin key and is already
	// in another room that is not archived needs allow_multi_room.
	var isAdmin, elsewhere, remote bool
	err = s.DB.QueryRowContext(r.Context(),
		`SELECT k.is_admin, EXISTS (
		   SELECT 1 FROM room_members m JOIN rooms o ON o.id = m.room_id
		   WHERE m.key_id = k.id AND m.room_id != ? AND o.archived_at IS NULL), k.remote_domain IS NOT NULL
		 FROM api_keys k WHERE k.id = ? AND k.revoked_at IS NULL`,
		id, req.KeyID,
	).Scan(&isAdmin, &elsewhere, &remote)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, r, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "key lookup")
		return
	}
	if remote {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "a bot of another server is invited by its address")
		return
	}
	if elsewhere && !isAdmin && !req.AllowMultiRoom {
		writeErr(w, r, http.StatusConflict, "key_in_other_room",
			"key is already a member of another room; pass allow_multi_room to add it anyway")
		return
	}
	if _, ok := s.addMember(w, r, id, req.KeyID, "", set); !ok {
		return
	}
	detail, _ := json.Marshal(map[string]any{"room_id": id, "rights": set})
	_ = audit.Write(r.Context(), s.DB, caller, "room.invite", "member", strconv.FormatInt(req.KeyID, 10), id, "ok", string(detail))
	w.WriteHeader(http.StatusCreated)
}

// inviteRemote invites the bot at address: its server is asked first,
// and only once it accepted is the bot's stand-in added.
func (s *Service) inviteRemote(w http.ResponseWriter, r *http.Request, roomID, address string, set rights.Set) {
	if s.InviteRemote == nil {
		writeErr(w, r, http.StatusNotFound, "federation_disabled", "federation is not enabled on this server")
		return
	}
	var (
		name     string
		archived sql.NullString
		member   bool
	)
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT r.name, r.archived_at, EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
		   WHERE m.room_id = r.id AND k.label = ? AND k.remote_domain IS NOT NULL)
		 FROM rooms r WHERE r.id = ?`, address, roomID).Scan(&name, &archived, &member)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "room lookup")
		return
	case archived.Valid:
		writeErr(w, r, http.StatusGone, "not_found", "room is archived")
		return
	case member:
		writeErr(w, r, http.StatusConflict, "conflict", "already a member")
		return
	}
	if err := s.InviteRemote(r.Context(), roomID, name, address, set); err != nil {
		writeFedErr(w, r, err, "invite")
		return
	}
	keyID, ok := s.addMember(w, r, roomID, 0, address, set)
	if !ok {
		return
	}
	detail, _ := json.Marshal(map[string]any{"room_id": roomID, "address": address, "rights": set})
	_ = audit.Write(r.Context(), s.DB, callerKeyID(r), "room.invite", "member", strconv.FormatInt(keyID, 10), roomID, "ok", string(detail))
	writeJSON(w, http.StatusCreated, map[string]any{"key_id": keyID, "address": address, "rights": set})
}

// addMember adds keyID to roomID with set, or with an address the
// stand-in of that remote bot, created if needed, and logs it, in one
// transaction. It returns the key added, or answers the request itself
// on failure.
func (s *Service) addMember(w http.ResponseWriter, r *http.Request, roomID string, keyID int64, address string, set rights.Set) (int64, bool) {
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return 0, false
	}
	defer tx.Rollback()
	if address != "" {
		if keyID, err = fedrooms.StandIn(ctx, tx, address); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "stand-in key")
			return 0, false
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)`,
		roomID, keyID, time.Now().UTC().Format(time.RFC3339Nano), set.Storage(),
	); err != nil {
		if isUniqueViolation(err) {
			writeErr(w, r, http.StatusConflict, "conflict", "already a member")
			return 0, false
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert member")
		return 0, false
	}
	if err := fedrooms.LogJoin(ctx, tx, roomID, keyID, s.FedDomain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "event log")
		return 0, false
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return 0, false
	}
	s.changed()
	return keyID, true
}

// setRights replaces a member's rights (PATCH /rooms/{id}/members/{keyid}).
func (s *Service) setRights(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	if s.refuseReplica(w, r, id) {
		return
	}
	keyid, err := strconv.ParseInt(chi.URLParam(r, "keyid"), 10, 64)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "keyid must be integer")
		return
	}
	var req struct {
		Rights []string `json:"rights"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Rights == nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", `body must be {"rights": [...]}`)
		return
	}
	set, err := rights.Parse(req.Rights)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// ponytail: the old rights are read outside a transaction, so two
	// concurrent PATCHes of one member can audit a stale "old".
	// Admin-only and rare; wrap both in a tx if that ever matters.
	var old string
	err = s.DB.QueryRowContext(r.Context(),
		`SELECT rights FROM room_members WHERE room_id = ? AND key_id = ?`, id, keyid,
	).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, r, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "member lookup")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE room_members SET rights = ? WHERE room_id = ? AND key_id = ?`, set.Storage(), id, keyid,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "update rights")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 { // removed between lookup and update
		writeErr(w, r, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if err := fedrooms.LogMember(r.Context(), tx, id, keyid, s.FedDomain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "event log")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	s.changed()
	detail, _ := json.Marshal(map[string]any{"room_id": id, "old": rights.FromStorage(old), "new": set})
	_ = audit.Write(r.Context(), s.DB, callerKeyID(r), "room.rights", "member", strconv.FormatInt(keyid, 10), id, "ok", string(detail))
	writeJSON(w, http.StatusOK, map[string]any{"key_id": keyid, "rights": set})
}

func (s *Service) removeMember(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	caller := callerKeyID(r)
	id := chi.URLParam(r, "id")
	keyid, err := strconv.ParseInt(chi.URLParam(r, "keyid"), 10, 64)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "keyid must be integer")
		return
	}
	// This server's own bot may leave a replica room; nothing else changes it.
	// A stand-in's label is its address, kept in the audit entry.
	var local, member bool
	var address string
	var home sql.NullString
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT remote_domain IS NULL, CASE WHEN remote_domain IS NULL THEN '' ELSE label END FROM api_keys WHERE id = ?`, keyid,
	).Scan(&local, &address); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "key lookup")
		return
	}
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT home_domain, EXISTS (SELECT 1 FROM room_members WHERE room_id = ? AND key_id = ?) FROM rooms WHERE id = ?`, id, keyid, id,
	).Scan(&home, &member); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "room lookup")
		return
	}
	switch {
	case !home.Valid: // hosted here, or no such room (404 below)
	case !local:
		writeErr(w, r, http.StatusForbidden, "replica_room", "room is hosted by "+home.String+"; change it there")
		return
	case !member:
		writeErr(w, r, http.StatusNotFound, "not_found", "member not found")
		return
	case s.Leave != nil: // the host first: if it cannot be told, nothing changes here
		if err := s.Leave(r.Context(), id, keyid); err != nil {
			writeFedErr(w, r, err, "leave")
			return
		}
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
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
	if err := fedrooms.LogRemove(r.Context(), tx, id, keyid, s.FedDomain); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "event log")
		return
	}
	// A replica with no key of ours left is archived, as when its host
	// removes our last bot.
	if home.Valid {
		if _, err := tx.ExecContext(r.Context(), `UPDATE rooms SET archived_at = COALESCE(archived_at, ?) WHERE id = ?
			AND NOT EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE m.room_id = ? AND k.remote_domain IS NULL)`,
			time.Now().UTC().Format(time.RFC3339Nano), id, id); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "archive replica")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	s.changed()
	detail := map[string]string{"room_id": id}
	if address != "" {
		detail["address"] = address
	}
	b, _ := json.Marshal(detail)
	_ = audit.Write(r.Context(), s.DB, caller, "room.remove", "member", strconv.FormatInt(keyid, 10), id, "ok", string(b))
	w.WriteHeader(http.StatusNoContent)
}

// refuseReplica answers 403 replica_room and returns true when roomID
// is a replica: its host owns the members, rights and archive state.
// A room that does not exist is left to the handler's 404.
func (s *Service) refuseReplica(w http.ResponseWriter, r *http.Request, roomID string) bool {
	var home sql.NullString
	err := s.DB.QueryRowContext(r.Context(), `SELECT home_domain FROM rooms WHERE id = ?`, roomID).Scan(&home)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "room lookup")
		return true
	case home.Valid:
		writeErr(w, r, http.StatusForbidden, "replica_room", "room is hosted by "+home.String+"; change it there")
		return true
	}
	return false
}

// writeFedErr answers a refusal of the federation service with its
// status and code, and any other error with 500.
func writeFedErr(w http.ResponseWriter, r *http.Request, err error, what string) {
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		writeErr(w, r, fe.Status, fe.Code, fe.Message)
		return
	}
	writeErr(w, r, http.StatusInternalServerError, "internal_error", what)
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
