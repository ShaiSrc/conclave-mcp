package federation

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/claims"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Member side of federated rooms: replicas of rooms hosted elsewhere.
// The host invites a bot of ours, then pushes the room's events, which
// the replica applies in order; it pulls what it missed.

const (
	catchupEvery  = 5 * time.Minute
	pullTimeout   = roomTimeout // per replica and pull
	maxPullPages  = 50          // pages of events read per pull
	maxReplicas   = 200         // replica rooms per host, archived ones included
	maxRefusalLog = 5           // reasons kept in a federation.room.event_refused row
)

// errRefused marks an event the replica does not apply as sent. It
// counts as applied: it is logged and audited, and the next one
// follows, so one bad event cannot stall the room.
var errRefused = errors.New("event refused")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRefused, fmt.Sprintf(format, args...))
}

// errNameTaken is nameReplica's error when neither name is free.
var errNameTaken = errors.New("no free name for the replica")

// refusals are the events one push or pull of a host refused, for its
// one federation.room.event_refused row: their count and the first
// reasons.
type refusals struct {
	n       int
	reasons []string
}

func (rf *refusals) add(ev fedrooms.Event, err error) {
	rf.n++
	if len(rf.reasons) < maxRefusalLog {
		rf.reasons = append(rf.reasons, fmt.Sprintf("seq %d %s: %v", ev.Seq, ev.Type, err))
	}
}

// auditRefused writes the row of what rf holds for replica roomID of
// host, and logs it, if it holds anything.
func (s *Service) auditRefused(ctx context.Context, roomID, host string, rf *refusals) {
	if rf.n == 0 {
		return
	}
	if s.warnLog.allow(host + " bad_event") {
		s.logger.Warn("federation room events refused", "room", roomID, "host", host, "count", rf.n, "first", rf.reasons[0])
	}
	detail, _ := json.Marshal(map[string]any{"count": rf.n, "reasons": rf.reasons})
	s.auditRoom(context.WithoutCancel(ctx), "federation.room.event_refused", host, roomID, "error:bad_event", string(detail))
}

