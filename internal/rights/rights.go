// Package rights is what a key may do in a room: the set of rights on
// its membership (room_members.rights, migration 0008), and the check
// every room-scoped handler runs (Require).
package rights

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// Rights a membership can hold besides the message types. Each message
// type is the right to send messages of that type; a message without a
// type is stored as System.
const (
	Read   = "read"
	Claim  = "claim"
	System = "system"
)

// Order lists every right in storage order.
var Order = []string{Read, Claim, "question", "answer", "decision", "handoff",
	"status_update", "task_assignment", "alert", System}

// Set is a membership's rights in Order. Parse and FromStorage never
// return nil, so an empty Set encodes as [] in JSON.
type Set []string

// All is every right.
func All() Set { return slices.Clone(Set(Order)) }

// Parse checks rights from the API or the CLI: names from Order plus
// the shorthands "all" and "none". "all" is expanded here, so a right
// added later is not granted to existing members.
func Parse(in []string) (Set, error) {
	want := map[string]bool{}
	for _, r := range in {
		switch r = strings.TrimSpace(r); {
		case r == "all":
			for _, o := range Order {
				want[o] = true
			}
		case r == "none" || r == "":
		case slices.Contains(Order, r):
			want[r] = true
		default:
			return nil, fmt.Errorf("unknown right %q (known: all, none, %s)", r, strings.Join(Order, ", "))
		}
	}
	out := Set{}
	for _, o := range Order {
		if want[o] {
			out = append(out, o)
		}
	}
	return out, nil
}

// FromRole translates the role an old client sends instead of rights.
func FromRole(role string) (Set, error) {
	switch role {
	case "owner", "writer":
		return All(), nil
	case "reader":
		return Set{Read}, nil
	}
	return nil, errors.New("role must be owner|writer|reader")
}

// FromStorage reads the room_members.rights column.
func FromStorage(s string) Set {
	if s == "" {
		return Set{}
	}
	return Set(strings.Split(s, ","))
}

// Storage is the room_members.rights value: comma-separated, in Order.
func (s Set) Storage() string { return strings.Join(s, ",") }

// Has reports whether s holds right.
func (s Set) Has(right string) bool { return slices.Contains(s, right) }

// Member is the caller's standing in one room.
type Member struct {
	KeyID       int64
	Admin       bool
	Rights      Set    // an admin key holds All
	TrustOrigin string // the caller key's mark, copied onto its messages
	Archived    bool
}

// DB is what Lookup needs; *sql.DB satisfies it.
type DB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Lookup reads the caller's membership of roomID. It runs on every
// request (no cache), so a change of rights applies to the next one.
// ok is false when the room does not exist or the caller is neither a
// member nor an admin key.
func Lookup(ctx context.Context, db DB, roomID string) (mem Member, ok bool, err error) {
	c, _ := auth.FromContext(ctx)
	mem = Member{KeyID: c.KeyID, Admin: auth.IsAdmin(ctx)}
	var archived, stored sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT r.archived_at, k.trust_origin, m.rights
		 FROM rooms r
		 JOIN api_keys k ON k.id = ?
		 LEFT JOIN room_members m ON m.room_id = r.id AND m.key_id = k.id
		 WHERE r.id = ?`,
		c.KeyID, roomID,
	).Scan(&archived, &mem.TrustOrigin, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return mem, false, nil
	}
	if err != nil {
		return mem, false, err
	}
	switch {
	case mem.Admin:
		mem.Rights = All()
	case stored.Valid:
		mem.Rights = FromStorage(stored.String)
	default:
		return mem, false, nil // not a member
	}
	mem.Archived = archived.Valid
	return mem, true, nil
}

// Require answers the request itself and returns ok=false unless the
// caller may act in roomID: 404 when the room does not exist or the
// caller is neither a member nor an admin key (a key cannot tell which),
// 403 missing_right when need is not "" and the caller lacks it.
// Archived rooms are left to the handler.
func Require(w http.ResponseWriter, r *http.Request, db DB, roomID, need string) (Member, bool) {
	mem, ok, err := Lookup(r.Context(), db, roomID)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "membership lookup")
	case !ok:
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
	case need != "" && !mem.Rights.Has(need):
		writeErr(w, r, http.StatusForbidden, "missing_right", "missing right: "+need)
	default:
		return mem, true
	}
	return mem, false
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "request_id": r.Header.Get("X-Request-Id")},
	})
}

// Split turns the CLI form "read, handoff" into ["read" "handoff"] and
// "" into [] (never nil, so it is sent as []); Parse checks the names.
func Split(s string) []string {
	out := []string{}
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
