package fedrooms

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Event types of a hosted room's log (fed_events).
const (
	EventMessage      = "message"
	EventMember       = "member"        // added, or rights changed
	EventMemberRemove = "member.remove" // removed or left
	EventRoom         = "room"          // name, archived
)

// SnapshotMessages is how many of the latest messages a room's log
// starts with when the room gets its first remote member.
const SnapshotMessages = 200

// Event is one entry of a room's log as it goes over the wire.
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// RoomPayload is the payload of a room event.
type RoomPayload struct {
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
}

// MemberPayload is the payload of a member event.
type MemberPayload struct {
	Address string   `json:"address"`
	Rights  []string `json:"rights"`
}

// RemovePayload is the payload of a member.remove event.
type RemovePayload struct {
	Address string `json:"address"`
}

// MessagePayload is the payload of a message event. It never carries
// the operator mark or the trust origin: those are local. Metadata is
// in its wire form (SplitMetadata) and ReplyTo is the host's id;
// OriginSig is the author server's signature, empty for authors on the
// host.
type MessagePayload struct {
	ID          int64           `json:"id"`
	Author      string          `json:"author"`
	TS          string          `json:"ts"`
	ContentType string          `json:"content_type"`
	MessageType string          `json:"message_type"`
	Content     string          `json:"content"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	ReplyTo     *int64          `json:"reply_to,omitempty"`
	OriginSig   string          `json:"origin_sig,omitempty"`
}

// Error is a refusal that the API answers with its status and code.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return e.Message }

// Logged reports whether roomID's changes go to its event log: it has
// a stand-in member.
func Logged(ctx context.Context, q DB, roomID string) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.remote_domain IS NOT NULL)`, roomID).Scan(&ok)
	return ok, err
}

// The Log functions write a change of roomID to its log, in the
// transaction tx of the change. They run after the change's own write,
// so tx holds SQLite's write lock and the next seq, MAX(seq)+1, cannot
// be taken by anyone else. With domain "" (federation off) they do
// nothing. Admin keys are left out of member events, as the members
// list leaves them out for other keys.

// LogJoin logs that keyID joined roomID: the room's snapshot when keyID
// is its first stand-in member, else keyID's member event. A stand-in's
// server gets a delivery cursor.
func LogJoin(ctx context.Context, tx *sql.Tx, roomID string, keyID int64, domain string) error {
	if domain == "" {
		return nil
	}
	var remote sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT remote_domain FROM api_keys WHERE id = ?`, keyID).Scan(&remote); err != nil {
		return err
	}
	if remote.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fed_delivery(room_id, peer) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			roomID, remote.String); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM room_members m JOIN api_keys k ON k.id = m.key_id
			WHERE m.room_id = ? AND k.remote_domain IS NOT NULL`, roomID).Scan(&n); err != nil {
			return err
		}
		if n == 1 {
			return snapshot(ctx, tx, roomID, domain)
		}
	}
	return LogMember(ctx, tx, roomID, keyID, domain)
}

// LogMember logs keyID's membership of roomID with its current rights.
func LogMember(ctx context.Context, tx *sql.Tx, roomID string, keyID int64, domain string) error {
	if ok, err := logged(ctx, tx, roomID, domain); !ok {
		return err
	}
	return logMember(ctx, tx, roomID, keyID, domain)
}

// LogRemove logs that keyID left roomID. It runs after the delete; the
// room is logged when it still has a stand-in or keyID was one, so the
// last remote member's removal reaches its server.
func LogRemove(ctx context.Context, tx *sql.Tx, roomID string, keyID int64, domain string) error {
	if domain == "" {
		return nil
	}
	var admin, remote bool
	if err := tx.QueryRowContext(ctx, `SELECT is_admin, remote_domain IS NOT NULL FROM api_keys WHERE id = ?`, keyID).
		Scan(&admin, &remote); err != nil {
		return err
	}
	if ok, err := Logged(ctx, tx, roomID); err != nil || admin || !ok && !remote {
		return err
	}
	addr, err := Address(ctx, tx, keyID, domain)
	if err != nil {
		return err
	}
	return appendEvent(ctx, tx, roomID, EventMemberRemove, RemovePayload{Address: addr})
}

// LogRoom logs roomID's name and archived state.
func LogRoom(ctx context.Context, tx *sql.Tx, roomID, domain string) error {
	if ok, err := logged(ctx, tx, roomID, domain); !ok {
		return err
	}
	return logRoom(ctx, tx, roomID)
}

// LogMessage logs message msgID of roomID.
func LogMessage(ctx context.Context, tx *sql.Tx, roomID string, msgID int64, domain string) error {
	if ok, err := logged(ctx, tx, roomID, domain); !ok {
		return err
	}
	return logMessage(ctx, tx, roomID, msgID, domain)
}

