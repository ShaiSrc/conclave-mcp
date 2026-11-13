package federation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Host side of federated rooms: the delivery worker. Each member server
// has a cursor per room (fed_delivery); the worker pushes it the events
// after acked_seq and stores the seq the member answers it applied.

const (
	backoffMin      = time.Second
	backoffMax      = time.Hour
	pauseAfter      = 7 * 24 * time.Hour // of failed pushes, then the cursor is paused
	deliveryIdle    = time.Minute        // the worker's longest sleep
	deliveryTimeout = 30 * time.Second   // per push
	maxLastError    = 500                // bytes of fed_delivery.last_error
)

// eventsBudget is what a push or a pull packs events into: 64 KiB plus
// the message cap. The first event always goes, so one may be larger;
// eventsLimit, 64 KiB plus twice the cap, is what bodies carrying
// events are read up to. ponytail: a message whose JSON form is over
// twice its size (control characters) stalls its room's delivery.
func (s *Service) eventsBudget() int { return maxBody + s.Rooms.MaxMessageBytes }
func (s *Service) eventsLimit() int  { return maxBody + 2*s.Rooms.MaxMessageBytes }

// DeliverSoon wakes the delivery worker. The rooms and messages
// services call it after each change; wake-ups while the worker is busy
// fold into one.
func (s *Service) DeliverSoon() {
	select {
	case s.deliver <- struct{}{}:
	default:
	}
}

// deliveryLoop pushes due events until ctx is done. Between passes it
// sleeps until the next retry is due, at most a minute, unless
// DeliverSoon wakes it.
func (s *Service) deliveryLoop(ctx context.Context) {
	for {
		t := time.NewTimer(s.deliverDue(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.deliver:
		case <-t.C:
		}
		t.Stop()
	}
}

// cursor is one row of fed_delivery.
type cursor struct {
	room, peer string
	acked      int64
	attempts   int
	failing    sql.NullString
}

// pending selects the cursors with events to send: not paused, the peer
// allowed, events after acked_seq.
const pending = ` FROM fed_delivery d JOIN federation_peers p ON p.domain = d.peer AND p.status = 'allowed'
	WHERE d.paused = 0 AND EXISTS (SELECT 1 FROM fed_events e WHERE e.room_id = d.room_id AND e.seq > d.acked_seq)`

// deliverDue makes one push for each pending cursor whose retry time
// has come and returns how long the worker may sleep. A peer that does
// not answer is not asked again in the same pass: its other cursors
// count the same failure. ponytail: one push at a time; a goroutine per
// peer if slow peers delay the others too much.
func (s *Service) deliverDue(ctx context.Context) time.Duration {
	now := s.now()
	due, err := s.dueCursors(ctx, now)
	if err != nil {
		s.logger.Warn("federation delivery failed", "err", err)
		return deliveryIdle
	}
	down := map[string]error{}
	for _, c := range due {
		err, isDown := down[c.peer]
		if !isDown {
			var unreachable bool
			unreachable, err = s.push(ctx, c)
			if unreachable {
				down[c.peer] = err
			}
		}
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			s.failed(ctx, c, err, now)
		}
	}
	return s.nextDue(ctx, now)
}

func (s *Service) dueCursors(ctx context.Context, now time.Time) ([]cursor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.room_id, d.peer, d.acked_seq, d.attempts, d.failing_since`+pending+`
		AND (d.next_try_at IS NULL OR d.next_try_at <= ?) ORDER BY d.room_id, d.peer`, storage.FormatTS(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cursor
	for rows.Next() {
		var c cursor
		if err := rows.Scan(&c.room, &c.peer, &c.acked, &c.attempts, &c.failing); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nextDue returns the time until the first pending cursor is due, at
// most deliveryIdle.
func (s *Service) nextDue(ctx context.Context, now time.Time) time.Duration {
	var next sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(COALESCE(d.next_try_at, ''))`+pending).Scan(&next); err != nil || !next.Valid {
		return deliveryIdle
	}
	t, err := time.Parse(time.RFC3339Nano, next.String)
	if next.String == "" || err != nil {
		return 0
	}
	return min(max(t.Sub(now), 0), deliveryIdle)
}

