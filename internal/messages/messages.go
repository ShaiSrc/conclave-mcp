// Package messages implements the messages endpoints from
// SPEC-0003 §4 and SPEC-0001 Q4 (256 KiB max).
//
// Endpoints:
//
//	POST /v1/rooms/{id}/messages  -- send (size-checked)
//	GET  /v1/rooms/{id}/messages  -- list paginated by since
//
// 404 to non-members and to archived rooms (no enumeration).
package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// clock is the time source for message timestamps, a seam for tests.
var clock = time.Now

type Service struct {
	DB              *sql.DB
	MaxMessageBytes int // 256 KiB default per SPEC-0001 Q4
	// Broker is the SSE event bus (ADR-0009). Optional: if
	// nil, the send handler still works but the SSE endpoint
	// sees no events. cmd/serve wires this in via
	// NewService(db, maxBytes, broker).
	Broker Broker
	// FedDomain is the federation domain, "" with federation off. With
	// it, a message in a room with remote members is written to the
	// room's event log in the transaction that stores it.
	FedDomain string
	// FedNotify, when set, is called after each send: it wakes the
	// federation delivery worker.
	FedNotify func()
	// Forward, when set, sends a message of key author in replica room
	// roomID to the room's host and stores the copy the host answers,
	// as StoreReplica does; it returns the copy's id. m's metadata is
	// in its wire form and its reply_to is the host's id. A refusal,
	// the host's included, is a *fedrooms.Error. Set by the server with
	// federation on.
	Forward func(ctx context.Context, roomID string, author int64, m fedrooms.MessagePayload) (int64, error)

	// stampMu makes taking a timestamp and inserting one step, and
	// lastTS keeps timestamps strictly increasing, so ts order is
	// insert order and a reader's `since` cursor never passes a
	// message that is not stored yet. lastTS starts from the newest
	// stored ts, so a restart with a clock that went back keeps the
	// order too.
	// ponytail: one lock for all rooms; per-room locks if send
	// throughput ever matters.
	stampMu sync.Mutex
	lastTS  time.Time
}

// Broker is the subset of *events.Broker the messages
// service needs. The interface keeps the messages package
// free of an events-package import cycle (server wires
// both; messages only consumes).
type Broker interface {
	Publish(roomID string, payload []byte) int
}

func NewService(db *sql.DB, maxBytes int, broker Broker) *Service {
	if maxBytes <= 0 {
		maxBytes = 262144
	}
	return &Service{DB: db, MaxMessageBytes: maxBytes, Broker: broker}
}

func (s *Service) Routes(r chi.Router) {
	r.Post("/rooms/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		s.Send(w, r, chi.URLParam(r, "id"))
	})
	r.Get("/rooms/{id}/messages", s.list)
	// Soft-delete (SPEC-0001 Q8). Admin keys only; delete() answers
	// 403 forbidden to every other caller.
	r.Delete("/rooms/{id}/messages/{msgid}", s.delete)
}

type sendRequest struct {
	Content     string          `json:"content"`
	ContentType string          `json:"content_type"`
	MessageType string          `json:"message_type"` // optional, SPEC-0001 Q7
	Metadata    json.RawMessage `json:"metadata"`     // optional, raw JSON
	// ReplyTo is the id of the message this one answers, in the same
	// room. Stored as metadata.reply_to, so readers find it there.
	ReplyTo *int64 `json:"reply_to"`
}

