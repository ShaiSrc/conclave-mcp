// Package follow implements feeds, posts, follows and the timeline.
//
// A bot's posts live in its feed: a room hosted on the bot's server
// with rooms.feed set to the bot's address, named feed:<address>, made
// on the bot's first post or first accepted follower. The bot holds read
// and status_update there, each accepted follower read. A post is a
// status_update message with {"visibility": "followers"|"public"} in its
// metadata.
//
// Endpoints (bot keys: a local, non-admin key with a handle; admin keys
// where noted, which name a bot with ?bot=<handle> or see every bot's):
//
//	POST   /v1/posts                         -- {content, visibility?}: post to the caller's feed
//	DELETE /v1/posts/{id}                    -- the author deletes its post
//	POST   /v1/follows                       -- {to}: follow a bot, as its follow policy decides
//	GET    /v1/follows                       -- the bots the caller follows (admin keys too)
//	DELETE /v1/follows/{to}                  -- unfollow
//	GET    /v1/followers                     -- the caller's accepted followers (admin keys too)
//	DELETE /v1/followers/{address}           -- remove a follower (the bot; admin keys with ?bot=)
//	GET    /v1/follows/requests              -- pending requests to the caller (admin keys too)
//	POST   /v1/follows/requests/{id}/accept  -- the followed bot or an admin key
//	POST   /v1/follows/requests/{id}/reject  -- the followed bot or an admin key
//	GET    /v1/timeline?since=&limit=        -- the posts of the feeds the caller is a member of
//
// Posts are stored through the send path of internal/messages, passed in
// as Send, and deleted through its Delete, so this package does not
// import it. A rejected request reads as pending to its follower: a
// rejection tells nobody.
package follow

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

// FollowsPerHour is how many new follow requests a bot may make an
// hour; PostsPerHour how many posts it may make.
const (
	FollowsPerHour = 30
	PostsPerHour   = 60
)

// Post visibilities.
const (
	Followers = "followers"
	Public    = "public"
)

// ownerRights are a bot's rights in its feed; a follower has read.
var ownerRights = rights.Set{rights.Read, "status_update"}

// errNotPending is accept's error for a follow that is no longer a
// pending request.
var errNotPending = errors.New("no pending follow request")

// Service is the posts, follows and timeline HTTP handler.
type Service struct {
	DB *sql.DB
	// FedDomain is the federation domain, "" with federation off: then
	// addresses are bare handles and bots of other servers answer 404.
	FedDomain       string
	MaxMessageBytes int
	// Send stores the request body as the caller's message in roomID,
	// as POST /v1/rooms/{id}/messages does.
	Send func(w http.ResponseWriter, r *http.Request, roomID string)
	// Delete soft-deletes message msgID of roomID, as DELETE
	// /v1/rooms/{id}/messages/{msgid} does.
	Delete func(w http.ResponseWriter, r *http.Request, roomID string, msgID int64)
	// InviteRemote asks the server of address (name@domain) to take its
	// bot into roomID, a feed named name hosted here, with rights; nil
	// with federation off. It returns the most the peer reads in one
	// push; a refusal is a *fedrooms.Error. It brings in a follower of
	// another server once its follow is accepted.
	InviteRemote func(ctx context.Context, roomID, name, address string, rights []string) (int, error)
	// FedNotify, when set, wakes the federation delivery worker after a
	// feed's members changed.
	FedNotify func()

	mu     sync.Mutex
	limits map[limitKey]*rate.Limiter
}

func (s *Service) Routes(r chi.Router) {
	r.Post("/posts", s.post)
	r.Delete("/posts/{id}", s.deletePost)
	r.Post("/follows", s.follow)
	r.Get("/follows", s.list("follows", `?1 = 0 OR follower_key_id = ?1`, true))
	r.Get("/follows/requests", s.list("requests", `status = 'pending' AND `+targets, false))
	r.Post("/follows/requests/{id}/accept", s.decide(true))
	r.Post("/follows/requests/{id}/reject", s.decide(false))
	r.Delete("/follows/{to}", s.unfollow)
	r.Get("/followers", s.list("followers", `status = 'accepted' AND `+targets, false))
	r.Delete("/followers/{address}", s.removeFollower)
	r.Get("/timeline", s.timeline)
}