// push sends c's peer the events of c.room after its cursor, up to the
// last it may get, and stores the seq it applied. unreachable is true
// when the peer did not answer at all. A cursor whose peer has no bot
// left in the room and has all it may get is deleted.
func (s *Service) push(ctx context.Context, c cursor) (unreachable bool, err error) {
	upto, member, err := s.deliveryLimit(ctx, c)
	if err != nil {
		return false, err
	}
	if upto <= c.acked {
		if !member {
			err = s.dropCursor(ctx, c, upto)
		}
		return false, err
	}
	evs, err := s.loadEvents(ctx, c.room, c.acked, upto)
	if err != nil || len(evs) == 0 {
		return false, err
	}
	pctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	status, resp, err := s.sendSigned(pctx, http.MethodPost, c.peer, "/fed/v1/rooms/"+c.room+"/events",
		rawJSON(map[string]any{"events": evs}), maxBody)
	if err != nil {
		return true, err
	}
	var out struct {
		Applied *int64 `json:"applied"`
	}
	if status != http.StatusOK || json.Unmarshal(resp, &out) != nil || out.Applied == nil {
		return false, fmt.Errorf("%s answered with status %d", c.peer, status)
	}
	last := evs[len(evs)-1].Seq
	applied := min(max(*out.Applied, 0), last)
	if applied < last {
		// Taken as the cursor, so the next push starts where the
		// member is; retried after the backoff.
		if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET acked_seq = ? WHERE room_id = ? AND peer = ?`,
			applied, c.room, c.peer); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%s applied up to %d of %d", c.peer, applied, last)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET acked_seq = ?, attempts = 0, next_try_at = NULL,
		failing_since = NULL, last_error = '' WHERE room_id = ? AND peer = ?`, applied, c.room, c.peer); err != nil {
		return false, err
	}
	if !member && applied >= upto {
		if err := s.dropCursor(ctx, c, upto); err != nil {
			return false, err
		}
	}
	return false, s.resumeDelivery(ctx, c.peer)
}

// dropCursor deletes c once its peer has the events up to upto and
// still has no bot in the room: delivery to it ends there.
func (s *Service) dropCursor(ctx context.Context, c cursor, upto int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM fed_delivery WHERE room_id = ? AND peer = ? AND acked_seq >= ?
		AND NOT EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = fed_delivery.room_id AND k.remote_domain = fed_delivery.peer)`, c.room, c.peer, upto)
	return err
}

// deliveryLimit returns the last event c's peer may get: the room's last
// while one of its bots is a member, else the removal of its last bot.
func (s *Service) deliveryLimit(ctx context.Context, c cursor) (upto int64, member bool, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.remote_domain = ?)`, c.room, c.peer).Scan(&member); err != nil {
		return 0, false, err
	}
	q, args := `SELECT COALESCE(MAX(seq), 0) FROM fed_events WHERE room_id = ?`, []any{c.room}
	if !member {
		q += ` AND type = 'member.remove' AND substr(json_extract(payload, '$.address'),
			instr(json_extract(payload, '$.address'), '@') + 1) = ?`
		args = append(args, c.peer)
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&upto)
	return upto, member, err
}

// loadEvents returns roomID's events after seq since, up to seq upto:
// at most 100 and, past the first, within eventsBudget.
func (s *Service) loadEvents(ctx context.Context, roomID string, since, upto int64) ([]fedrooms.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, type, payload FROM fed_events
		WHERE room_id = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, roomID, since, upto, maxRoomEvents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, size := []fedrooms.Event{}, 0
	for rows.Next() {
		var e fedrooms.Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.Type, &payload); err != nil {
			return nil, err
		}
		if size += len(payload) + 64; len(out) > 0 && size > s.eventsBudget() { // 64: the event's other fields
			break
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// failed records a failed push of c at now: the next try after the
// backoff; after 7 days of failures the cursor is paused, which is
// audited, until the peer is in contact again (resumeDelivery).
func (s *Service) failed(ctx context.Context, c cursor, cause error, now time.Time) {
	attempts, since := c.attempts+1, now
	if t, err := time.Parse(time.RFC3339Nano, c.failing.String); c.failing.Valid && err == nil {
		since = t
	}
	paused := now.Sub(since) >= pauseAfter
	msg := cause.Error()
	if len(msg) > maxLastError {
		msg = msg[:maxLastError]
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET attempts = ?, next_try_at = ?, failing_since = ?,
		last_error = ?, paused = ? WHERE room_id = ? AND peer = ?`, attempts, storage.FormatTS(now.Add(s.backoff(attempts))),
		storage.FormatTS(since), msg, paused, c.room, c.peer); err != nil {
		s.logger.Warn("federation delivery failed", "err", err)
		return
	}
	if paused {
		detail, _ := json.Marshal(map[string]any{"failing_since": storage.FormatTS(since), "attempts": attempts, "last_error": msg})
		s.auditRoom(ctx, "federation.delivery.paused", c.peer, c.room, "error:unreachable", string(detail))
	}
}

// backoff returns the wait after the n-th failure in a row: between
// half and all of 2^(n-1) seconds, at most an hour.
func (s *Service) backoff(n int) time.Duration {
	d := backoffMax
	if n <= 12 {
		d = min(backoffMin<<(n-1), backoffMax)
	}
	return d/2 + time.Duration(s.jitter()*float64(d/2))
}

// resumeDelivery unpauses the cursors of peer after contact: a verified
// request from it, or a push it took.
func (s *Service) resumeDelivery(ctx context.Context, peer string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET paused = 0, attempts = 0, next_try_at = NULL,
		failing_since = NULL, last_error = '' WHERE peer = ? AND paused = 1`, peer)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.DeliverSoon()
	}
	return nil
}

// rawJSON marshals v without escaping <, > and &: event payloads go as
// they are stored.
func rawJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return b.Bytes()
}
