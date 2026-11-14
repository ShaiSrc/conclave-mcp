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
// host. A start checks the message before it makes a room or asks the
// peer; a failure after the peer took the conversation archives it
// through the room's event log, and an invite without a clear answer
// leaves the room pending, to be invited into again on the next start.
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
	// reads in one push. A refusal is a *fedrooms.Error; no answer is
	// one with status 502.
	InviteRemote func(ctx context.Context, roomID, name, address string, rights []string) (int, error)
	// FedNotify, when set, wakes the federation delivery worker after a
	// conversation was archived.
	FedNotify func()

	mu     sync.Mutex
	limits map[int64]*rate.Limiter
}

func (s *Service) Routes(r chi.Router) {
	r.Post("/direct", s.start)
	r.Get("/direct", s.list)
}

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
	// A message without a type would be stored as system, which direct
	// members may not send: default to question, as the CLI and MCP do.
	if body, err = withDefaultType(body); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if !isBot {
		s.refuse(w, r, caller, req.To[:min(len(req.To), 300)], "not a bot", http.StatusForbidden, "forbidden", // an address is shorter
			"only a bot (a key with a handle) has direct conversations")
		return
	}
	if !s.checkMessage(w, r, body) {
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

	roomName, err := fedrooms.DirectName(from, toAddr)
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
	var undo func()
	if roomID == "" {
		// A narrower policy keeps existing conversations; it stops new ones.
		if policy == bots.DMNone {
			s.refuse(w, r, caller, toAddr, "policy", http.StatusForbidden, "dm_refused", toAddr+" does not take direct conversations")
			return
		}
		if !s.allow(w, r, caller, toAddr) {
			return
		}
		created := false
		if roomID, created, err = s.create(ctx, roomName, find, caller, to); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "create conversation")
			return
		}
		if created {
			undo = func() { s.drop(ctx, roomID) }
		}
	}
	s.post(w, r, caller, roomID, undo, body, from, toAddr)
}

// startRemote finds or starts the conversation of bot caller with the
// bot name on server domain, an allowed peer, and posts the message of
// body to it. A new one is hosted here: the room is made, the peer is
// asked to take its bot in, which checks the bot's policy and its own
// limit, and on refusal the room is removed and the peer's status and
// code passed on. Without an answer the room stays pending; when the
// message fails after the peer took the bot in, it is archived.
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
	roomName, err := fedrooms.DirectName(from, toAddr)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	find := func() (string, error) { return s.findRemote(ctx, caller, domain, roomName) }
	roomID, err := find()
	pending := false
	if err == nil && roomID != "" {
		pending, err = s.pending(ctx, roomID)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "conversation lookup")
		return
	}
	if roomID == "" || pending {
		// A new conversation, or one whose invite had no clear answer: it
		// takes a token of the sender's limit, and the peer is asked
		// (again) to take its bot in, which it takes as the same invite.
		if !s.allow(w, r, caller, toAddr) {
			return
		}
		if roomID == "" {
			roomID, _, err = s.create(ctx, roomName, find, caller)
			if err == nil { // a concurrent start may have made it whole
				pending, err = s.pending(ctx, roomID)
			}
			if err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "create conversation")
				return
			}
		}
	}
	var undo func()
	if pending {
		readLimit, accepted, err := s.invite(ctx, roomID, roomName, toAddr)
		var fe *fedrooms.Error
		reason := map[int]string{http.StatusForbidden: "policy", http.StatusNotFound: "not a bot", http.StatusTooManyRequests: "limit"}
		switch {
		case err == nil:
			undo = func() { s.abandon(ctx, roomID, toAddr, readLimit) }
		case accepted: // the peer took it, the stand-in did not follow
			s.abandon(ctx, roomID, toAddr, readLimit)
			if errors.Is(err, fedrooms.ErrAddress) {
				writeErr(w, r, http.StatusConflict, "conflict", err.Error())
			} else {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "invite")
			}
			return
		case errors.As(err, &fe) && fe.Status == http.StatusBadGateway:
			// The peer may have taken it: the room stays pending.
			writeErr(w, r, fe.Status, fe.Code, fe.Message)
			return
		case errors.As(err, &fe) && reason[fe.Status] != "":
			s.drop(ctx, roomID)
			s.refuse(w, r, caller, toAddr, reason[fe.Status], fe.Status, fe.Code, fe.Message)
			return
		case errors.As(err, &fe):
			s.drop(ctx, roomID)
			writeErr(w, r, fe.Status, fe.Code, fe.Message)
			return
		default:
			s.drop(ctx, roomID)
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "invite")
			return
		}
	}
	s.post(w, r, caller, roomID, undo, body, from, toAddr)
}

