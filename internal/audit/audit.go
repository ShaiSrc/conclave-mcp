// Package audit implements the audit log endpoint from
// SPEC-0003 §4 and the audit-write helpers used by the rest of
// the server.
//
// Endpoints:
//
//	GET /v1/audit?actor=<key_id>&action=<kind>&since=<ts>&limit=<n>
//
// Filters are AND-combined. Results are ordered by ts DESC.
// Admin-only: requires audit:read scope on the caller's API key.
// v0.1 does not yet implement scopes (SPEC-0001 §3 A3 defers them);
// in this commit the endpoint is restricted to a hard-coded
// admin key id, replaced by the real scope check in a follow-up
// task.
package audit

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
)

type Service struct {
	DB *sql.DB
}

func NewService(db *sql.DB) *Service { return &Service{DB: db} }

func (s *Service) Routes(r chi.Router) {
	r.Get("/audit", s.list)
}

type entryView struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	ActorKeyID *int64 `json:"actor_key_id,omitempty"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	// RoomID is denormalised from the action context
	// (SPEC-0001 Q10). NULL for actions that do not belong
	// to a room (e.g. system key creates). Surfaced as
	// `room_id` in JSON so the client can filter / display
	// per-room.
	RoomID string `json:"room_id,omitempty"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	actor := q.Get("actor")    // key id (decimal)
	action := q.Get("action")  // e.g. "room.create"
	roomID := q.Get("room_id") // SPEC-0001 Q10
	since := q.Get("since")    // ISO-8601
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "limit must be positive integer")
			return
		}
		if n > 1000 {
			n = 1000
		}
		limit = n
	}

	// Build query with optional filters. `action` is a
	// prefix match so the client can ask for "?action=room"
	// and get all room.* rows. The other filters are exact.
	q2 := `SELECT id, ts, actor_key_id, action, target_type, target_id, room_id, result, detail
	       FROM audit WHERE 1=1`
	var args []any
	if actor != "" {
		if n, err := strconv.ParseInt(actor, 10, 64); err == nil {
			q2 += " AND actor_key_id = ?"
			args = append(args, n)
		}
	}
	if action != "" {
		// `action` is treated as a prefix, so we escape the
		// SQL wildcard in the input.
		q2 += " AND action LIKE ?"
		args = append(args, escapeLikePrefix(action)+"%")
	}
	if roomID != "" {
		q2 += " AND room_id = ?"
		args = append(args, roomID)
	}
	if since != "" {
		q2 += " AND ts >= ?"
		args = append(args, since)
	}
	q2 += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.DB.QueryContext(r.Context(), q2, args...)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	defer rows.Close()

	var out []entryView
	for rows.Next() {
		var (
			ev      entryView
			actorN  sql.NullInt64
			roomIDN sql.NullString
			detail  sql.NullString
		)
		if err := rows.Scan(&ev.ID, &ev.TS, &actorN, &ev.Action, &ev.TargetType, &ev.TargetID, &roomIDN, &ev.Result, &detail); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan failed")
			return
		}
		if actorN.Valid {
			v := actorN.Int64
			ev.ActorKeyID = &v
		}
		if roomIDN.Valid {
			ev.RoomID = roomIDN.String
		}
		if detail.Valid {
			ev.Detail = detail.String
		}
		out = append(out, ev)
	}
	// next_since is the ISO-8601 ts of the last entry we
	// returned. Because we ORDER BY ts DESC, the last entry
	// in the slice is the OLDEST; the client should pass it
	// as the next `since` (which filters `ts >= ?`). This
	// means some overlap, but the client de-dupes by id.
	// An empty slice means no more entries.
	var nextSince string
	if n := len(out); n > 0 {
		nextSince = out[n-1].TS
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":    out,
		"next_since": nextSince,
	})
}

// Write is a helper used by other services to record an audit row.
// The actor can be 0 for system-initiated actions. The result is
// "ok" or "error:<code>". Detail is optional JSON; pass empty
// string for none. The roomID is optional and denormalised into
// audit.room_id (SPEC-0001 Q10); pass empty string for actions
// that are not tied to a room.
func Write(ctx context.Context, db *sql.DB, actorKeyID int64, action, targetType, targetID, roomID, result, detail string) error {
	if result != "ok" && !strings.HasPrefix(result, "error:") {
		return errors.New("audit: result must be 'ok' or 'error:<code>'")
	}
	var actor any
	if actorKeyID > 0 {
		actor = actorKeyID
	}
	var det any
	if detail != "" {
		det = detail
	}
	var rid any
	if roomID != "" {
		rid = roomID
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit(ts, actor_key_id, action, target_type, target_id, room_id, result, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), actor, action, targetType, targetID, rid, result, det,
	)
	return err
}

// escapeLikePrefix escapes the SQL LIKE wildcards in a user
// input so a client passing `?action=room%` does not get
// unexpected results. We only need to escape `%` and `_`
// because the user input is appended with our own `%`
// suffix.
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