// allowedOrigin answers 403 peer_not_allowed and returns false unless
// the verified origin is an allowed peer: open mode lets seen peers
// through Verify, rooms take allowed ones only.
func (s *Service) allowedOrigin(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.originAllowed(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if !ok {
		writeErr(w, r, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
	}
	return ok
}

// originAllowed reports whether the verified origin is an allowed peer.
func (s *Service) originAllowed(ctx context.Context) (bool, error) {
	p, err := s.peers.Get(ctx, OriginFrom(ctx))
	if errors.Is(err, ErrPeerNotFound) {
		err = nil
	}
	return p.Status == statusAllowed, err
}

// handleRoomInvite answers POST /fed/v1/rooms/{id}/invite from the
// host of room {id}: our bot named by handle joins the replica with the
// rights. It must be a local bot, not revoked and no admin key (404
// otherwise). The replica is created under the host's room id, of the
// invite's kind, or updated and made active again; a room of that id
// that is not the origin's replica of that kind is a 409. A room name
// starting with dm: is a direct conversation's (400 otherwise), one
// starting with feed: a feed's (400 otherwise). The answer has the kind
// and max_events_bytes.
//
// A feed invite (kind feed) brings our bot into the feed of a bot of the
// origin, whose address it carries as feed, named feed:<address> or that
// cut and suffixed with #<room id> by its host (400 otherwise); from a
// host that sends no feed, the address is read from the name. It needs a
// follow of our bot toward that address, on the origin's domain (403
// invite_refused otherwise), which it makes accepted. The bot gets read,
// whatever the host sent, and the replica keeps the address in
// rooms.feed.
//
// A direct invite (kind direct) starts a conversation with our bot: it
// takes one only from an allowed peer (seen ones get 403 dm_refused,
// before the handle is looked at, so they cannot probe for bots), named
// for our bot and one of the origin's (400 otherwise), for a bot whose
// DM policy is peers (403 dm_refused), and at most DMInvitesPerHour from
// each peer (429). The bot gets the direct rights, whatever the host
// sent, and the origin's bot joins with it. Into a replica with another
// member, of ours or of the origin, it is a 409; the same invite again
// changes nothing; an older active replica of the pair from the origin
// is archived.
func (s *Service) handleRoomInvite(w http.ResponseWriter, r *http.Request) {
	ctx, origin, roomID := r.Context(), OriginFrom(r.Context()), chi.URLParam(r, "id")
	var in struct {
		Name   string   `json:"name"`
		Handle string   `json:"handle"`
		Rights []string `json:"rights"`
		Kind   string   `json:"kind"`
		Feed   string   `json:"feed"` // a feed's bot
	}
	derr := json.NewDecoder(r.Body).Decode(&in)
	in.Kind = cmp.Or(in.Kind, "room")
	direct, feed := in.Kind == kindDirect, in.Kind == kindFeed
	feedOf := in.Feed
	if feedOf == "" { // a host that does not send it: the name holds it, unless it was cut
		feedOf, _, _ = strings.Cut(strings.TrimPrefix(in.Name, fedrooms.FeedPrefix), "#")
	}
	switch allowed, err := s.originAllowed(ctx); {
	case err != nil:
		s.fail(w, r, err)
		return
	case !allowed && direct:
		writeErr(w, r, http.StatusForbidden, "dm_refused", "direct conversations come from allowed peers only")
		return
	case !allowed:
		writeErr(w, r, http.StatusForbidden, "peer_not_allowed", "peer not allowed")
		return
	}
	// The audit row is written outside the transaction, which holds the
	// write lock: it is rolled back first.
	var tx *sql.Tx
	refuse := func(status int, code, msg string) {
		if tx != nil {
			_ = tx.Rollback()
		}
		detail, _ := json.Marshal(map[string]any{"handle": in.Handle, "rights": in.Rights})
		s.auditRoom(ctx, "federation.room.invite", origin, roomID, "error:"+code, string(detail))
		writeErr(w, r, status, code, msg)
	}
	// dmRefuse refuses a direct invite for reason (not a bot, policy,
	// limit).
	dmRefuse := func(status int, code, reason, msg string) {
		detail, _ := json.Marshal(map[string]string{"to": in.Handle, "reason": reason})
		s.auditRoom(ctx, "direct.refused", origin, roomID, "error:"+code, string(detail))
		refuse(status, code, msg)
	}
	if derr != nil {
		refuse(http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	id, err := ulid.ParseStrict(roomID)
	set, rerr := rights.Parse(in.Rights)
	switch {
	case direct:
		set, rerr = fedrooms.DirectRights, nil
	case feed: // a follower reads only
		set, rerr = rights.Set{rights.Read}, nil
	}
	switch {
	case err != nil || id.String() != roomID:
		refuse(http.StatusBadRequest, "bad_request", "room id must be a ULID")
		return
	case in.Name == "" || len(in.Name) > 128:
		refuse(http.StatusBadRequest, "bad_request", "name must be 1..128 chars")
		return
	case rerr != nil:
		refuse(http.StatusBadRequest, "bad_request", rerr.Error())
		return
	case in.Kind != "room" && !direct && !feed:
		refuse(http.StatusBadRequest, "bad_request", "kind must be room, direct or feed")
		return
	case !direct && strings.HasPrefix(in.Name, fedrooms.DirectPrefix):
		refuse(http.StatusBadRequest, "bad_request", "names starting with "+fedrooms.DirectPrefix+" are reserved for direct conversations")
		return
	case feed != strings.HasPrefix(in.Name, fedrooms.FeedPrefix):
		refuse(http.StatusBadRequest, "bad_request", "names starting with "+fedrooms.FeedPrefix+" are reserved for feeds, and a feed's starts with it")
		return
	case feed && in.Feed != "" && in.Name != fedrooms.FeedPrefix+in.Feed && in.Name != fedrooms.Suffixed(fedrooms.FeedPrefix+in.Feed, roomID):
		refuse(http.StatusBadRequest, "bad_request", "a feed's name is "+fedrooms.FeedPrefix+"<address>, or that cut with #<room id>")
		return
	case feed && !strings.HasSuffix(feedOf, "@"+origin):
		refuse(http.StatusForbidden, "invite_refused", "a feed is the feed of a bot of its host")
		return
	}
	var keyID int64
	var policy string
	h, err := bots.NormalizeHandle(in.Handle)
	if err == nil {
		err = s.db.QueryRowContext(ctx, `SELECT id, COALESCE((SELECT dm_policy FROM bot_profiles WHERE key_id = api_keys.id), 'local')
			FROM api_keys WHERE handle = ? AND revoked_at IS NULL AND remote_domain IS NULL AND is_admin = 0`, h).Scan(&keyID, &policy)
	}
	switch {
	case err == nil:
	case (errors.Is(err, sql.ErrNoRows) || errors.Is(err, bots.ErrInvalid)) && direct:
		dmRefuse(http.StatusNotFound, "not_found", "not a bot", "no such bot")
		return
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, bots.ErrInvalid):
		refuse(http.StatusNotFound, "not_found", "no such bot")
		return
	default:
		s.fail(w, r, err)
		return
	}
	var other string // the origin's bot in a direct conversation
	if direct {
		var ok bool
		other, ok = fedrooms.DirectOther(in.Name, h+"@"+s.id.Domain)
		name, domain, _ := strings.Cut(other, "@")
		if hn, err := bots.NormalizeHandle(name); !ok || err != nil || hn != name || domain != origin {
			refuse(http.StatusBadRequest, "bad_request", "a direct conversation's name is dm:<address>~<address>, sorted, of the invited bot and one of the host's")
			return
		}
		if policy != bots.DMPeers {
			dmRefuse(http.StatusForbidden, "dm_refused", "policy", h+" does not take direct conversations from other servers")
			return
		}
		now := s.now()
		if lim := s.dmLimiter(origin); !lim.AllowN(now, 1) {
			w.Header().Set("Retry-After", retryAfterAt(lim, now))
			dmRefuse(http.StatusTooManyRequests, "rate_limited", "limit", "too many new direct conversations, try again later")
			return
		}
	}

	if tx, err = s.db.BeginTx(ctx, nil); err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	// A direct room keeps its pair, a feed its bot.
	var home, pair sql.NullString
	var kind string
	switch err := tx.QueryRowContext(ctx, `SELECT home_domain, CASE WHEN feed IS NOT NULL THEN 'feed' ELSE kind END, COALESCE(pair, feed)
		FROM rooms WHERE id = ?`, roomID).Scan(&home, &kind, &pair); {
	case errors.Is(err, sql.ErrNoRows):
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms WHERE home_domain = ?`, origin).Scan(&n); err != nil {
			s.fail(w, r, err)
			return
		}
		if n >= maxReplicas {
			refuse(http.StatusConflict, "limit", fmt.Sprintf("%s has %d rooms here already", origin, maxReplicas))
			return
		}
		// Created archived, so its name cannot clash until nameReplica
		// picks one. Its owner is the host's operator.
		owner, err := fedrooms.StandIn(ctx, tx, "operator@"+origin)
		if errors.Is(err, fedrooms.ErrStandInLimit) {
			refuse(http.StatusConflict, "limit", err.Error())
			return
		}
		if err == nil {
			now := storage.FormatTS(time.Now())
			column := in.Kind
			var newPair, newFeed any // a direct replica keeps the pair's name, checked above, a feed its bot
			switch {
			case direct:
				newPair = in.Name
			case feed:
				column, newFeed = "room", feedOf
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, home_domain, archived_at, kind, pair, feed)
				VALUES (?, ?, ?, ?, 'private', ?, ?, ?, ?, ?)`, roomID, in.Name, owner, now, origin, now, column, newPair, newFeed)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
	case err != nil:
		s.fail(w, r, err)
		return
	case home.String != origin || kind != in.Kind || direct && pair.String != in.Name || feed && pair.String != feedOf:
		refuse(http.StatusConflict, "conflict", "a room with this id exists here")
		return
	}
	if feed { // the follow of our bot toward the feed's bot is accepted now; without one, no replica
		res, err := tx.ExecContext(ctx, `UPDATE follows SET status = 'accepted', decided_at = COALESCE(decided_at, ?)
			WHERE follower_key_id = ? AND target = ? AND target_key_id IS NULL`, storage.FormatTS(time.Now()), keyID, feedOf)
		var n int64
		if err == nil {
			n, err = res.RowsAffected()
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n == 0 {
			refuse(http.StatusForbidden, "invite_refused", h+" does not follow the feed's bot")
			return
		}
	}
	if direct {
		if !s.directPair(w, r, tx, roomID, in.Name, origin, keyID, other, refuse) {
			return
		}
	}
	if err := nameReplica(ctx, tx, roomID, in.Name, origin, true); err != nil {
		if errors.Is(err, errNameTaken) {
			refuse(http.StatusConflict, "conflict", "no free name for the room here")
		} else {
			s.fail(w, r, err)
		}
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)
		ON CONFLICT(room_id, key_id) DO UPDATE SET rights = excluded.rights`,
		roomID, keyID, storage.FormatTS(time.Now()), set.Storage()); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.fail(w, r, err)
		return
	}
	detail, _ := json.Marshal(map[string]any{"handle": h, "rights": set})
	s.auditRoom(ctx, "federation.room.invite", origin, roomID, "ok", string(detail))
	if direct {
		detail, _ := json.Marshal(map[string]string{"host": origin, "to": h + "@" + s.id.Domain})
		s.auditRoom(ctx, "direct.start", origin, roomID, "ok", string(detail))
	}
	// max_events_bytes is what this server reads a push up to: the host
	// packs its pushes within it. kind tells a host that it was taken as
	// a direct conversation.
	writeJSON(w, http.StatusCreated, map[string]any{"room_id": roomID, "max_events_bytes": s.eventsLimit(), "kind": in.Kind})
}

// directPair adds other, the origin's bot, to direct replica roomID of
// our bot keyID, and archives an older active replica of the pair from
// the origin, before the replica is named: it held the name. A replica
// with another member, of ours or of the origin, is a 409: a host
// cannot put a second bot of ours into a conversation. So is a replica
// that a newer one of the pair replaced, archived or left: a late retry
// of its invite neither brings it back nor archives the newer one. And
// so is an invite while we host a room of the pair with a lower id:
// when both bots start at once, the lower room is the pair's on both
// servers, since each refuses the other's higher one. Rooms of the pair
// are those whose pair is name, the pair's name. On an error it answers
// and returns false.
func (s *Service) directPair(w http.ResponseWriter, r *http.Request, tx *sql.Tx, roomID, name, origin string, keyID int64, other string,
	refuse func(int, string, string)) bool {
	ctx := r.Context()
	key, err := fedrooms.StandIn(ctx, tx, other)
	if errors.Is(err, fedrooms.ErrStandInLimit) {
		refuse(http.StatusConflict, "limit", err.Error())
		return false
	}
	var taken, superseded bool
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_members WHERE room_id = ? AND key_id NOT IN (?, ?))`,
			roomID, keyID, key).Scan(&taken)
	}
	if err == nil { // a late retry of an older invite: the newer replica stays, archived or left
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rooms o WHERE o.pair = ? AND o.home_domain = ?
			AND o.id != ? AND o.created_at > (SELECT created_at FROM rooms WHERE id = ?))`,
			name, origin, roomID, roomID).Scan(&superseded)
	}
	var lower bool
	if err == nil { // both bots started at once: the room with the lower id is the pair's
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rooms o WHERE o.pair = ? AND o.home_domain IS NULL
			AND o.archived_at IS NULL AND o.id < ?)`, name, roomID).Scan(&lower)
	}
	switch {
	case taken:
		refuse(http.StatusConflict, "conflict", "the conversation has another member")
		return false
	case superseded:
		refuse(http.StatusConflict, "conflict", "a newer conversation of the pair replaced this one")
		return false
	case lower:
		refuse(http.StatusConflict, "conflict", "the other bot started this conversation here at the same time: start again to use it")
		return false
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)
			ON CONFLICT(room_id, key_id) DO NOTHING`, roomID, key, storage.FormatTS(time.Now()), fedrooms.DirectRights.Storage())
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE rooms SET archived_at = ? WHERE pair = ? AND home_domain = ? AND id != ?
			AND archived_at IS NULL`, storage.FormatTS(time.Now()), name, origin, roomID)
	}
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	return true
}

