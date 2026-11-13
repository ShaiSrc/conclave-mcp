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
	"github.com/hermes/conclave-mcp/internal/rights"
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
	deliveryTimeout = roomTimeout        // per push
	peerBudget      = time.Minute        // per peer and pass, of delivery and of catch-up
	maxPeerRuns     = 8                  // peers served at once, by delivery and by catch-up
	maxLastError    = 500                // bytes of fed_delivery.last_error
	eventWrap       = 64                 // bytes around an event's payload: {"seq":…,"type":…,"payload":…},
	bodyWrap        = 16                 // and around the events of a body: {"events":[…]}
	maxOversizeLog  = 5                  // seqs kept in a federation.delivery.oversize row
)

// peerAllowed is a condition on fed_delivery rows that each update of
// the delivery state carries: their peer is still allowed. A push
// answered after the peer was blocked or removed then changes nothing,
// so it cannot resume a cursor the block paused.
const peerAllowed = ` AND EXISTS (SELECT 1 FROM federation_peers p WHERE p.domain = fed_delivery.peer AND p.status = 'allowed')`

// eventsBudget is what a push or a pull packs events into: 64 KiB plus
// the message cap, or less when the receiver reads less. The first
// event always goes, so one may be larger; eventsLimit, 64 KiB plus
// twice the cap, is what bodies carrying events are read up to, here,
// and what this server tells hosts it reads (in its invite answer and
// its pulls); a message whose event would not fit in it is refused when
// it is sent (fedrooms.ErrEventTooLarge).
func (s *Service) eventsBudget() int { return maxBody + s.Rooms.MaxMessageBytes }
func (s *Service) eventsLimit() int  { return fedrooms.EventLimit(s.Rooms.MaxMessageBytes) }

// DeliverSoon wakes the delivery worker. The rooms and messages
// services call it after each change; wake-ups while the worker is busy
// fold into one.
func (s *Service) DeliverSoon() {
	select {
	case s.deliver <- struct{}{}:
	default:
	}
}

// deliveryLoop starts the due pushes until ctx is done, then waits for
// the runs going. Between passes it sleeps until the next retry is due,
// at most a minute, unless DeliverSoon wakes it.
func (s *Service) deliveryLoop(ctx context.Context) {
	defer s.runs.Wait()
	for {
		t := time.NewTimer(s.startDue(ctx))
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
	next       string // next_try_at, "" when due at once
	maxBody    int    // what the peer reads a push up to, 0 when unknown
}

// pending selects the cursors with events to send: not paused, the peer
// allowed, events after acked_seq.
const pending = ` FROM fed_delivery d JOIN federation_peers p ON p.domain = d.peer AND p.status = 'allowed'
	WHERE d.paused = 0 AND EXISTS (SELECT 1 FROM fed_events e WHERE e.room_id = d.room_id AND e.seq > d.acked_seq)`

// startDue starts a run (runPeer) for each peer that has cursors due and
// no run going, and returns how long the worker may sleep: until the
// first cursor of a peer without a run is due, at most a minute. A run
// that ends wakes the worker, so a peer waits for its own run only.
func (s *Service) startDue(ctx context.Context) time.Duration {
	due, peers, wait, err := s.plan(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("federation delivery failed", "err", err)
		}
		return deliveryIdle
	}
	s.runMu.Lock()
	for _, p := range peers {
		s.running[p] = true
	}
	s.runMu.Unlock()
	for _, p := range peers {
		s.runs.Add(1)
		go s.runPeer(ctx, p, due[p])
	}
	return wait
}