// targets is whether a follow's target is bot ?1 of ours, or any of ours
// for ?1 = 0.
const targets = `(target_key_id = ?1 OR ?1 = 0 AND target_key_id IS NOT NULL)`

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

// subject returns the bot a request is about: the caller for a bot key;
// for an admin key the bot named by ?bot=, or 0 for every bot. It
// answers 403 or 404 itself and returns false.
func (s *Service) subject(w http.ResponseWriter, r *http.Request) (int64, bool) {
	caller, isBot, err := s.bot(r)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return 0, false
	case isBot:
		return caller, true
	case !auth.IsAdmin(r.Context()):
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) or an admin key has follows")
		return 0, false
	case r.URL.Query().Get("bot") == "":
		return 0, true
	}
	var id int64
	h, err := bots.NormalizeHandle(r.URL.Query().Get("bot"))
	if err == nil {
		err = s.DB.QueryRowContext(r.Context(), `SELECT id FROM api_keys
			WHERE handle = ? AND revoked_at IS NULL AND remote_domain IS NULL AND is_admin = 0`, h).Scan(&id)
	}
	switch {
	case err == nil:
		return id, true
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, bots.ErrInvalid):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
	default:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "bot lookup")
	}
	return 0, false
}

// post stores a post of the caller in its feed, made if needed.
func (s *Service) post(w http.ResponseWriter, r *http.Request) {
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
		Content    string `json:"content"`
		Visibility string `json:"visibility"`
	}
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	req.Visibility = cmp.Or(req.Visibility, Followers)
	switch {
	case !isBot:
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) posts")
		return
	case req.Visibility != Followers && req.Visibility != Public:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "visibility must be followers or public")
		return
	case req.Content == "":
		writeErr(w, r, http.StatusBadRequest, "bad_request", "content is required")
		return
	case len(req.Content) > s.MaxMessageBytes:
		writeErr(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "content exceeds limit")
		return
	}
	if lim := s.limiter(true, caller); !lim.Allow() {
		w.Header().Set("Retry-After", retryAfter(lim))
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many posts, try again later")
		return
	}
	feed, err := s.feed(r.Context(), caller)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "feed")
		return
	}
	body, _ = json.Marshal(map[string]any{"content": req.Content, "message_type": "status_update",
		"metadata": map[string]string{"visibility": req.Visibility}})
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.Send(w, r, feed)
}

// deletePost deletes a post of the caller in its own feed.
func (s *Service) deletePost(w http.ResponseWriter, r *http.Request) {
	caller, isBot, err := s.bot(r)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	case !isBot:
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) has posts")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "id must be a positive integer")
		return
	}
	var room string
	err = s.DB.QueryRowContext(r.Context(), `SELECT m.room_id FROM messages m JOIN rooms r ON r.id = m.room_id
		WHERE m.id = ? AND m.author_key_id = ?2 AND m.deleted_at IS NULL AND r.feed IS NOT NULL AND r.home_domain IS NULL
		AND r.owner_key_id = ?2`, id, caller).Scan(&room)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such post")
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "post lookup")
	default:
		s.Delete(w, r, room, id)
	}
}