// DMInvitesPerHour is how many direct invites a server takes from one
// peer an hour.
const DMInvitesPerHour = 100

// kindDirect is the kind of a direct conversation's room.
const kindDirect = "direct"

// dmLimiter returns origin's limiter of direct invites.
func (s *Service) dmLimiter(origin string) *rate.Limiter {
	return s.hourly(&s.dmLimits, origin, DMInvitesPerHour)
}

// hourly returns origin's limiter of n an hour in *m, which takes the
// time from s.now. ponytail: one per peer, never evicted; peers are
// bounded by the table.
func (s *Service) hourly(m *map[string]*rate.Limiter, origin string, n int) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *m == nil {
		*m = map[string]*rate.Limiter{}
	}
	lim := (*m)[origin]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(time.Hour/time.Duration(n)), n)
		(*m)[origin] = lim
	}
	return lim
}

// nameReplica gives replica roomID the name its host sent. If activate
// is set it also makes the replica active. An active replica whose name
// a local room holds is named name@host instead, so active rooms keep
// unique names; when that is taken too, it is errNameTaken.
func nameReplica(ctx context.Context, tx *sql.Tx, roomID, name, host string, activate bool) error {
	q := `UPDATE rooms SET name = ? WHERE id = ?`
	if activate {
		q = `UPDATE rooms SET name = ?, archived_at = NULL WHERE id = ?`
	}
	for _, n := range []string{name, name + "@" + host} {
		_, err := tx.ExecContext(ctx, q, n, roomID)
		if err == nil || !strings.Contains(err.Error(), "constraint failed") {
			return err
		}
	}
	return errNameTaken
}