func logged(ctx context.Context, tx *sql.Tx, roomID, domain string) (bool, error) {
	if domain == "" {
		return false, nil
	}
	return Logged(ctx, tx, roomID)
}

// snapshot starts roomID's log: the room, its members but admin keys,
// and its latest 200 messages that are not deleted, oldest first.
func snapshot(ctx context.Context, tx *sql.Tx, roomID, domain string) error {
	if err := logRoom(ctx, tx, roomID); err != nil {
		return err
	}
	members, err := ids(ctx, tx, `SELECT m.key_id FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.is_admin = 0 ORDER BY m.key_id`, roomID)
	if err != nil {
		return err
	}
	for _, id := range members {
		if err := logMember(ctx, tx, roomID, id, domain); err != nil {
			return err
		}
	}
	msgs, err := ids(ctx, tx, `SELECT id FROM (SELECT id, ts FROM messages WHERE room_id = ? AND deleted_at IS NULL
		ORDER BY ts DESC, id DESC LIMIT ?) ORDER BY ts, id`, roomID, SnapshotMessages)
	if err != nil {
		return err
	}
	for _, id := range msgs {
		if err := logMessage(ctx, tx, roomID, id, domain); err != nil {
			return err
		}
	}
	return nil
}

// ids reads a column of ids. The rows are closed before the caller runs
// its next statement on tx.
func ids(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func logRoom(ctx context.Context, tx *sql.Tx, roomID string) error {
	var p RoomPayload
	if err := tx.QueryRowContext(ctx, `SELECT name, archived_at IS NOT NULL FROM rooms WHERE id = ?`, roomID).
		Scan(&p.Name, &p.Archived); err != nil {
		return err
	}
	return appendEvent(ctx, tx, roomID, EventRoom, p)
}

func logMember(ctx context.Context, tx *sql.Tx, roomID string, keyID int64, domain string) error {
	var admin bool
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT k.is_admin, m.rights FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND m.key_id = ?`, roomID, keyID).Scan(&admin, &stored)
	if err != nil || admin {
		return err
	}
	addr, err := Address(ctx, tx, keyID, domain)
	if err != nil {
		return err
	}
	return appendEvent(ctx, tx, roomID, EventMember, MemberPayload{Address: addr, Rights: rights.FromStorage(stored)})
}

func logMessage(ctx context.Context, tx *sql.Tx, roomID string, msgID int64, domain string) error {
	var (
		p         MessagePayload
		author    int64
		meta, sig sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `SELECT id, author_key_id, ts, content_type, COALESCE(message_type, 'system'),
		content, metadata, origin_sig FROM messages WHERE id = ? AND room_id = ?`, msgID, roomID).
		Scan(&p.ID, &author, &p.TS, &p.ContentType, &p.MessageType, &p.Content, &meta, &sig); err != nil {
		return err
	}
	var err error
	if p.Author, err = Address(ctx, tx, author, domain); err != nil {
		return err
	}
	if p.Metadata, p.ReplyTo, err = SplitMetadata([]byte(meta.String)); err != nil {
		return err
	}
	p.OriginSig = sig.String
	return appendEvent(ctx, tx, roomID, EventMessage, p)
}

// appendEvent writes payload as roomID's next event, numbered
// MAX(seq)+1 in the same statement.
func appendEvent(ctx context.Context, tx *sql.Tx, roomID, typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fed_events(room_id, seq, type, payload, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM fed_events WHERE room_id = ?`,
		roomID, typ, string(b), storage.FormatTS(time.Now()), roomID)
	return err
}

// errNotObject is SplitMetadata's error for metadata that is not a JSON
// object or null.
var errNotObject = errors.New("metadata must be a JSON object")

// SplitMetadata returns a message's metadata in its wire form, and the
// reply_to it holds, if that is an id. The wire form leaves reply_to
// out, since it travels in its own field, and is re-encoded compactly
// with sorted keys: the author's server, signing the metadata it sends,
// and the host, logging the metadata it stored with reply_to merged
// in, derive the same bytes. No metadata, null and {} have an empty
// wire form.
func SplitMetadata(raw []byte) ([]byte, *int64, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, errNotObject
	}
	var reply *int64
	if r, ok := m["reply_to"]; ok {
		var id int64
		if json.Unmarshal(r, &id) == nil {
			reply = &id
		}
		delete(m, "reply_to")
	}
	if len(m) == 0 {
		return nil, reply, nil
	}
	wire, err := json.Marshal(m)
	return wire, reply, err
}
