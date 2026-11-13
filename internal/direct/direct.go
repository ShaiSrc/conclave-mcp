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
//
// A recipient on a peer server: the conversation is a federated room
// (internal/federation), hosted on the server of the bot that started
// it. A new one is invited through InviteRemote, and the recipient's
// server checks its policy then; in a replica, Send forwards to the
// host.
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
	// InviteRemote asks the server of address (name@domain) to take its
	// bot into roomID, a direct room named name hosted here, with
	// rights; nil with federation off. It returns the most the peer
	// reads in one push. A refusal is a *fedrooms.Error.
	InviteRemote func(ctx context.Context, roomID, name, address string, rights []string) (int, error)

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
			s.startRemote(w, r, caller, name, d, body)
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

	roomName, err := pairName(from, toAddr)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	find := func() (string, error) { return s.find(ctx, caller, to) }
	roomID, err := find()
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
		if !s.allow(w, r, caller, toAddr) {
			return
		}
		if roomID, created, err = s.create(ctx, roomName, find, caller, to); err != nil {
			writeCreateErr(w, r, err)
			return
		}
	}
	s.post(w, r, caller, roomID, created, body, from, toAddr)
}

// startRemote finds or starts the conversation of bot caller with the
// bot name on server domain, an allowed peer, and posts the message of
// body to it. A new one is hosted here: the room is made, the peer is
// asked to take its bot in, which checks the bot's policy and its own
// limit, and on refusal the room is removed and the peer's status and
// code passed on.
func (s *Service) startRemote(w http.ResponseWriter, r *http.Request, caller int64, name, domain string, body []byte) {
	ctx := r.Context()
	handle, err := bots.NormalizeHandle(name)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "to: "+err.Error())
		return
	case s.InviteRemote == nil:
		writeErr(w, r, http.StatusNotFound, "federation_disabled", "federation is not enabled on this server")
		return
	}
	toAddr := handle + "@" + domain
	var allowed bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM federation_peers WHERE domain = ? AND status = 'allowed')`,
		domain).Scan(&allowed); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "peer lookup")
		return
	}
	if !allowed {
		writeErr(w, r, http.StatusForbidden, "peer_not_allowed", domain+" is not an allowed peer")
		return
	}
	from, err := s.address(ctx, caller)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "address lookup")
		return
	}
	roomName, err := pairName(from, toAddr)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	find := func() (string, error) { return s.findRemote(ctx, caller, toAddr, domain, roomName) }
	roomID, err := find()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "conversation lookup")
		return
	}
	created := roomID == ""
	if created {
		if !s.allow(w, r, caller, toAddr) {
			return
		}
		if roomID, created, err = s.create(ctx, roomName, find, caller); err != nil {
			writeCreateErr(w, r, err)
			return
		}
	}
	if created {
		if err := s.invite(ctx, roomID, roomName, toAddr); err != nil {
			s.drop(ctx, roomID)
			var fe *fedrooms.Error
			switch reason := map[int]string{http.StatusForbidden: "policy", http.StatusNotFound: "not a bot",
				http.StatusTooManyRequests: "limit"}; {
			case errors.As(err, &fe) && reason[fe.Status] != "":
				s.refuse(w, r, caller, toAddr, reason[fe.Status], fe.Status, fe.Code, fe.Message)
			case errors.As(err, &fe):
				writeErr(w, r, fe.Status, fe.Code, fe.Message)
			case errors.Is(err, fedrooms.ErrAddress):
				writeErr(w, r, http.StatusConflict, "conflict", err.Error())
			default:
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "invite")
			}
			return
		}
	}
	s.post(w, r, caller, roomID, created, body, from, toAddr)
}

// invite asks the server of address to take its bot into roomID, a new
// conversation named name, and then adds the bot's stand-in, which
// starts the room's event log.
func (s *Service) invite(ctx context.Context, roomID, name, address string) error {
	readLimit, err := s.InviteRemote(ctx, roomID, name, address, rightsOf)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key, err := fedrooms.StandIn(ctx, tx, address)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)`,
			roomID, key, storage.FormatTS(time.Now()), rightsOf.Storage())
	}
	if err == nil {
		err = fedrooms.LogJoin(ctx, tx, roomID, key, s.FedDomain, s.MaxMessageBytes, readLimit)
	}
	if err == nil {
		err = tx.Commit()
	}
	return err
}