// invite asks the server of address to take its bot into roomID, the
// conversation name, and then joins the bot's stand-in. It returns what
// the peer reads a push up to and whether it took the conversation.
func (s *Service) invite(ctx context.Context, roomID, name, address string) (readLimit int, accepted bool, err error) {
	if readLimit, err = s.InviteRemote(ctx, roomID, name, address, fedrooms.DirectRights); err != nil {
		return 0, false, err
	}
	return readLimit, true, s.join(ctx, roomID, address, readLimit, false)
}

// join adds the stand-in of address to roomID, which starts the room's
// event log with its snapshot, and keeps readLimit on the peer's
// delivery cursor; a room with a stand-in (a concurrent start added it)
// is left as it is. With archive it archives the room first, so the
// snapshot, or a room event when the log has started, archives the
// peer's replica too; a room another start posted in is left as it is.
func (s *Service) join(ctx context.Context, roomID, address string, readLimit int, archive bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if archive {
		res, err := tx.ExecContext(ctx, `UPDATE rooms SET archived_at = COALESCE(archived_at, ?) WHERE id = ? AND NOT `+hasMessage,
			storage.FormatTS(time.Now()), roomID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err // another start used the conversation: it stays
		}
	}
	logged, err := fedrooms.Logged(ctx, tx, roomID)
	switch {
	case err != nil:
	case logged && archive:
		err = fedrooms.LogRoom(ctx, tx, roomID, s.FedDomain)
	case !logged:
		var key int64
		key, err = fedrooms.StandIn(ctx, tx, address)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)`,
				roomID, key, storage.FormatTS(time.Now()), fedrooms.DirectRights.Storage())
		}
		if err == nil {
			err = fedrooms.LogJoin(ctx, tx, roomID, key, s.FedDomain, s.MaxMessageBytes, readLimit)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	return err
}

// abandon archives roomID, a conversation the peer took whose start
// failed after that, through its event log, so the peer's replica is
// archived too; one that another start posted in meanwhile stays. Best
// effort: a room it cannot archive stays pending, is invited into again
// on the next start, and the peer then archives an older replica of the
// pair.
func (s *Service) abandon(ctx context.Context, roomID, address string, readLimit int) {
	if s.join(context.WithoutCancel(ctx), roomID, address, readLimit, true) == nil && s.FedNotify != nil {
		s.FedNotify()
	}
}

// drop removes roomID, a conversation this start made that the peer
// refused or whose first message failed: its members and reads go with
// the room. It has no event log yet. One that a concurrent start used
// meanwhile, a stand-in or a message, is kept.
func (s *Service) drop(ctx context.Context, roomID string) {
	_, _ = s.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM rooms WHERE id = ? AND NOT `+hasStandIn+` AND NOT `+hasMessage,
		roomID)
}

// hasStandIn is whether room rooms.id has a member of another server.
const hasStandIn = `EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
	WHERE m.room_id = rooms.id AND k.remote_domain IS NOT NULL)`

// hasMessage is whether room rooms.id holds a message: a start's undo
// leaves a conversation alone once another start posted in it.
const hasMessage = `EXISTS (SELECT 1 FROM messages WHERE room_id = rooms.id)`

// pending reports whether roomID is a conversation hosted here whose
// invite had no clear answer: it has no member of another server yet.
func (s *Service) pending(ctx context.Context, roomID string) (bool, error) {
	var p bool
	err := s.DB.QueryRowContext(ctx, `SELECT home_domain IS NULL AND NOT `+hasStandIn+` FROM rooms WHERE id = ?`, roomID).Scan(&p)
	return p, err
}