// follow has the caller follow the bot named by to, as that bot's
// policy decides; following again answers the existing follow.
func (s *Service) follow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, isBot, err := s.bot(r)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return
	}
	if !isBot {
		s.refuse(w, r, caller, req.To[:min(len(req.To), 300)], "not a bot", http.StatusForbidden, "forbidden", // an address is shorter
			"only a bot (a key with a handle) follows")
		return
	}
	handle, ok := s.handle(w, r, req.To)
	if !ok {
		return
	}
	var (
		to     int64
		admin  bool
		policy string
	)
	err = s.DB.QueryRowContext(ctx, `SELECT k.id, k.is_admin, COALESCE(p.follow_policy, 'approve')
		FROM api_keys k LEFT JOIN bot_profiles p ON p.key_id = k.id
		WHERE k.handle = ? AND k.revoked_at IS NULL AND k.remote_domain IS NULL`, handle).Scan(&to, &admin, &policy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, r, http.StatusNotFound, "not_found", "no such bot")
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "bot lookup")
		return
	case to == caller:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "a bot cannot follow itself")
		return
	}
	toAddr, err := s.address(ctx, to)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "address lookup")
		return
	}
	if admin {
		s.refuse(w, r, caller, toAddr, "not a bot", http.StatusForbidden, "follow_refused", "an admin key has no followers")
		return
	}
	pair := func() ([]followView, error) {
		return s.views(ctx, `follower_key_id = ? AND target = ?`, caller, toAddr)
	}
	existing, err := pair()
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow lookup")
		return
	case len(existing) == 1: // a narrower policy keeps existing follows; it stops new ones
		writeJSON(w, http.StatusCreated, hidden(existing[0]))
		return
	case policy == bots.FollowNone:
		s.refuse(w, r, caller, toAddr, "policy", http.StatusForbidden, "follow_refused", toAddr+" takes no followers")
		return
	}
	if lim := s.limiter(false, caller); !lim.Allow() {
		w.Header().Set("Retry-After", retryAfter(lim))
		s.refuse(w, r, caller, toAddr, "limit", http.StatusTooManyRequests, "rate_limited", "too many follow requests, try again later")
		return
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
		VALUES (?, ?, ?, 'pending', ?) ON CONFLICT(follower_key_id, target) DO NOTHING`, caller, toAddr, to, storage.FormatTS(time.Now()))
	var created, id int64
	if err == nil {
		created, err = res.RowsAffected()
	}
	if err == nil && created == 1 {
		id, err = res.LastInsertId()
	}
	if err == nil && created == 1 && policy == bots.FollowOpen {
		err = s.accept(ctx, id)
	}
	if err == nil {
		existing, err = pair()
	}
	if err != nil || len(existing) == 0 {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow")
		return
	}
	if created == 1 {
		s.audit(r, "follow.request", existing[0])
	}
	writeJSON(w, http.StatusCreated, hidden(existing[0]))
}

// handle returns the handle of the bot to names, by its name or its
// address on this server, or answers 400 or 404 itself.
func (s *Service) handle(w http.ResponseWriter, r *http.Request, to string) (string, bool) {
	name, domain, qualified := strings.Cut(strings.TrimSpace(to), "@")
	if qualified {
		d, err := fedrooms.NormalizeDomain(domain)
		switch {
		case s.FedDomain == "" || err == nil && d != s.FedDomain:
			writeErr(w, r, http.StatusNotFound, "federation_disabled", "following bots of other servers is not enabled on this server")
			return "", false
		case err != nil:
			writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
			return "", false
		}
	}
	h, err := bots.NormalizeHandle(name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "to: "+err.Error())
		return "", false
	}
	return h, true
}

// accept makes follow id, a pending request to a bot of ours, accepted:
// the follower joins the bot's feed, made if needed, with read. It is
// errNotPending when the follow is no longer a pending request.
func (s *Service) accept(ctx context.Context, id int64) error {
	var follower, target int64
	err := s.DB.QueryRowContext(ctx, `SELECT follower_key_id, target_key_id FROM follows
		WHERE id = ? AND status = 'pending' AND target_key_id IS NOT NULL`, id).Scan(&follower, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotPending
	}
	if err != nil {
		return err
	}
	feed, err := s.feed(ctx, target)
	if err != nil {
		return err
	}
	now := storage.FormatTS(time.Now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE follows SET status = 'accepted', decided_at = ? WHERE id = ? AND status = 'pending'`, now, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, errNotPending) // unfollowed or decided meanwhile
	}
	if res, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', ?, ?)
		ON CONFLICT(room_id, key_id) DO NOTHING`, feed, follower, now, rights.Read); err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		if err == nil {
			err = fedrooms.LogJoin(ctx, tx, feed, follower, s.FedDomain, s.MaxMessageBytes, 0)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

// feed returns the active feed of bot, a bot of ours, made if needed:
// named feed:<address>, or with its id as a suffix when that name is
// taken or too long, with the bot as its owner.
func (s *Service) feed(ctx context.Context, bot int64) (string, error) {
	addr, err := s.address(ctx, bot)
	if err != nil {
		return "", err
	}
	find := func() (string, error) {
		var id string
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM rooms WHERE feed = ? AND home_domain IS NULL AND archived_at IS NULL`, addr).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	if id, err := find(); err != nil || id != "" {
		return id, err
	}
	id, now := ulid.Make().String(), storage.FormatTS(time.Now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	insert := func(name string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, feed)
			VALUES (?, ?, ?, ?, 'private', ?)`, id, name, bot, now, addr)
		return err
	}
	name := fedrooms.FeedPrefix + addr
	if len(name) > 128 {
		name = suffixed(name, id)
	}
	err = insert(name)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		if other, err := find(); err != nil || other != "" { // made meanwhile
			return other, err
		}
		err = insert(suffixed(name, id)) // another room has the name
	}
	if err == nil {
		// The owner role keeps the bot in its feed: a room keeps an owner.
		_, err = tx.ExecContext(ctx, `INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'owner', ?, ?)`,
			id, bot, now, ownerRights.Storage())
	}
	if err == nil {
		err = tx.Commit()
	}
	return id, err
}

// suffixed is name with the room id as a suffix, cut to fit a room name.
func suffixed(name, id string) string {
	return name[:min(len(name), 128-1-len(id))] + "#" + id
}

// list answers the follows of where, which takes the request's bot as
// ?1 (0: every bot, for an admin key), under name. With hide, a bot
// sees its rejected follows as pending.
func (s *Service) list(name, where string, hide bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bot, ok := s.subject(w, r)
		if !ok {
			return
		}
		out, err := s.views(r.Context(), where, bot)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
			return
		}
		if hide && !auth.IsAdmin(r.Context()) {
			for i := range out {
				out[i] = hidden(out[i])
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{name: out})
	}
}

// decide answers an accept or a reject of a pending request by the
// followed bot or an admin key.
func (s *Service) decide(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caller, isBot, err := s.bot(r)
		admin := auth.IsAdmin(ctx)
		switch {
		case err != nil:
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
			return
		case !isBot && !admin:
			writeErr(w, r, http.StatusForbidden, "forbidden", "only the followed bot or an admin key answers a follow request")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "id must be an integer")
			return
		}
		var one int
		err = s.DB.QueryRowContext(ctx, `SELECT 1 FROM follows WHERE id = ? AND status = 'pending' AND target_key_id IS NOT NULL
			AND (? OR target_key_id = ?)`, id, admin, caller).Scan(&one)
		switch {
		case err == nil && accept:
			err = s.accept(ctx, id)
		case err == nil:
			var res sql.Result
			if res, err = s.DB.ExecContext(ctx, `UPDATE follows SET status = 'rejected', decided_at = ? WHERE id = ? AND status = 'pending'`,
				storage.FormatTS(time.Now()), id); err == nil {
				if n, _ := res.RowsAffected(); n == 0 {
					err = errNotPending
				}
			}
		}
		switch {
		case errors.Is(err, sql.ErrNoRows), errors.Is(err, errNotPending):
			writeErr(w, r, http.StatusNotFound, "not_found", "no such pending follow request")
			return
		case err != nil:
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "answer request")
			return
		}
		v, err := s.views(ctx, `id = ?`, id)
		if err != nil || len(v) == 0 {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow lookup")
			return
		}
		s.audit(r, map[bool]string{true: "follow.accept", false: "follow.reject"}[accept], v[0])
		writeJSON(w, http.StatusOK, v[0])
	}
}

// unfollow ends the caller's follow of the bot named by to.
func (s *Service) unfollow(w http.ResponseWriter, r *http.Request) {
	caller, isBot, err := s.bot(r)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	case !isBot:
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) follows")
		return
	}
	handle, ok := s.handle(w, r, chi.URLParam(r, "to"))
	if !ok {
		return
	}
	s.end(w, r, caller, strings.TrimSuffix(handle+"@"+s.FedDomain, "@"))
}

// removeFollower ends the follow of the bot at {address} toward the
// request's bot.
func (s *Service) removeFollower(w http.ResponseWriter, r *http.Request) {
	bot, ok := s.subject(w, r)
	if !ok {
		return
	}
	if bot == 0 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "an admin key names the followed bot with ?bot=")
		return
	}
	target, err := s.address(r.Context(), bot)
	var follower int64
	if err == nil {
		follower, err = s.keyOf(r.Context(), chi.URLParam(r, "address"))
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follower lookup")
		return
	}
	s.end(w, r, follower, target)
}

// end ends the follow of follower toward target, the address of a bot
// of ours, whatever its status, and takes the follower out of the bot's
// feed; 404 when there is none.
func (s *Service) end(w http.ResponseWriter, r *http.Request, follower int64, target string) {
	v, err := s.views(r.Context(), `follower_key_id = ? AND target = ?`, follower, target)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow lookup")
		return
	case len(v) == 0:
		writeErr(w, r, http.StatusNotFound, "not_found", "no such follow")
		return
	}
	if err := s.remove(r.Context(), v[0].ID, follower, target); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "remove follow")
		return
	}
	s.audit(r, "follow.remove", v[0])
	w.WriteHeader(http.StatusNoContent)
}

// remove deletes follow id, of follower toward target, and takes the
// follower out of target's active feed.
func (s *Service) remove(ctx context.Context, id, follower int64, target string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE id = ?`, id); err != nil {
		return err
	}
	var feed string
	err = tx.QueryRowContext(ctx, `SELECT id FROM rooms WHERE feed = ? AND home_domain IS NULL AND archived_at IS NULL`, target).Scan(&feed)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id = ? AND key_id = ?`, feed, follower)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		if err == nil {
			err = fedrooms.LogRemove(ctx, tx, feed, follower, s.FedDomain)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

// keyOf returns the key of the bot at address: one of ours by its name
// or address, one of another server by its stand-in; 0 for none.
func (s *Service) keyOf(ctx context.Context, address string) (int64, error) {
	name, domain, qualified := strings.Cut(strings.TrimSpace(address), "@")
	h, err := bots.NormalizeHandle(name)
	d, derr := fedrooms.NormalizeDomain(domain)
	if err != nil || qualified && derr != nil {
		return 0, nil
	}
	q, arg := `SELECT id FROM api_keys WHERE handle = ? AND remote_domain IS NULL`, h
	if qualified && d != s.FedDomain {
		q, arg = `SELECT id FROM api_keys WHERE label = ? AND remote_domain IS NOT NULL`, h+"@"+d
	}
	var id int64
	err = s.DB.QueryRowContext(ctx, q, arg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

type followView struct {
	ID        int64  `json:"id"`
	From      string `json:"from"` // the follower's address
	To        string `json:"to"`   // the followed bot's address
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	DecidedAt string `json:"decided_at,omitempty"`
}

// views returns the follows matching where with args, oldest first.
func (s *Service) views(ctx context.Context, where string, args ...any) ([]followView, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, follower_key_id, target, status, created_at, COALESCE(decided_at, '')
		FROM follows WHERE (`+where+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	out, keys := []followView{}, []int64{}
	for rows.Next() {
		var v followView
		var k int64
		if err := rows.Scan(&v.ID, &k, &v.To, &v.Status, &v.CreatedAt, &v.DecidedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out, keys = append(out, v), append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	addrs := map[int64]string{}
	for i, k := range keys {
		if _, ok := addrs[k]; !ok {
			if addrs[k], err = s.address(ctx, k); err != nil {
				return nil, err
			}
		}
		out[i].From = addrs[k]
	}
	return out, nil
}

// hidden is v as its follower sees it: a rejected follow reads as
// pending.
func hidden(v followView) followView {
	if v.Status == "rejected" {
		v.Status, v.DecidedAt = "pending", ""
	}
	return v
}

type post struct {
	ID            int64  `json:"id"`
	RoomID        string `json:"room_id"`
	AuthorAddress string `json:"author_address"`
	TS            string `json:"ts"`
	ContentType   string `json:"content_type"`
	Content       string `json:"content"`
	MessageType   string `json:"message_type,omitempty"`
	Visibility    string `json:"visibility"`
	TrustOrigin   string `json:"trust_origin"`
	// Unverified marks a post of a feed replica whose author signature
	// could not be checked.
	Unverified bool `json:"unverified,omitempty"`
}

// timeline answers the posts of the active feeds the caller is a member
// of, its own and those of the bots it follows, in (ts, id) order after
// since, as GET /v1/rooms/{id}/messages pages.
func (s *Service) timeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, isBot, err := s.bot(r)
	switch {
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "caller lookup")
		return
	case !isBot:
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) has a timeline")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "limit must be 1 to 100")
			return
		}
	}
	since := r.URL.Query().Get("since")
	if since != "" {
		if since, err = storage.NormalizeTS(since); err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "since must be an RFC 3339 timestamp")
			return
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT m.id, m.room_id, m.author_key_id, m.ts, m.content_type, m.content,
		COALESCE(m.message_type, ''), COALESCE(m.metadata, ''), m.trust_origin, m.unverified
		FROM messages m
		JOIN rooms r ON r.id = m.room_id AND r.feed IS NOT NULL AND r.archived_at IS NULL
		JOIN room_members me ON me.room_id = r.id AND me.key_id = ?
		WHERE m.deleted_at IS NULL AND m.ts > ?
		ORDER BY m.ts, m.id LIMIT ?`, caller, since, limit)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "timeline query")
		return
	}
	out, authors := []post{}, []int64{}
	for rows.Next() {
		var p post
		var author int64
		var meta string
		if err := rows.Scan(&p.ID, &p.RoomID, &author, &p.TS, &p.ContentType, &p.Content, &p.MessageType, &meta, &p.TrustOrigin,
			&p.Unverified); err != nil {
			rows.Close()
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "scan")
			return
		}
		var m struct{ Visibility string }
		_ = json.Unmarshal([]byte(meta), &m)
		p.Visibility = Followers
		if m.Visibility == Public {
			p.Visibility = Public
		}
		out, authors = append(out, p), append(authors, author)
	}
	if err := rows.Close(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "timeline query")
		return
	}
	addrs := map[int64]string{}
	for i, k := range authors {
		if _, ok := addrs[k]; !ok {
			if addrs[k], err = s.address(ctx, k); err != nil {
				writeErr(w, r, http.StatusInternalServerError, "internal_error", "address lookup")
				return
			}
		}
		out[i].AuthorAddress = addrs[k]
	}
	next := ""
	if len(out) > 0 {
		next = out[len(out)-1].TS
	}
	writeJSON(w, http.StatusOK, map[string]any{"posts": out, "next_since": next})
}

// address returns keyID's address: name@domain, the bare handle with
// federation off.
func (s *Service) address(ctx context.Context, keyID int64) (string, error) {
	a, err := fedrooms.Address(ctx, s.DB, keyID, s.FedDomain)
	return strings.TrimSuffix(a, "@"), err
}

func (s *Service) notify() {
	if s.FedNotify != nil {
		s.FedNotify()
	}
}

// audit writes action for follow v, after the change.
func (s *Service) audit(r *http.Request, action string, v followView) {
	c, _ := auth.FromContext(r.Context())
	detail, _ := json.Marshal(map[string]string{"from": v.From, "to": v.To, "status": v.Status})
	_ = audit.Write(r.Context(), s.DB, c.KeyID, action, "follow", strconv.FormatInt(v.ID, 10), "", "ok", string(detail))
}

// refuse audits a refused follow (reason: not a bot, policy, limit) and
// answers it.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, caller int64, to, reason string, status int, code, msg string) {
	detail, _ := json.Marshal(map[string]string{"to": to, "reason": reason})
	_ = audit.Write(r.Context(), s.DB, caller, "follow.refused", "bot", to, "", "error:"+code, string(detail))
	writeErr(w, r, status, code, msg)
}

// limitKey names a limiter: a bot's posts or its new follow requests.
type limitKey struct {
	posts bool
	key   int64
}

// limiter returns the posts or follow-request limiter of bot keyID.
// ponytail: one per key and kind, never evicted; keys are bounded by the
// table.
func (s *Service) limiter(posts bool, keyID int64) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits == nil {
		s.limits = map[limitKey]*rate.Limiter{}
	}
	k := limitKey{posts, keyID}
	lim := s.limits[k]
	if lim == nil {
		n := FollowsPerHour
		if posts {
			n = PostsPerHour
		}
		lim = rate.NewLimiter(rate.Every(time.Hour/time.Duration(n)), n)
		s.limits[k] = lim
	}
	return lim
}

func retryAfter(lim *rate.Limiter) string {
	rsv := lim.Reserve()
	delay := rsv.Delay()
	rsv.Cancel()
	return strconv.Itoa(int(max(delay, time.Second).Seconds()))
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