// drop removes roomID, a conversation this start made: its members,
// messages and reads go with the room, its event log and delivery
// cursors here.
func (s *Service) drop(ctx context.Context, roomID string) {
	ctx = context.WithoutCancel(ctx)
	for _, q := range []string{`DELETE FROM fed_events WHERE room_id = ?`, `DELETE FROM fed_delivery WHERE room_id = ?`,
		`DELETE FROM rooms WHERE id = ?`} {
		_, _ = s.DB.ExecContext(ctx, q, roomID)
	}
}

// allow takes a token of caller's new-conversation limit, or answers
// 429 and returns false.
func (s *Service) allow(w http.ResponseWriter, r *http.Request, caller int64, to string) bool {
	lim := s.limiter(caller)
	if lim.Allow() {
		return true
	}
	w.Header().Set("Retry-After", retryAfter(lim))
	s.refuse(w, r, caller, to, "limit", http.StatusTooManyRequests, "rate_limited", "too many new conversations, try again later")
	return false
}

// post sends the message of body as caller's to roomID through the send
// path, which validates and stores it, and answers it; a conversation
// this start created is removed when its first message is refused.
func (s *Service) post(w http.ResponseWriter, r *http.Request, caller int64, roomID string, created bool, body []byte, from, to string) {
	ctx := r.Context()
	rec := httptest.NewRecorder()
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.Send(rec, r, roomID)
	if rec.Code != http.StatusCreated {
		if created {
			s.drop(ctx, roomID)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	if created {
		detail, _ := json.Marshal(map[string]string{"from": from, "to": to})
		_ = audit.Write(ctx, s.DB, caller, "direct.start", "room", roomID, roomID, "ok", string(detail))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"room_id": roomID, "message": json.RawMessage(rec.Body.Bytes())})
}

var (
	errLongName  = errors.New("the two addresses are too long for a conversation name")
	errNameTaken = errors.New("a room that is not a conversation of the two has its name")
)

// pairName is the name of the conversation of the bots at addresses a
// and b: dm:<a>~<b>, sorted, so the same on every server.
func pairName(a, b string) (string, error) {
	pair := []string{a, b}
	slices.Sort(pair)
	name := "dm:" + pair[0] + "~" + pair[1]
	// ponytail: a room name has at most 128 bytes, so two addresses with
	// long domains do not fit; derive a shorter unique name if that shows.
	if len(name) > 128 {
		return "", errLongName
	}
	return name, nil
}

func writeCreateErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNameTaken) {
		writeErr(w, r, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeErr(w, r, http.StatusInternalServerError, "internal_error", "create conversation")
}

// create makes the direct room name with members and returns it, and
// whether it made it: when the two started it meanwhile, find's room is
// returned.
func (s *Service) create(ctx context.Context, name string, find func() (string, error), members ...int64) (string, bool, error) {
	id, now := ulid.Make().String(), storage.FormatTS(time.Now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind) VALUES (?, ?, ?, ?, 'private', 'direct')`,
		id, name, members[0], now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") { // an active room has the name
		id, err := find()
		if err == nil && id == "" {
			err = errNameTaken
		}
		return id, false, err
	}
	for _, k := range members {
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

// findRemote returns the active direct room of bot me with the bot at
// address on server host, or "": one hosted here, or the replica of one
// host started. Either has the bot's stand-in as a member; a replica
// whose member events have not arrived yet is found by its name.
func (s *Service) findRemote(ctx context.Context, me int64, address, host, name string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT r.id FROM rooms r JOIN room_members me ON me.room_id = r.id AND me.key_id = ?
		WHERE r.kind = 'direct' AND r.archived_at IS NULL AND (r.home_domain IS NULL OR r.home_domain = ?)
		  AND (r.name = ? OR EXISTS (SELECT 1 FROM room_members o JOIN api_keys k ON k.id = o.key_id
		       WHERE o.room_id = r.id AND k.label = ? AND k.remote_domain = ?))
		ORDER BY r.id LIMIT 1`, me, host, name, address, host).Scan(&id)
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