// checkMessage answers 400 or 413 and returns false unless body holds a
// message that a member of a conversation may send: a message type
// other than system, some content, within the cap. A start checks it
// before it makes a room or asks a peer; the send path checks the rest.
func (s *Service) checkMessage(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var m struct {
		Content     string `json:"content"`
		MessageType string `json:"message_type"`
	}
	_ = json.Unmarshal(body, &m)
	switch {
	case m.MessageType == rights.Read || !slices.Contains(fedrooms.DirectRights, m.MessageType):
		writeErr(w, r, http.StatusBadRequest, "bad_request", "message_type must be a message type other than system")
	case m.Content == "":
		writeErr(w, r, http.StatusBadRequest, "bad_request", "content is required")
	case len(m.Content) > s.MaxMessageBytes:
		writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "content exceeds limit")
	default:
		return true
	}
	return false
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
// path, which stores it, and answers it. undo is set when this start
// made the room or brought the peer's bot in: it runs when the message
// fails, and a stored one is audited as the start.
func (s *Service) post(w http.ResponseWriter, r *http.Request, caller int64, roomID string, undo func(), body []byte, from, to string) {
	rec := httptest.NewRecorder()
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.Send(rec, r, roomID)
	if rec.Code != http.StatusCreated {
		if undo != nil {
			undo()
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	if undo != nil {
		detail, _ := json.Marshal(map[string]string{"from": from, "to": to})
		_ = audit.Write(r.Context(), s.DB, caller, "direct.start", "room", roomID, roomID, "ok", string(detail))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"room_id": roomID, "message": json.RawMessage(rec.Body.Bytes())})
}

// create makes the direct room name with members and returns it, and
// whether it made it: when the two started it meanwhile, find's room is
// returned. A room that is not theirs holding the name does not stop
// them: theirs gets the name with its id as a suffix, since a
// conversation is found by its pair, which is name.
func (s *Service) create(ctx context.Context, name string, find func() (string, error), members ...int64) (string, bool, error) {
	id, now := ulid.Make().String(), storage.FormatTS(time.Now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	insert := func(n string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, kind, pair)
			VALUES (?, ?, ?, ?, 'private', 'direct', ?)`, id, n, members[0], now, name)
		return err
	}
	err = insert(name)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") { // an active room has the name
		if other, err := find(); err != nil || other != "" {
			return other, false, err
		}
		err = insert(fedrooms.Suffixed(name, id)) // the room keeps name as its pair
	}
	for _, k := range members {
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)`,
				id, k, now, fedrooms.DirectRights.Storage())
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	return id, err == nil, err
}

// find returns the active direct room of bots a and b, both of this
// server, or "". Only a room hosted here counts: no host can make the
// conversation of two bots of ours its own.
func (s *Service) find(ctx context.Context, a, b int64) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT r.id FROM rooms r
		JOIN room_members x ON x.room_id = r.id AND x.key_id = ?
		JOIN room_members y ON y.room_id = r.id AND y.key_id = ?
		WHERE r.kind = 'direct' AND r.archived_at IS NULL AND r.home_domain IS NULL LIMIT 1`, a, b).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// findRemote returns the active direct room of bot me and pair, the
// pair's name, with a bot on server host, or "": one hosted here
// (pending or not), or the replica of one host started; the lowest id
// when both bots started one at once.
func (s *Service) findRemote(ctx context.Context, me int64, host, pair string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT r.id FROM rooms r JOIN room_members me ON me.room_id = r.id AND me.key_id = ?
		WHERE r.kind = 'direct' AND r.pair = ? AND r.archived_at IS NULL AND (r.home_domain IS NULL OR r.home_domain = ?)
		ORDER BY r.id LIMIT 1`, me, pair, host).Scan(&id)
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
	Remote        bool   `json:"remote"`  // the other bot is on another server
	LastMessageAt string `json:"last_message_at,omitempty"`
	// LastReadAt is the time of the last message the caller read, "" for
	// none: a reader asks for the messages since it.
	LastReadAt string `json:"last_read_at,omitempty"`
	// Unread counts the other bot's messages after the last one the
	// caller read; Actionable those of them that ask for something.
	Unread     int `json:"unread"`
	Actionable int `json:"actionable"`
}

// ActionableTypes are the message types that ask the reader for
// something, as agent.Actionable has them: the Stop hook reads a
// conversation only when one of them is unread.
var ActionableTypes = []string{"question", "task_assignment", "handoff", "alert", "decision"}

// unreadSQL counts the messages of room r.id that key ?1 has not read,
// in the order a room's messages are paged, by its direct_reads row d.
const unreadSQL = `SELECT COUNT(*) FROM messages m WHERE m.room_id = r.id AND m.deleted_at IS NULL AND m.author_key_id != ?1
	AND (m.ts, m.id) > (COALESCE(d.last_read_ts, ''), COALESCE(d.last_read_id, 0))`

// list answers the caller's active conversations, the latest first,
// each once.
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
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id, o.id, o.remote_domain IS NOT NULL,
		COALESCE((SELECT MAX(ts) FROM messages WHERE room_id = r.id AND deleted_at IS NULL), ''),
		COALESCE(d.last_read_ts, ''), (`+unreadSQL+`),
		(`+unreadSQL+` AND m.message_type IN ('`+strings.Join(ActionableTypes, "','")+`'))
		FROM rooms r
		JOIN room_members me ON me.room_id = r.id AND me.key_id = ?1
		LEFT JOIN direct_reads d ON d.room_id = r.id AND d.key_id = ?1
		JOIN api_keys o ON o.id = (SELECT MIN(key_id) FROM room_members WHERE room_id = r.id AND key_id != ?1)
		WHERE r.kind = 'direct' AND r.archived_at IS NULL
		ORDER BY 4 DESC, r.id`, caller)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
		return
	}
	out, others := []conversation{}, []int64{}
	for rows.Next() {
		var c conversation
		var other int64
		if err := rows.Scan(&c.RoomID, &other, &c.Remote, &c.LastMessageAt, &c.LastReadAt, &c.Unread, &c.Actionable); err != nil {
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

// withDefaultType sets message_type to question when the start body has
// none, keeping every other field as sent.
func withDefaultType(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("not an object")
	}
	var t string
	if raw, ok := fields["message_type"]; ok && json.Unmarshal(raw, &t) == nil && t != "" {
		return body, nil
	}
	fields["message_type"] = json.RawMessage(`"question"`)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