// handleRoomPush answers POST /fed/v1/rooms/{id}/events from the host of
// replica {id}: up to 100 events of its log, applied in seq order. A gap
// is filled by pulling. It answers {"applied": seq}, the last seq
// applied, which the host keeps as its cursor.
func (s *Service) handleRoomPush(w http.ResponseWriter, r *http.Request) {
	ctx, origin, roomID := r.Context(), OriginFrom(r.Context()), chi.URLParam(r, "id")
	if !s.allowedOrigin(w, r) {
		return
	}
	var home sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT home_domain FROM rooms WHERE id = ?`, roomID).Scan(&home); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		s.fail(w, r, err)
		return
	}
	if !home.Valid || home.String != origin {
		writeErr(w, r, http.StatusNotFound, "not_found", "room not found")
		return
	}
	var in struct {
		Events []fedrooms.Event `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Events) > maxRoomEvents {
		writeErr(w, r, http.StatusBadRequest, "bad_request", `body must be {"events": [...]}, at most 100`)
		return
	}
	rf := &refusals{}
	defer s.auditRefused(ctx, roomID, origin, rf)
	applied, gap, err := s.applyBatch(ctx, roomID, origin, in.Events, rf)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if gap {
		pctx, cancel := context.WithTimeout(ctx, pullTimeout)
		defer cancel()
		if applied, err = s.pull(pctx, roomID, origin, rf); err != nil {
			s.logger.Warn("federation room pull failed", "room", roomID, "host", origin, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{"applied": applied})
}

// applyBatch applies evs to replica roomID of host in seq order. Events
// it has are skipped; at a gap it stops and reports it. A refused event
// counts as applied and goes to rf. It returns the last seq applied.
// ponytail: one lock for all replicas; one per room if applying ever
// gets slow.
func (s *Service) applyBatch(ctx context.Context, roomID, host string, evs []fedrooms.Event, rf *refusals) (applied int64, gap bool, err error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if err := s.db.QueryRowContext(ctx, `SELECT applied_seq FROM rooms WHERE id = ? AND home_domain = ?`, roomID, host).
		Scan(&applied); err != nil {
		return 0, false, err
	}
	for _, ev := range slices.SortedFunc(slices.Values(evs), func(a, b fedrooms.Event) int { return cmp.Compare(a.Seq, b.Seq) }) {
		if ev.Seq <= applied {
			continue
		}
		if ev.Seq > applied+1 {
			return applied, true, nil
		}
		if err := s.applyEvent(ctx, roomID, host, ev); errors.Is(err, errRefused) {
			rf.add(ev, err)
		} else if err != nil {
			return applied, false, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE rooms SET applied_seq = ? WHERE id = ?`, ev.Seq, roomID); err != nil {
			return applied, false, err
		}
		applied = ev.Seq
	}
	return applied, false, nil
}

// applyEvent applies one event of host to replica roomID. Each kind is
// idempotent, so an event applied again after a crash changes nothing.
// A skip changes nothing. An event that cannot be applied as sent is
// errRefused.
func (s *Service) applyEvent(ctx context.Context, roomID, host string, ev fedrooms.Event) error {
	switch ev.Type {
	case fedrooms.EventSkip:
		return nil
	case fedrooms.EventMessage:
		// The blanked event of a message the host deleted is applied
		// as its message.delete.
		var p struct{ Deleted bool }
		if json.Unmarshal(ev.Payload, &p) != nil || !p.Deleted {
			return s.applyMessage(ctx, roomID, host, ev.Payload)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	switch ev.Type {
	case fedrooms.EventRoom:
		err = applyRoom(ctx, tx, roomID, host, ev.Payload)
	case fedrooms.EventMember:
		err = s.applyMember(ctx, tx, roomID, host, ev.Payload)
	case fedrooms.EventMemberRemove:
		err = s.applyRemove(ctx, tx, roomID, ev.Payload)
	case fedrooms.EventMessageDelete, fedrooms.EventMessage:
		err = applyDelete(ctx, tx, roomID, ev.Payload)
	default:
		err = refused("unknown event type %q", ev.Type)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// applyRoom renames the replica, and archives it when the host did. Its
// kind is the invite's: a room event of another kind is refused. An
// archived feed ends the accepted follows of our bots in it, but not of
// those in a newer active replica of the same feed from that host.
func applyRoom(ctx context.Context, tx *sql.Tx, roomID, host string, payload json.RawMessage) error {
	var p fedrooms.RoomPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Name == "" || len(p.Name) > 128 {
		return refused("room event: bad payload")
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM rooms WHERE id = ?`, roomID).Scan(&kind); err != nil {
		return err
	}
	if cmp.Or(p.Kind, "room") != kind {
		return refused("room event: kind %q, the replica is %s", p.Kind, kind)
	}
	if p.Archived {
		_, err := tx.ExecContext(ctx, `UPDATE rooms SET name = ?, archived_at = COALESCE(archived_at, ?) WHERE id = ?`,
			p.Name, storage.FormatTS(time.Now()), roomID)
		if err == nil { // as on its host, an archived feed ends its accepted follows
			_, err = tx.ExecContext(ctx, `DELETE FROM follows WHERE status = 'accepted' AND target_key_id IS NULL
				AND target = (SELECT feed FROM rooms WHERE id = ?1)
				AND NOT EXISTS (SELECT 1 FROM rooms o JOIN room_members m ON m.room_id = o.id AND m.key_id = follows.follower_key_id
					WHERE o.feed = follows.target AND o.home_domain = ?2 AND o.id != ?1 AND o.archived_at IS NULL)`, roomID, host)
		}
		return err
	}
	err := nameReplica(ctx, tx, roomID, p.Name, host, false)
	if errors.Is(err, errNameTaken) {
		return refused("room event: %v", err)
	}
	return err
}

// applyMember adds a member to the replica or sets its rights. A member
// of this server must be one of its bots; a direct conversation takes
// ours by invite only, which checks their DM policy, so here it only
// sets the rights of one it has, and it has one bot of the host, which
// joined with the invite: another is refused. A feed takes ours by
// invite only too, which checks their follow, and gives them read. When
// it is the first bot of ours in an archived replica (invited again),
// the replica is active again.
func (s *Service) applyMember(ctx context.Context, tx *sql.Tx, roomID, host string, payload json.RawMessage) error {
	var p fedrooms.MemberPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return refused("member event: bad payload")
	}
	set, err := rights.Parse(p.Rights)
	if err != nil {
		return refused("member event: %v", err)
	}
	key, local, err := s.memberKey(ctx, tx, p.Address, true)
	if err != nil {
		return err
	}
	if !local {
		var second bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rooms WHERE id = ? AND kind = 'direct')
			AND EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
			WHERE m.room_id = ? AND k.remote_domain IS NOT NULL AND m.key_id != ?)`, roomID, roomID, key).Scan(&second); err != nil {
			return err
		}
		if second {
			return refused("member event: a direct conversation has one bot of the host, not %s too", p.Address)
		}
	}
	var bot, ours, invited bool
	var kind string
	if local {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = ? AND handle IS NOT NULL
			AND is_admin = 0 AND revoked_at IS NULL), `+localMember+`,
			EXISTS (SELECT 1 FROM room_members WHERE room_id = ? AND key_id = ?),
			(SELECT CASE WHEN feed IS NOT NULL THEN 'feed' ELSE kind END FROM rooms WHERE id = ?)`,
			key, roomID, roomID, key, roomID).Scan(&bot, &ours, &invited, &kind); err != nil {
			return err
		}
		switch {
		case !bot:
			return refused("member event: no bot of ours is %s", p.Address)
		case kind != "room" && !invited:
			return refused("member event: %s was not invited into the %s", p.Address, kind)
		case kind == kindFeed:
			set = rights.Set{rights.Read}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)
		ON CONFLICT(room_id, key_id) DO UPDATE SET rights = excluded.rights`,
		roomID, key, storage.FormatTS(time.Now()), set.Storage()); err != nil {
		return err
	}
	if !local || ours {
		return nil
	}
	var name string
	var archived bool
	if err := tx.QueryRowContext(ctx, `SELECT name, archived_at IS NOT NULL FROM rooms WHERE id = ?`, roomID).Scan(&name, &archived); err != nil || !archived {
		return err
	}
	err = nameReplica(ctx, tx, roomID, name, host, true)
	if errors.Is(err, errNameTaken) {
		return refused("member event: %v", err)
	}
	return err
}

// localMember is whether replica ? has a member of this server.
const localMember = `EXISTS (SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id
	WHERE m.room_id = ? AND k.remote_domain IS NULL)`

// applyRemove removes a member from the replica. When no bot of ours is
// left, the replica is archived. A bot of ours out of a feed no longer
// follows the feed's bot.
func (s *Service) applyRemove(ctx context.Context, tx *sql.Tx, roomID string, payload json.RawMessage) error {
	var p fedrooms.RemovePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return refused("member.remove event: bad payload")
	}
	key, local, err := s.memberKey(ctx, tx, p.Address, false)
	if err != nil || key == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id = ? AND key_id = ?`, roomID, key); err != nil || !local {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_key_id = ? AND target_key_id IS NULL
		AND target = (SELECT feed FROM rooms WHERE id = ?)`, key, roomID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE rooms SET archived_at = COALESCE(archived_at, ?) WHERE id = ? AND NOT `+localMember,
		storage.FormatTS(time.Now()), roomID, roomID)
	return err
}

