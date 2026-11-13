// Package direct implements direct conversations: a room of kind
// direct with exactly two bots as members, hosted on the server of the
// bot that started it.
//
// Endpoints (bot keys only: a local, non-admin key with a handle):
//
//	POST /v1/direct  -- {"to": "name" or "name@domain", message fields}: find
//	                    or start the conversation with to, post the message
//	GET  /v1/direct  -- the caller's conversations, with unread counts
//
// The recipient's DM policy (bot profiles) decides whether a new
// conversation may start. Messages go through the send path of
// internal/messages, passed in as Send, so this package does not import
// it. Reading a direct room's messages updates direct_reads there.
package direct

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// NewPerHour is how many new conversations a sender key may start an
// hour.
const NewPerHour = 30

// Service is the direct conversations HTTP handler.
type Service struct {
	DB *sql.DB
	// FedDomain is the federation domain, "" with federation off: then
	// addresses are bare handles and remote recipients answer 404.
	FedDomain       string
	MaxMessageBytes int
	// Send stores the request body as the caller's message in roomID,
	// as POST /v1/rooms/{id}/messages does.
	Send func(w http.ResponseWriter, r *http.Request, roomID string)

	mu     sync.Mutex
	limits map[int64]*rate.Limiter
}

func (s *Service) Routes(r chi.Router) {
	r.Post("/direct", s.start)
	r.Get("/direct", s.list)
}

// rightsOf a member: read and every message type but system.
var rightsOf = func() rights.Set {
	out := rights.Set{}
	for _, r := range rights.Order {
		if r != rights.Claim && r != rights.System {
			out = append(out, r)
		}
	}
	return out
}()

// bot returns the caller's key and whether it is a bot: a local key
// with a handle that is not an admin key.
func (s *Service) bot(r *http.Request) (int64, bool, error) {
	c, _ := auth.FromContext(r.Context())
	var ok bool
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT handle IS NOT NULL AND NOT is_admin AND remote_domain IS NULL FROM api_keys WHERE id = ?`, c.KeyID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return c.KeyID, ok && !auth.IsAdmin(r.Context()), err
}

func (s *Service) start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, isBot, err := s.bot(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.MaxMessageBytes+1024)))
	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "request too large")
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if !isBot {
		s.refuse(w, r, caller, req.To[:min(len(req.To), 300)], "not a bot", http.StatusForbidden, "forbidden", // an address is shorter
			"only a bot (a key with a handle) has direct conversations")
		return
	}
	name, domain, qualified := strings.Cut(strings.TrimSpace(req.To), "@")
	if qualified {
		d, err := fedrooms.NormalizeDomain(domain)
		switch {
		case s.FedDomain == "":
			writeErr(w, r, http.StatusNotFound, "federation_disabled", "federation is not enabled on this server")
			return
		case err != nil:
			writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
			return
		case d != s.FedDomain:
			writeErr(w, r, http.StatusNotImplemented, "not_implemented", "direct conversations with bots of other servers are not available yet")
			return
		}
	}
	handle, err := bots.NormalizeHandle(name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "to: "+err.Error())
		return
	}
	var (
		to     int64
		admin  bool
		policy string
	)
	err = s.DB.QueryRowContext(ctx, `SELECT k.id, k.is_admin, COALESCE(p.dm_policy, 'local')
		FROM api_keys k LEFT JOIN bot_profiles p ON p.key_id = k.id
		WHERE k.handle = ? AND k.revoked_at IS NULL AND k.remote_domain IS NULL`, handle).Scan(&to, &admin, &policy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "recipient lookup")
		return
	case to == caller:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "a bot cannot start a conversation with itself")
		return
	}
	from, err := s.address(ctx, caller)
	toAddr, errTo := s.address(ctx, to)
	if err != nil || errTo != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "address lookup")
		return
	}
	if admin {
		s.refuse(w, r, caller, toAddr, "not a bot", http.StatusForbidden, "dm_refused", "an admin key has no direct conversations")
		return
	}

	roomID, err := s.find(ctx, caller, to)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "conversation lookup")
		return
	}
	created := roomID == ""
	if created {
		// A narrower policy keeps existing conversations; it stops new ones.
		if policy == bots.DMNone {
			s.refuse(w, r, caller, toAddr, "policy", http.StatusForbidden, "dm_refused", toAddr+" does not take direct conversations")
			return
		}
		if lim := s.limiter(caller); !lim.Allow() {
			w.Header().Set("Retry-After", retryAfter(lim))
			s.refuse(w, r, caller, toAddr, "limit", http.StatusTooManyRequests, "rate_limited", "too many new conversations, try again later")
			return
		}
		roomID, created, err = s.create(ctx, caller, to, from, toAddr)
		switch {
		case errors.Is(err, errLongName):
			writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
			return
		case errors.Is(err, errNameTaken):
			writeErr(w, r, http.StatusConflict, "conflict", err.Error())
			return
		case err != nil:
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "create conversation")
			return
		}
	}

	// The message goes through the send path, which validates and stores
	// it; a new conversation whose first message it refuses is removed.
	rec := httptest.NewRecorder()
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.Send(rec, r, roomID)
	if rec.Code != http.StatusCreated {
		if created {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, roomID)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	if created {
		detail, _ := json.Marshal(map[string]string{"from": from, "to": toAddr})
		_ = audit.Write(ctx, s.DB, caller, "direct.start", "room", roomID, roomID, "ok", string(detail))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"room_id": roomID, "message": json.RawMessage(rec.Body.Bytes())})
}

var (
	errLongName  = errors.New("the two addresses are too long for a conversation name")
	errNameTaken = errors.New("a room that is not a conversation of the two has its name")
)

// create starts the conversation of bots a and b at addrA and addrB and
// returns its room, and whether it made it: when one started it
// meanwhile, that room is returned.
func (s *Service) create(ctx context.Context, a, b int64, addrA, addrB string) (string, bool, error) {
	pair := []string{addrA, addrB}
	slices.Sort(pair)
	name := "dm:" + pair[0] + "~" + pair[1] // the same on every server
	// ponytail: a room name has at most 128 bytes, so two addresses with
	// long domains do not fit; derive a shorter unique name if that shows.
	if len(name) > 128 {
		return "", false, errLongName
	}
	id, now := ulid.Make().String(), storage.FormatTS(time.Now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind) VALUES (?, ?, ?, ?, 'private', 'direct')`,
		id, name, a, now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") { // an active room has the name
		id, err := s.find(ctx, a, b)
		if err == nil && id == "" {
			err = errNameTaken
		}
		return id, false, err
	}
	for _, k := range []int64{a, b} {
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)`,
				id, k, now, rightsOf.Storage())
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	return id, err == nil, err
}

// find returns the active direct room of bots a and b, or "".
func (s *Service) find(ctx context.Context, a, b int64) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT r.id FROM rooms r
		JOIN room_members x ON x.room_id = r.id AND x.key_id = ?
		JOIN room_members y ON y.room_id = r.id AND y.key_id = ?
		WHERE r.kind = 'direct' AND r.archived_at IS NULL LIMIT 1`, a, b).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// address returns keyID's address: name@domain, the bare handle with