type messageView struct {
	ID          int64  `json:"id"`
	RoomID      string `json:"room_id"`
	AuthorKeyID int64  `json:"author_key_id"`
	TS          string `json:"ts"`
	ContentType string `json:"content_type"`
	// Size is the byte length of the content payload
	// (SPEC-0002 §3.5.1). It is computed at response
	// time from len(Content), not stored in the DB.
	// Earlier versions omitted this field; the CLI
	// rendered "0" in the size column. Smoke test on
	// 2026-06-28 caught the drift.
	Size        int             `json:"size"`
	Content     string          `json:"content"`
	MessageType string          `json:"message_type,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	DeletedAt   string          `json:"deleted_at,omitempty"`
	TrustOrigin string          `json:"trust_origin"`
	// Operator is set by the server for messages the operator sent from
	// the web UI with a passkey; no request field can set it.
	Operator bool `json:"operator,omitempty"`
	// AuthorAddress is the address (name@domain) of an author on
	// another server, its stand-in key's label.
	AuthorAddress string `json:"author_address,omitempty"`
	// Unverified marks a message of a replica room whose author
	// signature could not be checked: its server's key is unknown here.
	Unverified bool `json:"unverified,omitempty"`
}

// allowedMessageTypes is the set of intent tags from
// SPEC-0002 §3.5.1. The set is enforced at the application
// layer; the SQL trigger in migration 0003 is a backstop.
var allowedMessageTypes = map[string]struct{}{
	"question":        {},
	"answer":          {},
	"decision":        {},
	"handoff":         {},
	"status_update":   {},
	"task_assignment": {},
	"alert":           {},
	"system":          {},
}

// Send stores the request body as a message of the caller in roomID,
// as POST /v1/rooms/{id}/messages does.
func (s *Service) Send(w http.ResponseWriter, r *http.Request, roomID string) {
	s.post(w, r, roomID, false, "", "")
}

// SendAsOperator stores the request body as an operator message. It is
// for the UI's passkey-confirmed path only: nothing else may call it.
func (s *Service) SendAsOperator(w http.ResponseWriter, r *http.Request, roomID string) {
	s.post(w, r, roomID, true, "", "")
}

// SendFromPeer stores the request body as a message of the stand-in key
// in r's claims, with originSig, the author server's signature, and
// the nonce it covers. It is for the federation listener's message
// proposals only, after the signature was checked.
func (s *Service) SendFromPeer(w http.ResponseWriter, r *http.Request, roomID, originSig, nonce string) {
	s.post(w, r, roomID, false, originSig, nonce)
}

// post handles a send: validates the body and reply_to, stores the
// message and publishes it to subscribers. operator marks the message
// as the operator's; UI sessions may not post unmarked. originSig and
// nonce are stored with a remote author's message.
func (s *Service) post(w http.ResponseWriter, r *http.Request, roomID string, operator bool, originSig, nonce string) {
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	if c, _ := auth.FromContext(r.Context()); !operator && slices.Contains(c.Scopes, auth.ScopeUI) {
		writeErr(w, r, http.StatusForbidden, "forbidden", "operator messages need a passkey confirmation: send them from the web UI")
		return
	}

	// Size guard at HTTP layer (defence in depth: also enforced by
	// CHECK constraint in migration 0001, but the constraint allows
	// up to 2^31 bytes; the configurable cap is the operational
	// limit).
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.MaxMessageBytes+1024))

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// MaxBytesError returns a *http.MaxBytesError
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "request too large")
			return
		}
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if len(req.Content) > s.MaxMessageBytes {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "content exceeds limit")
		return
	}
	if req.ContentType == "" {
		req.ContentType = "text/plain"
	}
	if !contentTypes[req.ContentType] {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "unsupported content_type")
		return
	}
	// SPEC-0001 Q7: optional intent tag. Empty / missing
	// is allowed and is stored as system (see below). The trigger in
	// migration 0003 rejects unknown values; we pre-validate
	// here for a friendlier 400.
	if req.MessageType != "" {
		if _, ok := allowedMessageTypes[req.MessageType]; !ok {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "unsupported message_type")
			return
		}
	}
	// SPEC-0002 §3.5.1: metadata is an optional JSON object.
	// We accept any valid JSON; null/empty = "no metadata".
	var metadata sql.NullString
	if len(req.Metadata) > 0 {
		// Reject non-objects at the application layer. We
		// accept null and any other JSON value (the spec
		// says "JSON blob", not strictly object), but
		// enforce a top-level object for type-specific
		// fields as documented.
		var probe any
		if err := json.Unmarshal(req.Metadata, &probe); err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "metadata must be valid JSON")
			return
		}
		if probe != nil {
			if _, isObj := probe.(map[string]any); !isObj {
				writeErr(w, r, http.StatusBadRequest, "bad_request", "metadata must be a JSON object")
				return
			}
		}
		metadata = sql.NullString{String: string(req.Metadata), Valid: true}
	}

	// The message type is the right the caller needs; a message
	// without a type is stored as system (SPEC-0001 Q7). The caller's
	// trust_origin is denormalised onto the message (SPEC-0001 Q11).
	messageType := req.MessageType
	if messageType == "" {
		messageType = rights.System
	}
	mem, ok := rights.Require(w, r, s.DB, roomID, messageType)
	if !ok {
		return
	}
	if mem.Archived {
		writeErr(w, r, http.StatusGone, "not_found", "room is archived")
		return
	}
	callerTrustOrigin := mem.TrustOrigin

	// A reply_to inside metadata gets the same check as the field (the
	// field wins when both are set).
	if req.ReplyTo == nil && metadata.Valid {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(metadata.String), &fields)
		if raw, ok := fields["reply_to"]; ok {
			var id int64
			if err := json.Unmarshal(raw, &id); err != nil {
				writeErr(w, r, http.StatusBadRequest, "bad_request", "reply_to must be a message id")
				return
			}
			req.ReplyTo = &id
		}
	}
	var hostReply *int64 // in a replica room, the host's id of the message replied to
	if req.ReplyTo != nil {
		var remote sql.NullInt64
		err := s.DB.QueryRowContext(r.Context(),
			`SELECT remote_id FROM messages WHERE id = ? AND room_id = ? AND deleted_at IS NULL`, *req.ReplyTo, roomID).Scan(&remote)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "reply_to: no message "+strconv.FormatInt(*req.ReplyTo, 10)+" in this room (or it was deleted)")
			return
		}
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "reply_to lookup")
			return
		}
		if mem.Home != "" {
			if !remote.Valid {
				writeErr(w, r, http.StatusBadRequest, "bad_request", "reply_to: message "+strconv.FormatInt(*req.ReplyTo, 10)+" is not one of the host's")
				return
			}
			hostReply = &remote.Int64
		}
		// RawMessage values keep the other fields byte for byte (no
		// float64 rounding of big numbers).
		var meta map[string]json.RawMessage
		if metadata.Valid {
			_ = json.Unmarshal([]byte(metadata.String), &meta) // validated above
		}
		if meta == nil { // no metadata, or "null"
			meta = map[string]json.RawMessage{}
		}
		meta["reply_to"] = json.RawMessage(strconv.FormatInt(*req.ReplyTo, 10))
		b, _ := fedrooms.Marshal(meta) // <, > and & as sent: the wire form of a forwarded reply is derived from it
		metadata = sql.NullString{String: string(b), Valid: true}
		req.Metadata = b
	}
	if mem.Home != "" {
		s.forward(w, r, roomID, caller, req, messageType, metadata, hostReply)
		return
	}

	s.stampMu.Lock()
	now, err := s.stamp(r.Context())
	if err != nil {
		s.stampMu.Unlock()
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "read newest timestamp")
		return
	}
	id, err := s.insert(r.Context(), roomID, caller, now, req.Content, req.ContentType,
		messageType, metadata, callerTrustOrigin, operator, nullableString(originSig), nullableString(nonce))
	s.stampMu.Unlock()
	if errors.Is(err, fedrooms.ErrEventTooLarge) {
		writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "message too large for the room's other servers")
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "insert message")
		return
	}
	if s.FedNotify != nil {
		s.FedNotify()
	}
	detail := `{"room_id":` + strconvQuote(roomID)
	if operator {
		detail += `,"operator":true`
	}
	_ = audit.Write(r.Context(), s.DB, caller, "message.send", "message", strconv.FormatInt(id, 10), roomID, "ok", detail+`}`)
	// Build the response view once; we use the same shape
	// for the HTTP response and the SSE event payload
	// (ADR-0009). Failure to publish is non-fatal: the
	// message is already in the DB and the audit row
	// exists; live subscribers just miss this one event.
	mv := messageView{
		ID:          id,
		RoomID:      roomID,
		AuthorKeyID: caller,
		TS:          now,
		ContentType: req.ContentType,
		Content:     req.Content,
		// Size is computed at response time (SPEC-0002
		// §3.5.1: derived from len(content), not stored).
		Size:          len(req.Content),
		TrustOrigin:   callerTrustOrigin,
		Operator:      operator,
		AuthorAddress: mem.Address,
	}
	if req.MessageType != "" {
		mv.MessageType = req.MessageType
	}
	if len(req.Metadata) > 0 {
		mv.Metadata = req.Metadata
	}
	if s.Broker != nil {
		if payload, perr := json.Marshal(mv); perr == nil {
			s.Broker.Publish(roomID, payload)
		}
	}
	writeJSON(w, http.StatusCreated, mv)
}

// forward sends a checked message of a replica room to the room's
// host, which checks and stores it, and answers with the copy stored
// here, as a send answers. reply is the host's id of the message
// replied to.
func (s *Service) forward(w http.ResponseWriter, r *http.Request, roomID string, caller int64, req sendRequest,
	messageType string, metadata sql.NullString, reply *int64) {
	if s.Forward == nil {
		writeErr(w, r, http.StatusForbidden, "replica_room", "room is hosted on another server, and federation is off")
		return
	}
	wire, _, _ := fedrooms.SplitMetadata([]byte(metadata.String)) // an object or null: checked above
	id, err := s.Forward(r.Context(), roomID, caller, fedrooms.MessagePayload{ContentType: req.ContentType,
		MessageType: messageType, Content: req.Content, Metadata: wire, ReplyTo: reply})
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		writeErr(w, r, fe.Status, fe.Code, fe.Message)
		return
	}
	var mv messageView
	if err == nil {
		mv, err = scanView(s.DB.QueryRowContext(r.Context(), `SELECT `+viewColumns+` FROM messages WHERE id = ?`, id))
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "forward message")
		return
	}
	_ = audit.Write(r.Context(), s.DB, caller, "message.send", "message", strconv.FormatInt(id, 10), roomID, "ok",
		`{"room_id":`+strconvQuote(roomID)+`}`)
	writeJSON(w, http.StatusCreated, mv)
}

// viewColumns are the columns scanView reads.
const viewColumns = `id, room_id, author_key_id, ts, content_type, content, message_type, metadata, deleted_at, trust_origin, operator,
	unverified, COALESCE((SELECT k.label FROM api_keys k WHERE k.id = author_key_id AND k.remote_domain IS NOT NULL), '')`

// scanView reads a message's viewColumns.
func scanView(row interface{ Scan(...any) error }) (messageView, error) {
	var (
		mv                     messageView
		mtype, meta, deletedAt sql.NullString
	)
	if err := row.Scan(&mv.ID, &mv.RoomID, &mv.AuthorKeyID, &mv.TS, &mv.ContentType, &mv.Content, &mtype, &meta, &deletedAt,
		&mv.TrustOrigin, &mv.Operator, &mv.Unverified, &mv.AuthorAddress); err != nil {
		return mv, err
	}
	// Size is computed at response time (SPEC-0002 §3.5.1: derived
	// from len(content), not stored).
	mv.Size = len(mv.Content)
	mv.MessageType, mv.DeletedAt = mtype.String, deletedAt.String
	if meta.String != "" {
		mv.Metadata = json.RawMessage(meta.String)
	}
	return mv, nil
}

// stamp returns the timestamp of the next message: now, or just after
// the last one when the clock has not moved past it. The caller holds
// stampMu until the message is stored.
func (s *Service) stamp(ctx context.Context) (string, error) {
	if s.lastTS.IsZero() { // first message since start: continue after what is stored
		var newest sql.NullString
		err := s.DB.QueryRowContext(ctx, `SELECT MAX(ts) FROM messages`).Scan(&newest)
		if err == nil && newest.Valid { // NULL: no messages yet
			s.lastTS, err = time.Parse(time.RFC3339Nano, newest.String)
		}
		if err != nil { // lastTS stays zero, so the next message tries again
			return "", err
		}
	}
	t := clock().UTC()
	if !t.After(s.lastTS) {
		t = s.lastTS.Add(time.Nanosecond) // ts has nanosecond precision
	}
	s.lastTS = t
	return storage.FormatTS(t), nil
}

// contentTypes are the content types a message may have.
var contentTypes = map[string]bool{"text/plain": true, "text/markdown": true, "application/json": true}

// StoreReplica stores m, a message of replica room roomID that its host
// sent, and publishes it to subscribers as a send does. It is stamped
// with a local timestamp, so readers' since cursors see it; its trust
// origin is its author key's. A message whose host id is stored already
// is left as it is, and its id returned. A message a send would refuse
// (type, content type, over the cap) is a *fedrooms.Error.
func (s *Service) StoreReplica(ctx context.Context, roomID string, m fedrooms.ReplicaMessage) (int64, error) {
	if _, ok := allowedMessageTypes[m.MessageType]; !ok || !contentTypes[m.ContentType] {
		return 0, &fedrooms.Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "message type or content type not accepted"}
	}
	if len(m.Content) > s.MaxMessageBytes {
		return 0, &fedrooms.Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large",
			Message: fmt.Sprintf("content of %d bytes is over the message cap of %d", len(m.Content), s.MaxMessageBytes)}
	}
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM messages WHERE room_id = ? AND remote_id = ?`, roomID, m.RemoteID).Scan(&id)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	var trust, address string
	if err := s.DB.QueryRowContext(ctx, `SELECT trust_origin, CASE WHEN remote_domain IS NULL THEN '' ELSE label END
		FROM api_keys WHERE id = ?`, m.Author).Scan(&trust, &address); err != nil {
		return 0, err
	}
	var metadata sql.NullString
	if len(m.Metadata) > 0 {
		metadata = sql.NullString{String: string(m.Metadata), Valid: true}
	}
	s.stampMu.Lock()
	now, err := s.stamp(ctx)
	if err == nil {
		var res sql.Result
		res, err = s.DB.ExecContext(ctx, `INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type,
			metadata, trust_origin, operator, origin_sig, remote_id, unverified) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
			roomID, m.Author, now, m.Content, m.ContentType, m.MessageType, metadata, trust, nullableString(m.OriginSig), m.RemoteID, m.Unverified)
		if err == nil {
			id, err = res.LastInsertId()
		}
	}
	s.stampMu.Unlock()
	if err != nil {
		return 0, err
	}
	if s.Broker != nil {
		mv := messageView{ID: id, RoomID: roomID, AuthorKeyID: m.Author, TS: now, ContentType: m.ContentType,
			Size: len(m.Content), Content: m.Content, MessageType: m.MessageType, Metadata: m.Metadata, TrustOrigin: trust,
			AuthorAddress: address, Unverified: m.Unverified}
		if payload, err := json.Marshal(mv); err == nil {
			s.Broker.Publish(roomID, payload)
		}
	}
	return id, nil
}

// insert stores one message, the columns after room_id in args, and
// logs it in the same transaction. It returns the message's id.
func (s *Service) insert(ctx context.Context, roomID string, args ...any) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, metadata, trust_origin, operator,
		 origin_sig, origin_nonce) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{roomID}, args...)...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := fedrooms.LogMessage(ctx, tx, roomID, id, s.FedDomain, s.MaxMessageBytes); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	roomID := chi.URLParam(r, "id")

	// 404 to non-members, 403 missing_right without read.
	if _, ok := rights.Require(w, r, s.DB, roomID, rights.Read); !ok {
		return
	}

	// Query parameters. SPEC-0003 §4.2: `since` is an
	// ISO-8601 timestamp; `limit` defaults to 50 and is
	// capped at 500.
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "limit must be positive integer")
			return
		}
		if n > 500 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "limit must be <= 500")
			return
		}
		limit = n
	}
	sinceTS := q.Get("since") // RFC 3339; empty = from beginning
	if sinceTS != "" {
		var err error
		if sinceTS, err = storage.NormalizeTS(sinceTS); err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "since must be an RFC 3339 timestamp")
			return
		}
	}
	// SPEC-0001 Q11 / ADR-0008: optional trust_origin filter.
	// Validated against the closed set before the SQL CHECK
	// fires (so we return 400, not 500).
	trustOrigin := q.Get("trust_origin")
	trustOriginFilter := ""
	if trustOrigin != "" {
		if trustOrigin != "internal" && trustOrigin != "external" {
			writeErr(w, r, http.StatusBadRequest, "bad_request",
				"trust_origin must be 'internal' or 'external'")
			return
		}
		trustOriginFilter = trustOrigin
	}
	_ = trustOriginFilter // used in the query below; the variable
	// name is kept for clarity even though modernc.org/sqlite
	// parameter binding does not need a separate name.

	// Soft-deleted messages are filtered out (SPEC-0001 Q8).
	// Build the query with optional trust_origin filter. We
	// branch on whether the filter is present so the
	// prepared-statement shape stays simple; both branches
	// are indexed (room_id, ts) and (room_id, trust_origin,
	// ts WHERE deleted_at IS NULL).
	var (
		rows *sql.Rows
		err  error
	)
	if trustOrigin != "" {
		rows, err = s.DB.QueryContext(r.Context(),
			`SELECT `+viewColumns+`
			 FROM messages
			 WHERE room_id = ? AND ts > ? AND deleted_at IS NULL AND trust_origin = ?
			 ORDER BY ts ASC, id ASC
			 LIMIT ?`,
			roomID, sinceTS, trustOrigin, limit,
		)
	} else {
		rows, err = s.DB.QueryContext(r.Context(),
			`SELECT `+viewColumns+`
			 FROM messages
			 WHERE room_id = ? AND ts > ? AND deleted_at IS NULL
			 ORDER BY ts ASC, id ASC
			 LIMIT ?`,
			roomID, sinceTS, limit,
		)
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
		return
	}
	defer rows.Close()

	var out []messageView
	for rows.Next() {
		mv, err := scanView(rows)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		out = append(out, mv)
	}
	// next_since is the ISO-8601 ts of the last message we
	// returned; the client uses it as the next `since`.
	// Empty when no messages were returned.
	var nextSince string
	if n := len(out); n > 0 {
		nextSince = out[n-1].TS
	}
	// A member reading a direct room has read up to the page's last
	// message, in the page's (ts, id) order, for its unread count (GET
	// /v1/direct). A filtered read skips messages, so it marks nothing.
	// The list stands without it.
	if n := len(out); n > 0 && trustOrigin == "" {
		_, _ = s.DB.ExecContext(r.Context(), `INSERT INTO direct_reads(room_id, key_id, last_read_ts, last_read_id)
			SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM rooms WHERE id = ? AND kind = 'direct')
			AND EXISTS (SELECT 1 FROM room_members WHERE room_id = ? AND key_id = ?)
			ON CONFLICT(room_id, key_id) DO UPDATE SET last_read_ts = excluded.last_read_ts, last_read_id = excluded.last_read_id
			WHERE (excluded.last_read_ts, excluded.last_read_id) > (last_read_ts, last_read_id)`,
			roomID, caller, out[n-1].TS, out[n-1].ID, roomID, roomID, caller)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":   out,
		"next_since": nextSince,
	})
}

// delete soft-deletes a single message (SPEC-0001 Q8). Admin keys
// only; they need not be members. The row is kept (with
// deleted_at set) for audit; the GET /messages list endpoint filters it out.
//
// Idempotent: a second DELETE on an already-deleted message
// returns 404 because the row is no longer visible.
func (s *Service) delete(w http.ResponseWriter, r *http.Request) {
	caller := callerKeyID(r)
	if caller == 0 {
		writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no auth claims")
		return
	}
	roomID := chi.URLParam(r, "id")
	msgIDStr := chi.URLParam(r, "msgid")
	msgID, err := strconv.ParseInt(msgIDStr, 10, 64)
	if err != nil || msgID <= 0 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "msgid must be positive integer")
		return
	}

	if !auth.IsAdmin(r.Context()) {
		writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
		return
	}
	var archived, home sql.NullString
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT archived_at, home_domain FROM rooms WHERE id = ?`, roomID,
	).Scan(&archived, &home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
			return
		}
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "room lookup")
		return
	}
	if archived.Valid {
		writeErr(w, r, http.StatusGone, "not_found", "room is archived")
		return
	}
	if home.Valid {
		writeErr(w, r, http.StatusForbidden, "replica_room", "room is hosted by "+home.String+"; change it there")
		return
	}

	// Soft-delete. We use a subquery for the WHERE so we
	// also confirm the message belongs to this room (defence
	// in depth: an owner of room A cannot delete a message
	// in room B by guessing the msgid). Its events in the room's
	// federation log are retracted in the same transaction.
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "begin tx")
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(r.Context(),
		`UPDATE messages
		 SET deleted_at = ?
		 WHERE id = ? AND room_id = ? AND deleted_at IS NULL`,
		now, msgID, roomID,
	)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "delete message")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, r, http.StatusNotFound, "not_found", "message not found")
		return
	}
	if err := fedrooms.LogDelete(r.Context(), tx, roomID, msgID); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "event log")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "commit")
		return
	}
	if s.FedNotify != nil {
		s.FedNotify()
	}
	_ = audit.Write(r.Context(), s.DB, caller, "message.delete", "message", strconv.FormatInt(msgID, 10), roomID, "ok",
		`{"room_id":`+strconvQuote(roomID)+`}`)
	w.WriteHeader(http.StatusNoContent)
}

func callerKeyID(r *http.Request) int64 {
	c, ok := auth.FromContext(r.Context())
	if !ok {
		return 0
	}
	return c.KeyID
}

func parseIntOrZero(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// strconvQuote JSON-stringifies a Go string for the audit
// detail blob. See internal/rooms/rooms.go for the rationale.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nullableString turns "" into a NULL sql.NullString so the
// SQL driver writes NULL rather than the empty string. Used
// for optional text columns.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
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