// applyDelete deletes our copy of the message the host deleted. When we
// do not have it, it stores a tombstone: a deleted row with its host id
// and no content, so the message's event or the host's answer to its
// proposal, arriving later, finds it stored and stores nothing.
func applyDelete(ctx context.Context, tx *sql.Tx, roomID string, payload json.RawMessage) error {
	var p fedrooms.DeletePayload
	if err := json.Unmarshal(payload, &p); err != nil || p.ID <= 0 {
		return refused("message.delete event: bad payload")
	}
	now := storage.FormatTS(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_at = ? WHERE room_id = ? AND remote_id = ? AND deleted_at IS NULL`,
		now, roomID, p.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type,
		trust_origin, remote_id, deleted_at) SELECT id, owner_key_id, ?, '', 'text/plain', 'system', 'external', ?, ? FROM rooms
		WHERE id = ? ON CONFLICT DO NOTHING`, now, p.ID, now, roomID)
	return err
}

// memberKey returns the key that stands for address in a replica, and
// whether it is a key of this server. An address of this server is our
// own key: handle@, operator@ (the operator key) or key-<id>@. Any other
// is its stand-in, created when create is set. It returns 0 for an
// address without a key.
func (s *Service) memberKey(ctx context.Context, tx *sql.Tx, address string, create bool) (id int64, local bool, err error) {
	name, domain, ok := strings.Cut(address, "@")
	if !ok {
		return 0, false, refused("invalid address %q", address)
	}
	if domain != s.id.Domain {
		if create {
			if id, err = fedrooms.StandIn(ctx, tx, address); errors.Is(err, fedrooms.ErrAddress) {
				err = refused("%v", err)
			}
			return id, false, err
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE label = ? AND remote_domain = ?`, address, domain).Scan(&id)
	} else {
		local = true
		q, arg := `SELECT id FROM api_keys WHERE handle = ? AND remote_domain IS NULL`, any(name)
		if name == "operator" {
			q, arg = `SELECT k.id FROM api_keys k JOIN meta m ON m.key = ? AND m.value = CAST(k.id AS TEXT)`, fedrooms.OperatorMetaKey
		} else if n, ok := strings.CutPrefix(name, "key-"); ok {
			if kid, err := strconv.ParseInt(n, 10, 64); err == nil {
				q, arg = `SELECT id FROM api_keys WHERE id = ? AND remote_domain IS NULL`, kid
			}
		}
		err = tx.QueryRowContext(ctx, q, arg).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, local, nil
	}
	return id, local, err
}

// applyMessage stores a message of the replica. Its author signature is
// checked first; then its author key is found or created, and reply_to
// translated to our id. A reply_to we do not have is dropped. A
// signature stored here with another host id is a replay: refused. A
// blanked event (applyEvent applies it as a delete) stores nothing.
func (s *Service) applyMessage(ctx context.Context, roomID, host string, payload json.RawMessage) error {
	var p fedrooms.MessagePayload
	if err := json.Unmarshal(payload, &p); err != nil || p.ID <= 0 {
		return refused("message event: bad payload")
	}
	if p.Deleted {
		return nil
	}
	unverified, err := s.checkAuthor(ctx, roomID, host, p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	author, _, err := s.memberKey(ctx, tx, p.Author, true)
	if err != nil {
		return err
	}
	if author == 0 {
		return refused("message event: no key of ours is %s", p.Author)
	}
	if p.OriginSig != "" {
		var replay bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE room_id = ? AND origin_sig = ?
			AND remote_id IS NOT ?)`, roomID, p.OriginSig, p.ID).Scan(&replay); err != nil {
			return err
		}
		if replay {
			return refused("message event: the author signature of another message")
		}
	}
	var reply *int64
	if p.ReplyTo != nil {
		var id int64
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE room_id = ? AND remote_id = ?`, roomID, *p.ReplyTo).Scan(&id); {
		case err == nil:
			reply = &id
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	meta, err := localMetadata(p.Metadata, reply)
	if err != nil {
		return err
	}
	_, err = s.Rooms.Replicate(ctx, roomID, fedrooms.ReplicaMessage{Author: author, ContentType: p.ContentType,
		MessageType: p.MessageType, Content: p.Content, Metadata: meta, RemoteID: p.ID, OriginSig: p.OriginSig, Unverified: unverified})
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		return refused("message event: %s", fe.Message)
	}
	return err
}

// checkAuthor checks the author signature of p, a message of replica
// roomID hosted by host. A message of one of the host's keys needs none:
// the host's request signature covers it. Any other author's server
// signed it: we check our own with our key and a third server's with
// its stored key, if it is an allowed or seen peer. It reports whether
// the signature could not be checked (no key known for the author's
// server, or that server blocked); a wrong one is errRefused.
func (s *Service) checkAuthor(ctx context.Context, roomID, host string, p fedrooms.MessagePayload) (unverified bool, err error) {
	_, domain, _ := strings.Cut(p.Author, "@")
	if domain == host {
		return false, nil
	}
	pub := s.id.Public
	if domain != s.id.Domain {
		peer, err := s.peers.Get(ctx, domain)
		switch {
		case errors.Is(err, ErrPeerNotFound):
			return true, nil
		case err != nil:
			return false, err
		case peer.Status != statusAllowed && peer.Status != statusSeen, len(peer.PublicKey) != ed25519.PublicKeySize:
			return true, nil
		}
		pub = peer.PublicKey
	}
	m := MessageSig{Room: roomID + "@" + host, Author: p.Author, ContentType: p.ContentType, MessageType: p.MessageType,
		Content: p.Content, Metadata: p.Metadata, ReplyTo: p.ReplyTo, Nonce: p.Nonce}
	if !VerifyMessage(pub, m, p.OriginSig) {
		return false, refused("the author signature of %s does not verify", p.Author)
	}
	return false, nil
}

// localMetadata is a replica message's metadata as stored here: the
// host's wire form, with reply_to as our id when we have the message.
func localMetadata(wire json.RawMessage, reply *int64) ([]byte, error) {
	var m map[string]json.RawMessage
	if len(wire) > 0 {
		if err := json.Unmarshal(wire, &m); err != nil {
			return nil, refused("message event: metadata is not an object")
		}
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	delete(m, "reply_to") // travels in its own field
	if reply != nil {
		m["reply_to"] = json.RawMessage(strconv.FormatInt(*reply, 10))
	}
	if len(m) == 0 {
		return nil, nil
	}
	return fedrooms.Marshal(m)
}

// pull fetches and applies the events of replica roomID after the last
// it applied from its host, page by page, until a page brings nothing
// new. Each asks for pages within what this server reads (max). Refused
// events go to rf. It returns the last seq applied.
func (s *Service) pull(ctx context.Context, roomID, host string, rf *refusals) (int64, error) {
	var applied int64
	if err := s.db.QueryRowContext(ctx, `SELECT applied_seq FROM rooms WHERE id = ?`, roomID).Scan(&applied); err != nil {
		return 0, err
	}
	for range maxPullPages {
		status, body, err := s.sendSigned(ctx, http.MethodGet, host,
			fmt.Sprintf("/fed/v1/rooms/%s/events?since=%d&max=%d", roomID, applied, s.eventsLimit()), nil, s.eventsLimit())
		if err != nil {
			return applied, err
		}
		var out struct {
			Events []fedrooms.Event `json:"events"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &out) != nil || len(out.Events) > maxRoomEvents {
			return applied, fmt.Errorf("events of %s: status %d", host, status)
		}
		next, _, err := s.applyBatch(ctx, roomID, host, out.Events, rf)
		if err != nil || next == applied {
			return next, err
		}
		applied = next
	}
	return applied, nil
}