// federation off.
func (s *Service) address(ctx context.Context, keyID int64) (string, error) {
	a, err := fedrooms.Address(ctx, s.DB, keyID, s.FedDomain)
	return strings.TrimSuffix(a, "@"), err
}

// limiter returns the new-conversation limiter of sender key keyID.
// ponytail: one per key, never evicted; keys are bounded by the table.
func (s *Service) limiter(keyID int64) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits == nil {
		s.limits = map[int64]*rate.Limiter{}
	}
	lim := s.limits[keyID]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(time.Hour/NewPerHour), NewPerHour)
		s.limits[keyID] = lim
	}
	return lim
}

func retryAfter(lim *rate.Limiter) string {
	rsv := lim.Reserve()
	delay := rsv.Delay()
	rsv.Cancel()
	return strconv.Itoa(int(max(delay, time.Second).Seconds()))
}

// refuse audits a refused start (reason: not a bot, policy, limit) and
// answers it.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, caller int64, to, reason string, status int, code, msg string) {
	detail, _ := json.Marshal(map[string]string{"to": to, "reason": reason})
	_ = audit.Write(r.Context(), s.DB, caller, "direct.refused", "bot", to, "", "error:"+code, string(detail))
	writeErr(w, r, status, code, msg)
}

type conversation struct {
	RoomID        string `json:"room_id"`
	Address       string `json:"address"` // the other bot's
	LastMessageAt string `json:"last_message_at,omitempty"`
	// Unread counts the other bot's messages after the last one the
	// caller read.
	Unread int `json:"unread"`
}

// list answers the caller's active conversations, the latest first.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, isBot, err := s.bot(r)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	case !isBot:
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) has direct conversations")
		return
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id, o.key_id,
		COALESCE((SELECT MAX(ts) FROM messages WHERE room_id = r.id AND deleted_at IS NULL), ''),
		(SELECT COUNT(*) FROM messages m WHERE m.room_id = r.id AND m.deleted_at IS NULL AND m.author_key_id != ?
		   AND m.id > COALESCE((SELECT last_read_id FROM direct_reads d WHERE d.room_id = r.id AND d.key_id = ?), 0))
		FROM rooms r
		JOIN room_members me ON me.room_id = r.id AND me.key_id = ?
		JOIN room_members o ON o.room_id = r.id AND o.key_id != ?
		WHERE r.kind = 'direct' AND r.archived_at IS NULL
		ORDER BY 3 DESC, r.id`, caller, caller, caller, caller)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
		return
	}
	out, others := []conversation{}, []int64{}
	for rows.Next() {
		var c conversation
		var other int64
		if err := rows.Scan(&c.RoomID, &other, &c.LastMessageAt, &c.Unread); err != nil {
			rows.Close()
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		out, others = append(out, c), append(others, other)
	}
	if err := rows.Close(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
		return
	}
	for i, k := range others {
		if out[i].Address, err = s.address(ctx, k); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "address lookup")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": r.Header.Get("X-Request-Id"),
	}})
}