// plan returns the pending cursors due at now of the peers without a
// run, by peer, the peers in order, and the time until the first other
// cursor of those peers is due, at most deliveryIdle.
func (s *Service) plan(ctx context.Context, now time.Time) (map[string][]cursor, []string, time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.room_id, d.peer, d.acked_seq, d.attempts, d.failing_since,
		COALESCE(d.next_try_at, ''), d.max_body`+pending+` ORDER BY d.peer, d.room_id`)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	due, peers, wait, nowTS := map[string][]cursor{}, []string(nil), deliveryIdle, storage.FormatTS(now)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	for rows.Next() {
		var c cursor
		if err := rows.Scan(&c.room, &c.peer, &c.acked, &c.attempts, &c.failing, &c.next, &c.maxBody); err != nil {
			return nil, nil, 0, err
		}
		switch t, err := time.Parse(time.RFC3339Nano, c.next); {
		case s.running[c.peer]:
		case c.next == "" || c.next <= nowTS || err != nil:
			if due[c.peer] == nil {
				peers = append(peers, c.peer)
			}
			due[c.peer] = append(due[c.peer], c)
		default:
			wait = min(wait, t.Sub(now))
		}
	}
	return due, peers, wait, rows.Err()
}

// runPeer pushes peer's due cursors one after the other, in one of
// maxPeerRuns slots and within the peer budget, so a slow peer holds up
// neither the others nor, past its budget, its own next pass. A peer
// that does not answer is not asked again in the run: its other cursors
// count the same failure. Cursors left when the budget is spent wait
// for the next pass; their order is shuffled for each run, so the ones
// left differ from pass to pass and no room waits behind the others for
// ever.
func (s *Service) runPeer(ctx context.Context, peer string, cs []cursor) {
	defer func() {
		s.runMu.Lock()
		delete(s.running, peer)
		s.runMu.Unlock()
		s.runs.Done()
		s.DeliverSoon()
	}()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return
	}
	for i := len(cs) - 1; i > 0; i-- {
		j := min(int(s.jitter()*float64(i+1)), i)
		cs[i], cs[j] = cs[j], cs[i]
	}
	pctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	now := s.now()
	var down error
	for _, c := range cs {
		if pctx.Err() != nil {
			return
		}
		err := down
		if err == nil {
			var unreachable bool
			if unreachable, err = s.push(ctx, pctx, c); unreachable {
				down = err
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.failed(ctx, c, err, now)
		}
	}
}

// push sends c's peer the events of c.room after its cursor, up to the
// last it may get, and stores the seq it applied. The request runs in
// pctx, the peer's run; the database in ctx. unreachable is true when
// the peer did not answer at all. A cursor whose peer has no bot left
// in the room and has all it may get is deleted.
func (s *Service) push(ctx, pctx context.Context, c cursor) (unreachable bool, err error) {
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
	evs, err := s.loadEvents(ctx, c.room, c.peer, c.acked, upto, c.maxBody)
	if err != nil || len(evs) == 0 {
		return false, err
	}
	hctx, cancel := context.WithTimeout(pctx, deliveryTimeout)
	defer cancel()
	status, resp, err := s.sendSigned(hctx, http.MethodPost, c.peer, "/fed/v1/rooms/"+c.room+"/events",
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
		if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET acked_seq = ? WHERE room_id = ? AND peer = ?`+peerAllowed,
			applied, c.room, c.peer); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%s applied up to %d of %d", c.peer, applied, last)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET acked_seq = ?, attempts = 0, next_try_at = NULL,
		failing_since = NULL, last_error = '' WHERE room_id = ? AND peer = ?`+peerAllowed, applied, c.room, c.peer); err != nil {
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
		WHERE m.room_id = fed_delivery.room_id AND k.remote_domain = fed_delivery.peer)`+peerAllowed, c.room, c.peer, upto)
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

// reads reports whether peer has a bot in roomID with the read right.
func (s *Service) reads(ctx context.Context, roomID, peer string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.rights FROM room_members m JOIN api_keys k ON k.id = m.key_id
		WHERE m.room_id = ? AND k.remote_domain = ?`, roomID, peer)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return false, err
		}
		if rights.FromStorage(stored).Has(rights.Read) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// loadEvents returns roomID's events after seq since, up to seq upto,
// as peer gets them: at most 100 and, past the first, within
// eventsBudget and limit, what peer reads a body up to (0 when unknown:
// then taken to be what this server reads). A peer without a reader in
// the room (reads, checked now) gets each message event as a skip,
// which keeps its seqs contiguous. The other events go to every member
// server: it needs the members and the room to render it, and a
// message.delete, which carries only an id, lets one that lost read
// delete its copy. An event over limit goes as a skip too, so it cannot
// stall the room; one federation.delivery.oversize row per call names
// them.
func (s *Service) loadEvents(ctx context.Context, roomID, peer string, since, upto int64, limit int) ([]fedrooms.Event, error) {
	reader, err := s.reads(ctx, roomID, peer)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = s.eventsLimit()
	}
	limit -= bodyWrap
	budget := min(s.eventsBudget(), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT seq, type, payload FROM fed_events
		WHERE room_id = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, roomID, since, upto, maxRoomEvents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, size, oversize := []fedrooms.Event{}, 0, []int64(nil)
	for rows.Next() {
		var e fedrooms.Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.Type, &payload); err != nil {
			return nil, err
		}
		over := false
		switch {
		case e.Type == fedrooms.EventMessage && !reader:
			e.Type, payload = fedrooms.EventSkip, ""
		case len(payload)+eventWrap > limit:
			e.Type, payload, over = fedrooms.EventSkip, "", true
		}
		if size += len(payload) + eventWrap; len(out) > 0 && size > budget {
			break
		}
		if over {
			oversize = append(oversize, e.Seq)
		}
		if payload != "" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close() // before the audit write
	if len(oversize) > 0 {
		detail, _ := json.Marshal(map[string]any{"count": len(oversize), "seqs": oversize[:min(len(oversize), maxOversizeLog)],
			"limit": limit + bodyWrap})
		s.auditRoom(ctx, "federation.delivery.oversize", peer, roomID, "error:too_large", string(detail))
	}
	return out, nil
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
	res, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET attempts = ?, next_try_at = ?, failing_since = ?,
		last_error = ?, paused = ? WHERE room_id = ? AND peer = ?`+peerAllowed, attempts, storage.FormatTS(now.Add(s.backoff(attempts))),
		storage.FormatTS(since), msg, paused, c.room, c.peer)
	if err != nil {
		s.logger.Warn("federation delivery failed", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 && paused {
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
// request from it, a push it took, or an invite it accepted. Only
// paused cursors change, and only while the peer is allowed.
func (s *Service) resumeDelivery(ctx context.Context, peer string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET paused = 0, attempts = 0, next_try_at = NULL,
		failing_since = NULL, last_error = '' WHERE peer = ? AND paused = 1`+peerAllowed, peer)
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