// catchupLoop pulls what each active replica missed, at start and every
// 5 minutes, until ctx is done.
func (s *Service) catchupLoop(ctx context.Context) {
	t := time.NewTicker(catchupEvery)
	defer t.Stop()
	for {
		s.catchUp(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// catchUp pulls for each active replica whose host is an allowed peer:
// the hosts at once, at most maxPeerRuns, each within the peer budget, its
// rooms one after the other and each pull within 30 seconds, so a slow
// host holds up only its own rooms. It returns when all are done.
func (s *Service) catchUp(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.home_domain FROM rooms r JOIN federation_peers p ON p.domain = r.home_domain
		WHERE p.status = 'allowed' AND r.archived_at IS NULL ORDER BY r.id`)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("federation catch-up failed", "err", err)
		}
		return
	}
	byHost := map[string][]string{}
	for rows.Next() {
		var id, host string
		if err := rows.Scan(&id, &host); err == nil {
			byHost[host] = append(byHost[host], id)
		}
	}
	_ = rows.Close()
	slots := make(chan struct{}, maxPeerRuns)
	var wg sync.WaitGroup
	for host, rooms := range byHost {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			hctx, cancel := context.WithTimeout(ctx, s.budget)
			defer cancel()
			for _, room := range rooms {
				if hctx.Err() != nil {
					return
				}
				pctx, cancel := context.WithTimeout(hctx, pullTimeout)
				rf := &refusals{}
				if _, err := s.pull(pctx, room, host, rf); err != nil && ctx.Err() == nil {
					s.logger.Warn("federation catch-up failed", "room", room, "host", host, "err", err)
				}
				cancel()
				s.auditRefused(ctx, room, host, rf)
			}
		}()
	}
	wg.Wait()
}

// dropPeer cuts the rooms shared with domain once it is blocked or
// removed: its bots' stand-ins leave the rooms hosted here, with
// member.remove events, delivery to it pauses, our replicas of its
// rooms are archived, and our bots' follows of its bots, pending and
// accepted, end.
func (s *Service) dropPeer(ctx context.Context, domain string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT m.room_id, m.key_id FROM room_members m JOIN api_keys k ON k.id = m.key_id
		JOIN rooms r ON r.id = m.room_id WHERE k.remote_domain = ? AND r.home_domain IS NULL`, domain)
	if err != nil {
		return err
	}
	type membership struct {
		room string
		key  int64
	}
	var ms []membership
	for rows.Next() {
		var m membership
		if err := rows.Scan(&m.room, &m.key); err != nil {
			_ = rows.Close()
			return err
		}
		ms = append(ms, m)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, m := range ms {
		if _, err := s.leaveHosted(ctx, m.room, m.key); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE fed_delivery SET paused = 1 WHERE peer = ?`, domain); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE rooms SET archived_at = ? WHERE home_domain = ? AND archived_at IS NULL`,
		storage.FormatTS(time.Now()), domain); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM follows WHERE target_key_id IS NULL
		AND substr(target, instr(target, '@') + 1) = ?`, domain); err != nil {
		return err
	}
	s.DeliverSoon()
	return nil
}

// leaveHosted removes stand-in keyID from room roomID hosted here and
// logs its member.remove event, in one transaction; out of a feed, its
// follow of the feed's bot ends. It reports whether keyID was a member.
func (s *Service) leaveHosted(ctx context.Context, roomID string, keyID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id = ? AND key_id = ?`, roomID, keyID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := fedrooms.LogRemove(ctx, tx, roomID, keyID, s.id.Domain); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_key_id = ? AND target = (SELECT feed FROM rooms WHERE id = ?)`,
		keyID, roomID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Writes from this server's bots in replicas: messages, claims and
// leaving go to the room's host, which decides; a message the host
// stored is applied here like a pushed one.

// ourAuthor returns the host of replica roomID and the address there of
// our key keyID, handle@ourdomain. A key without a handle has no stand-in
// on any host: 403 no_handle.
func (s *Service) ourAuthor(ctx context.Context, roomID string, keyID int64) (host, address string, err error) {
	var handle string
	if err := s.db.QueryRowContext(ctx, `SELECT r.home_domain, COALESCE(k.handle, '') FROM rooms r JOIN api_keys k ON k.id = ?
		WHERE r.id = ? AND r.home_domain IS NOT NULL`, keyID, roomID).Scan(&host, &handle); err != nil {
		return "", "", err
	}
	if handle == "" {
		return "", "", &fedrooms.Error{Status: http.StatusForbidden, Code: "no_handle",
			Message: "only a bot with a handle can write to a room hosted on another server"}
	}
	return host, handle + "@" + s.id.Domain, nil
}

// unreachable is the refusal for a forwarded request the host did not
// answer: no answer, our signature refused (401) or a failure (5xx).
// It is nil for any other answer.
func (s *Service) unreachable(host string, status int, err error) error {
	switch {
	case err != nil:
		if s.warnLog.allow(host + " host_unreachable") {
			s.logger.Warn("federation room host unreachable", "host", host, "err", err)
		}
		return &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: host + " did not answer"}
	case status == http.StatusUnauthorized || status >= 500:
		return &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: fmt.Sprintf("%s answered with status %d", host, status)}
	}
	return nil
}

// codeRE is an error code as this server writes them.
var codeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// errCode is the error code of body, an error answer of a peer, or def
// when it has none.
func errCode(body []byte, def string) string {
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || !codeRE.MatchString(env.Error.Code) {
		return def
	}
	return env.Error.Code
}

// hostError is host's 4xx answer as a refusal: its status, its code
// (host_refused when it is none), and its message marked as the host's,
// on one line, at most 200 characters of it. Nothing else of the body
// is kept.
func hostError(host string, status int, body []byte) error {
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	code, msg := errCode(body, "host_refused"), oneLine(env.Error.Message, 200)
	if msg == "" {
		msg = fmt.Sprintf("status %d", status)
	}
	return &fedrooms.Error{Status: status, Code: code, Message: "host " + host + " refused: " + msg}
}

// oneLine is s as agent.OneLine prints it, cut to max characters.
func oneLine(s string, max int) string {
	r := []rune(agent.OneLine(s))
	return string(r[:min(len(r), max)])
}

// ForwardMessage sends m, a message of our key author in replica
// roomID, to the room's host with our author signature, and applies the
// event the host answers like a pushed one, so the message is stored
// and published here once, whichever comes first. It returns the
// message's id here. m's metadata is in its wire form and its reply_to
// is the host's id. A refusal, the host's 4xx included, is a
// *fedrooms.Error; nothing is stored then.
func (s *Service) ForwardMessage(ctx context.Context, roomID string, author int64, m fedrooms.MessagePayload) (int64, error) {
	host, addr, err := s.ourAuthor(ctx, roomID, author)
	if err != nil {
		return 0, err
	}
	nonce := newNonce()
	sig := s.id.SignMessage(MessageSig{Room: roomID + "@" + host, Author: addr, ContentType: m.ContentType,
		MessageType: m.MessageType, Content: m.Content, Metadata: m.Metadata, ReplyTo: m.ReplyTo, Nonce: nonce})
	body := rawJSON(struct {
		Author      string          `json:"author"`
		Content     string          `json:"content"`
		ContentType string          `json:"content_type"`
		MessageType string          `json:"message_type"`
		Metadata    json.RawMessage `json:"metadata,omitempty"`
		ReplyTo     *int64          `json:"reply_to,omitempty"`
		Sig         string          `json:"sig"`
		Nonce       string          `json:"nonce"`
	}{addr, m.Content, m.ContentType, m.MessageType, m.Metadata, m.ReplyTo, sig, nonce})
	status, out, err := s.sendSigned(ctx, http.MethodPost, host, "/fed/v1/rooms/"+roomID+"/messages", body, s.eventsLimit())
	if err := s.unreachable(host, status, err); err != nil {
		return 0, err
	}
	if status >= 400 {
		return 0, hostError(host, status, out)
	}
	var ev fedrooms.Event
	var p fedrooms.MessagePayload
	if status != http.StatusCreated || json.Unmarshal(out, &ev) != nil || ev.Type != fedrooms.EventMessage ||
		json.Unmarshal(ev.Payload, &p) != nil || p.ID <= 0 || p.Author != addr || p.OriginSig != sig {
		return 0, &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: host + " did not answer with the message's event"}
	}
	return s.applyAnswered(ctx, roomID, host, ev, p.ID)
}

// applyAnswered applies ev, the event of message remoteID that the host
// answered, through the replica's apply path, and returns the message's
// id here. Events before it that the replica lacks are pulled first, so
// the message is stored after them. If they cannot be, it is stored out
// of order; applying it again in order finds it stored.
func (s *Service) applyAnswered(ctx context.Context, roomID, host string, ev fedrooms.Event, remoteID int64) (int64, error) {
	rf := &refusals{}
	defer s.auditRefused(ctx, roomID, host, rf)
	_, gap, err := s.applyBatch(ctx, roomID, host, []fedrooms.Event{ev}, rf)
	if err != nil {
		return 0, err
	}
	if gap {
		pctx, cancel := context.WithTimeout(ctx, pullTimeout)
		if _, err := s.pull(pctx, roomID, host, rf); err != nil {
			s.logger.Warn("federation room pull failed", "room", roomID, "host", host, "err", err)
		}
		cancel()
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE room_id = ? AND remote_id = ?`, roomID, remoteID).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	s.applyMu.Lock()
	err = s.applyMessage(ctx, roomID, host, ev.Payload)
	s.applyMu.Unlock()
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE room_id = ? AND remote_id = ?`, roomID, remoteID).Scan(&id)
	return id, err
}

// ForwardClaims sends r, a claims request (list, claim, heartbeat or
// release) of our key author in replica roomID, to the room's host for
// the author's address, and returns the host's status with an answer
// rebuilt from it: nothing of the host's body reaches our agents as it
// is. Each claim and conflict keeps its known fields, checked (a path
// or held_path that is no claim path, an expires_at that is no RFC 3339
// time, an id or auto of the wrong type drops it), and its note on one
// line, at most 200 characters. Its holder is our key for its address
// (claimHolder; no key drops it), its label that address and its trust
// external: forwarded claims are the host's word, and a bot finds its
// own claims by key id. A 409's message is rebuilt from its conflicts.
// A 4xx other than 409 is a hostError; a refusal (no handle, host
// unreachable or answering something else) is a *fedrooms.Error.
func (s *Service) ForwardClaims(r *http.Request, roomID string, author int64) (int, []byte, error) {
	ctx := r.Context()
	host, addr, err := s.ourAuthor(ctx, roomID, author)
	if err != nil {
		return 0, nil, err
	}
	var body []byte
	if r.Method != http.MethodGet {
		if body, err = io.ReadAll(io.LimitReader(r.Body, maxBody+1)); err != nil {
			return 0, nil, err
		}
		if len(body) > maxBody {
			return 0, nil, &fedrooms.Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: "body over 64 KiB"}
		}
	}
	_, sub, _ := strings.Cut(r.URL.Path, "/rooms/"+roomID) // /claims, /claims/heartbeat or /claims/release
	// ponytail: claim lists are read up to the events limit, which bounds
	// what one answer of the host may cost.
	status, out, err := s.sendSigned(ctx, r.Method, host,
		"/fed/v1/rooms/"+roomID+sub+"?"+url.Values{"author": {addr}}.Encode(), body, s.eventsLimit())
	if err := s.unreachable(host, status, err); err != nil {
		return 0, nil, err
	}
	badAnswer := &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: host + " did not answer with claims"}
	switch {
	case status == http.StatusConflict, status/100 == 2:
	case status/100 == 4:
		return 0, nil, hostError(host, status, out)
	default:
		return 0, nil, badAnswer
	}
	var in struct {
		Claims    []json.RawMessage `json:"claims"`
		Conflicts []json.RawMessage `json:"conflicts"`
		Refreshed *int64            `json:"refreshed"`
		Released  []json.RawMessage `json:"released"`
	}
	if json.Unmarshal(out, &in) != nil {
		return 0, nil, badAnswer
	}
	var answer any
	switch {
	case status == http.StatusConflict:
		conflicts, reasons := []conflictView{}, []string{}
		err = s.eachHostClaim(ctx, roomID, in.Conflicts, true, func(c hostClaim, key int64) {
			conflicts = append(conflicts, conflictView{Path: c.Path, HeldPath: c.HeldPath, KeyID: key, Label: c.Address,
				ExpiresAt: c.ExpiresAt, TrustOrigin: "external"})
			reasons = append(reasons, fmt.Sprintf("%s is claimed by %s (EXTERNAL, untrusted) (claim %s, until %s)",
				c.Path, c.Address, c.HeldPath, c.ExpiresAt))
		})
		msg := cmp.Or(strings.Join(reasons, "; "), "the paths are claimed on "+host)
		answer = map[string]any{"error": map[string]any{"code": "conflict", "message": msg, "request_id": r.Header.Get("X-Request-Id")},
			"conflicts": conflicts}
	case sub == "/claims":
		list := []claims.Claim{}
		err = s.eachHostClaim(ctx, roomID, in.Claims, false, func(c hostClaim, key int64) {
			list = append(list, claims.Claim{ID: c.ID, Path: c.Path, KeyID: key, Label: c.Address, Auto: c.Auto, Note: c.Note,
				ExpiresAt: c.ExpiresAt, TrustOrigin: "external"})
		})
		answer = map[string]any{"claims": list}
	case sub == "/claims/heartbeat" && in.Refreshed != nil:
		answer = map[string]any{"refreshed": *in.Refreshed}
	case sub == "/claims/release":
		released := []string{}
		for _, raw := range in.Released {
			var p string
			if json.Unmarshal(raw, &p) == nil {
				if p, err := claims.Normalize(p); err == nil {
					released = append(released, p)
				}
			}
		}
		answer = map[string]any{"released": released}
	default:
		return 0, nil, badAnswer
	}
	if err != nil {
		return 0, nil, err
	}
	return status, rawJSON(answer), nil
}

// hostClaim is a claim or conflict of a host's claims answer: the
// fields kept, typed. Key ids, labels and trust are the host's and
// never read; the holder is named by its address.
type hostClaim struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	HeldPath  string `json:"held_path"`
	Address   string `json:"address"`
	Auto      bool   `json:"auto"`
	Note      string `json:"note"`
	ExpiresAt string `json:"expires_at"`
}

// conflictView is a conflict of a 409 claims answer, as the claims
// handler writes it.
type conflictView struct {
	Path        string `json:"path"`
	HeldPath    string `json:"held_path"`
	KeyID       int64  `json:"key_id"`
	Label       string `json:"label"`
	ExpiresAt   string `json:"expires_at"`
	TrustOrigin string `json:"trust_origin"`
}

// eachHostClaim calls f with each claim of list that passes the checks
// (held_path too for a conflict), cleaned, and its holder's key here.
// The others are dropped.
func (s *Service) eachHostClaim(ctx context.Context, roomID string, list []json.RawMessage, conflict bool,
	f func(c hostClaim, key int64)) error {
	for _, raw := range list {
		var c hostClaim
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil {
			continue
		}
		c.ExpiresAt = exp.UTC().Format(time.RFC3339)
		if c.Path, err = claims.Normalize(c.Path); err != nil {
			continue
		}
		if conflict {
			if c.HeldPath, err = claims.Normalize(c.HeldPath); err != nil {
				continue
			}
		}
		c.Note = oneLine(c.Note, 200)
		key, err := s.claimHolder(ctx, roomID, c.Address)
		if err != nil {
			return err
		}
		if key != 0 {
			f(c, key)
		}
	}
	return nil
}

// claimHolder returns our key for the holder address of a claim in
// replica roomID, or 0. An address of this server is one of our bots
// that is a member of the room, never operator@ or key-<id>@; any
// other is its stand-in, if we have one: claim answers make none, only
// member events and message authors do.
func (s *Service) claimHolder(ctx context.Context, roomID, address string) (int64, error) {
	name, domain, _ := strings.Cut(address, "@")
	q, args := `SELECT id FROM api_keys WHERE label = ? AND remote_domain = ?`, []any{address, domain}
	if domain == s.id.Domain {
		q, args = `SELECT k.id FROM api_keys k JOIN room_members m ON m.key_id = k.id AND m.room_id = ?
			WHERE k.handle = ? AND k.remote_domain IS NULL AND k.is_admin = 0 AND k.revoked_at IS NULL`, []any{roomID, name}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// LeaveRemote tells the host of replica roomID that our bot keyID leaves
// the room. The host agrees with 204, or with 404 when it no longer has
// the bot in the room; any other answer is a refusal.
// A host that is blocked or no allowed peer is not told: the bot leaves
// here only, which the audit row notes. A refusal is a
// *fedrooms.Error.
func (s *Service) LeaveRemote(ctx context.Context, roomID string, keyID int64) error {
	host, addr, err := s.ourAuthor(ctx, roomID, keyID)
	if err != nil {
		return err
	}
	switch p, err := s.peers.Get(ctx, host); {
	case err != nil && !errors.Is(err, ErrPeerNotFound):
		return err
	case p.Status != statusAllowed:
		detail, _ := json.Marshal(map[string]any{"address": addr, "local_only": true})
		s.auditRoom(ctx, "federation.room.leave", host, roomID, "ok", string(detail))
		return nil
	}
	status, out, err := s.sendSigned(ctx, http.MethodPost, host, "/fed/v1/rooms/"+roomID+"/leave",
		rawJSON(map[string]string{"author": addr}), maxBody)
	// 204 is the host's agreement, and 404 (no such member there); a
	// redirect, which the client does not follow, or another success is
	// no answer.
	switch err = s.unreachable(host, status, err); {
	case err != nil, status == http.StatusNoContent, status == http.StatusNotFound:
	case status >= 400:
		err = hostError(host, status, out)
	default:
		err = &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: fmt.Sprintf("%s answered with status %d", host, status)}
	}
	detail, _ := json.Marshal(map[string]string{"address": addr})
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		s.auditRoom(ctx, "federation.room.leave", host, roomID, "error:"+fe.Code, string(detail))
		return err
	}
	s.auditRoom(ctx, "federation.room.leave", host, roomID, "ok", string(detail))
	return nil
}
