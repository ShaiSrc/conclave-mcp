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
//	GET    /v1/timeline?since=&limit=&order= -- the posts of the feeds the caller is a member of
//	                                            (admin keys too: every feed hosted here)
//
// The three lists page by id: ?limit= (1 to 500, default 100) and
// ?after=<id>, answering next_after while more follow.
//
// Posts are stored through the send path of internal/messages, passed in
// as Send, and deleted through its Delete, so this package does not
// import it. A rejected request reads as pending to its follower: a
// rejection tells nobody.
//
// A bot of another server: following it stores a pending follow and
// sends the request to its server (Request), which decides by the bot's
// policy and, when it accepts, invites our bot into the feed; that
// invite (internal/federation) makes the follow accepted here. A
// follower of another server is its stand-in key: its requests come
// through Remote, and accepting one invites its server into the feed
// (InviteRemote) before the stand-in joins. Unfollowing a bot of
// another server leaves the feed's replica through its host (Leave);
// withdrawing a pending follow of one tells its server (Withdraw, and
// RemoteWithdraw on that side).
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
// hour; PostsPerHour how many posts it may make. MaxPending is how many
// pending follow requests a bot takes; beyond it a new one is refused
// with 429, Retry-After an hour.
const (
	FollowsPerHour = 30
	PostsPerHour   = 60
	MaxPending     = 1000
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
	// Send stores the request body as the caller's post in roomID, its
	// feed, through the send path of POST /v1/rooms/{id}/messages:
	// messages.Service.SendPost, the one way into a feed.
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
	// Request sends the follow request of follower, our bot's address, to
	// the server of address (name@domain); nil with federation off. A
	// refusal is a *fedrooms.Error with the peer's status and code; no
	// answer is one with status 502.
	Request func(ctx context.Context, follower, address string) error
	// Leave tells the host of replica roomID that our bot keyID leaves
	// it; nil with federation off. A refusal is a *fedrooms.Error.
	Leave func(ctx context.Context, roomID string, keyID int64) error
	// Withdraw tells the server of address (name@domain) that follower,
	// our bot's address, withdraws its pending follow request; nil with
	// federation off.
	Withdraw func(ctx context.Context, follower, address string) error
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
		writeErr(w, r, http.StatusForbidden, "forbidden", "only a bot (a key with a handle) or an admin key has follows and a timeline")
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
	handle, domain, ok := s.handle(w, r, req.To)
	switch {
	case !ok:
		return
	case domain != "":
		s.followRemote(w, r, caller, handle+"@"+domain, domain)
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
	case len(existing) == 1 && (existing[0].Status != "pending" || policy != bots.FollowOpen):
		// a narrower policy keeps existing follows; it stops new ones
		writeJSON(w, http.StatusCreated, hidden(existing[0]))
		return
	case len(existing) == 1: // pending under open: its accept failed, so again, as Remote does
		if err := s.accept(ctx, existing[0].ID); err != nil && !errors.Is(err, errNotPending) {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow")
			return
		}
		if existing, err = pair(); err != nil || len(existing) == 0 {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow")
			return
		}
		writeJSON(w, http.StatusCreated, hidden(existing[0]))
		return
	case policy == bots.FollowNone:
		s.refuse(w, r, caller, toAddr, "policy", http.StatusForbidden, "follow_refused", toAddr+" takes no followers")
		return
	}
	if full, err := s.full(ctx, to); err != nil || full {
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "request count")
			return
		}
		w.Header().Set("Retry-After", "3600")
		s.refuse(w, r, caller, toAddr, "limit", http.StatusTooManyRequests, "rate_limited", toAddr+" has too many pending follow requests, try again later")
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
// address, and the domain of its server, "" for this one, or answers 400
// or 404 itself.
func (s *Service) handle(w http.ResponseWriter, r *http.Request, to string) (string, string, bool) {
	name, domain, qualified := strings.Cut(strings.TrimSpace(to), "@")
	if qualified {
		d, err := fedrooms.NormalizeDomain(domain)
		switch {
		case s.FedDomain == "":
			writeErr(w, r, http.StatusNotFound, "federation_disabled", "following bots of other servers is not enabled on this server")
			return "", "", false
		case err != nil:
			writeErr(w, r, http.StatusBadRequest, "invalid_domain", err.Error())
			return "", "", false
		}
		if domain = d; d == s.FedDomain {
			domain = ""
		}
	}
	h, err := bots.NormalizeHandle(name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "to: "+err.Error())
		return "", "", false
	}
	return h, domain, true
}

// followRemote has bot caller follow the bot at address on server
// domain, an allowed peer. The follow is stored pending, then the peer
// is asked, which checks its bot, its policy and its own limit, and
// invites our bot into the feed before it answers when it accepts. On a
// refusal the follow is deleted and the peer's status and code passed
// on; without an answer it stays pending, and the next follow asks
// again. An accepted follow is answered as it is, after the peer was
// asked again, best effort, in case it lost the answer to its invite.
func (s *Service) followRemote(w http.ResponseWriter, r *http.Request, caller int64, address, domain string) {
	ctx := r.Context()
	if s.Request == nil {
		writeErr(w, r, http.StatusNotFound, "federation_disabled", "following bots of other servers is not enabled on this server")
		return
	}
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
	pair := func() ([]followView, error) {
		return s.views(ctx, `follower_key_id = ? AND target = ?`, caller, address)
	}
	existing, err := pair()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow lookup")
		return
	}
	// A new follow, one without a clear answer, or an accepted one: it
	// takes a token, and the peer is asked (again), which answers the same
	// follow. An accepted one is asked about because the answer to its
	// invite may have been lost: the peer then keeps the request pending
	// and invites again, which this server takes as the same.
	if lim := s.limiter(false, caller); !lim.Allow() {
		w.Header().Set("Retry-After", retryAfter(lim))
		s.refuse(w, r, caller, address, "limit", http.StatusTooManyRequests, "rate_limited", "too many follow requests, try again later")
		return
	}
	if len(existing) == 1 && existing[0].Status == "accepted" { // best effort: it stays accepted here either way
		if from, err := s.address(ctx, caller); err == nil {
			_ = s.Request(ctx, from, address)
		}
		writeJSON(w, http.StatusCreated, existing[0])
		return
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO follows(follower_key_id, target, status, created_at)
		VALUES (?, ?, 'pending', ?) ON CONFLICT(follower_key_id, target) DO NOTHING`, caller, address, storage.FormatTS(time.Now()))
	var created int64
	if err == nil {
		created, err = res.RowsAffected()
	}
	var from string
	if err == nil {
		from, err = s.address(ctx, caller)
	}
	if err == nil {
		err = s.Request(ctx, from, address)
	}
	var fe *fedrooms.Error
	reason := "" // of an audited refusal
	if errors.As(err, &fe) {
		switch {
		case fe.Status == http.StatusNotFound:
			reason = "not a bot"
		case fe.Status == http.StatusTooManyRequests: // the peer's limits are hourly
			reason = "limit"
			w.Header().Set("Retry-After", "3600")
		case fe.Status == http.StatusForbidden && fe.Code == "follow_refused":
			reason = "policy"
		case fe.Status == http.StatusForbidden && fe.Code == "peer_not_allowed":
			reason = "peer"
		case fe.Status == http.StatusForbidden:
			reason = "refused"
		}
	}
	switch {
	case err == nil:
	case errors.As(err, &fe) && fe.Status == http.StatusBadGateway: // the peer may have taken it: it stays pending
		writeErr(w, r, fe.Status, fe.Code, fe.Message)
		return
	case errors.As(err, &fe):
		if created == 1 { // a refused resend keeps the request the peer still holds
			_, _ = s.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM follows WHERE follower_key_id = ? AND target = ? AND status = 'pending'`,
				caller, address)
		}
		if reason != "" {
			s.refuse(w, r, caller, address, reason, fe.Status, fe.Code, fe.Message)
		} else {
			writeErr(w, r, fe.Status, fe.Code, fe.Message)
		}
		return
	default:
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow request")
		return
	}
	if existing, err = pair(); err != nil || len(existing) == 0 {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", "follow")
		return
	}
	if created == 1 {
		s.audit(r, "follow.request", existing[0])
	}
	writeJSON(w, http.StatusCreated, existing[0])
}

// Remote takes the follow request of follower, a bot of another server
// (name@domain, its domain the signing server's: the federation listener
// checks it), toward our bot handle, as that bot's policy decides, and
// returns the follow's status as the follower sees it. Following again
// answers the existing follow; under policy open a pending one is
// accepted again, and an accepted one invites its server again: that
// server asks only while its follow is pending, so it lacks the feed. A
// refusal is a *fedrooms.Error: 404 for no such bot, 403 follow_refused
// for policy none.
func (s *Service) Remote(ctx context.Context, follower, handle string) (string, error) {
	var (
		to, key int64
		policy  string
	)
	h, err := bots.NormalizeHandle(handle)
	if err == nil {
		err = s.DB.QueryRowContext(ctx, `SELECT k.id, COALESCE(p.follow_policy, 'approve'),
			COALESCE((SELECT id FROM api_keys WHERE label = ? AND remote_domain IS NOT NULL), 0)
			FROM api_keys k LEFT JOIN bot_profiles p ON p.key_id = k.id
			WHERE k.handle = ? AND k.revoked_at IS NULL AND k.remote_domain IS NULL AND k.is_admin = 0`, follower, h).Scan(&to, &policy, &key)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, bots.ErrInvalid):
		s.refused(ctx, 0, follower, handle[:min(len(handle), 300)], "not a bot", "not_found")
		return "", &fedrooms.Error{Status: http.StatusNotFound, Code: "not_found", Message: "no such bot"}
	case err != nil:
		return "", err
	}
	toAddr := h + "@" + s.FedDomain
	pair := func() ([]followView, error) {
		return s.views(ctx, `follower_key_id = ? AND target = ?`, key, toAddr)
	}
	existing, err := pair()
	switch {
	case err != nil:
		return "", err
	case len(existing) == 0 && policy == bots.FollowNone:
		s.refused(ctx, key, follower, toAddr, "policy", "follow_refused")
		return "", &fedrooms.Error{Status: http.StatusForbidden, Code: "follow_refused", Message: toAddr + " takes no followers"}
	case len(existing) == 0:
		var full bool
		var created int64
		if full, err = s.full(ctx, to); err == nil && full {
			err = &fedrooms.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: toAddr + " has too many pending follow requests"}
		} else if err == nil {
			key, created, err = s.store(ctx, follower, toAddr, to)
		}
		if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) && fe.Status == http.StatusTooManyRequests {
			s.refused(ctx, key, follower, toAddr, "limit", fe.Code)
		}
		if err != nil {
			return "", err
		}
		if existing, err = pair(); err != nil || len(existing) == 0 {
			return "", cmp.Or(err, errNotPending)
		}
		if created == 1 {
			s.auditAs(ctx, key, "follow.request", existing[0])
		}
	}
	switch v := existing[0]; {
	case v.Status == "pending" && policy == bots.FollowOpen:
		err = s.accept(ctx, v.ID)
	case v.Status == "accepted":
		var feed string
		if feed, err = s.feed(ctx, to); err == nil {
			_, err = s.invite(ctx, feed, key)
		}
	}
	// An invite without an answer leaves the follow as it is: the
	// follower's next request tries again.
	if fe := (*fedrooms.Error)(nil); err != nil && !errors.As(err, &fe) && !errors.Is(err, errNotPending) {
		return "", err
	}
	if existing, err = pair(); err != nil || len(existing) == 0 {
		return "", cmp.Or(err, errNotPending)
	}
	return hidden(existing[0]).Status, nil
}

// store stores a pending follow of follower, a bot of another server,
// toward to, our bot at toAddr, and returns its stand-in and whether it
// stored one (1) or found one (0). A follower whose server has all the
// stand-ins it may have here is 429.
func (s *Service) store(ctx context.Context, follower, toAddr string, to int64) (int64, int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	key, err := fedrooms.StandIn(ctx, tx, follower)
	switch {
	case errors.Is(err, fedrooms.ErrStandInLimit): // an ErrAddress too
		return 0, 0, &fedrooms.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: err.Error()}
	case errors.Is(err, fedrooms.ErrAddress):
		return 0, 0, &fedrooms.Error{Status: http.StatusConflict, Code: "conflict", Message: err.Error()}
	}
	var res sql.Result
	if err == nil {
		res, err = tx.ExecContext(ctx, `INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
			VALUES (?, ?, ?, 'pending', ?) ON CONFLICT(follower_key_id, target) DO NOTHING`, key, toAddr, to, storage.FormatTS(time.Now()))
	}
	var created int64
	if err == nil {
		created, err = res.RowsAffected()
	}
	if err == nil {
		err = tx.Commit()
	}
	return key, created, err
}

// full reports whether bot, a bot of ours, has MaxPending pending
// requests. ponytail: counts by scanning follows; an index on
// (target_key_id, status) if that shows.
func (s *Service) full(ctx context.Context, bot int64) (bool, error) {
	var full bool
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) >= ? FROM follows WHERE target_key_id = ? AND status = 'pending'`,
		MaxPending, bot).Scan(&full)
	return full, err
}

// accept makes follow id, a pending request to a bot of ours, accepted:
// the follower joins the bot's feed, made if needed, with read. A
// follower of another server is its stand-in: as for an invite by
// address, its server is invited into the feed first, and the stand-in
// joins with what that server reads a push up to; a refused invite (a
// *fedrooms.Error) leaves the follow pending. It is errNotPending when
// the follow is no longer a pending request.
func (s *Service) accept(ctx context.Context, id int64) error {
	var follower, target int64
	var remote bool
	err := s.DB.QueryRowContext(ctx, `SELECT f.follower_key_id, f.target_key_id, k.remote_domain IS NOT NULL
		FROM follows f JOIN api_keys k ON k.id = f.follower_key_id
		WHERE f.id = ? AND f.status = 'pending' AND f.target_key_id IS NOT NULL`, id).Scan(&follower, &target, &remote)
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
	readLimit := 0
	if remote {
		if readLimit, err = s.invite(ctx, feed, follower); err != nil {
			return err
		}
		// The follower's server has the replica now: the stand-in joins
		// even if the request ends meanwhile. ponytail: a follow ended
		// while the invite was out leaves that server a replica its host
		// does not feed; its bot unfollows to drop it.
		ctx = context.WithoutCancel(ctx)
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
			err = fedrooms.LogJoin(ctx, tx, feed, follower, s.FedDomain, s.MaxMessageBytes, readLimit)
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

// invite asks the server of follower, a stand-in, to take its bot into
// feed with read, and returns what that server reads a push up to.
func (s *Service) invite(ctx context.Context, feed string, follower int64) (int, error) {
	if s.InviteRemote == nil {
		return 0, &fedrooms.Error{Status: http.StatusNotFound, Code: "federation_disabled", Message: "federation is not enabled on this server"}
	}
	var name, address string
	if err := s.DB.QueryRowContext(ctx, `SELECT r.name, k.label FROM rooms r, api_keys k WHERE r.id = ? AND k.id = ?`,
		feed, follower).Scan(&name, &address); err != nil {
		return 0, err
	}
	return s.InviteRemote(ctx, feed, name, address, []string{rights.Read})
}

// feed returns the active feed of bot, a bot of ours, made if needed:
// named feed:<address>, or with its id as a suffix when that name is
// taken or too long, with the bot as its owner. It is found by its
// owner, not its address, which changes with the federation domain.
func (s *Service) feed(ctx context.Context, bot int64) (string, error) {
	addr, err := s.address(ctx, bot)
	if err != nil {
		return "", err
	}
	find := func() (string, error) {
		var id string
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM rooms WHERE owner_key_id = ? AND feed IS NOT NULL AND home_domain IS NULL
			AND archived_at IS NULL`, bot).Scan(&id)
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
		name = fedrooms.Suffixed(name, id)
	}
	err = insert(name)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		if other, err := find(); err != nil || other != "" { // made meanwhile
			return other, err
		}
		err = insert(fedrooms.Suffixed(name, id)) // another room has the name
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

// Reconcile moves feeds and follows to the current addresses of their
// bots, which change when federation is turned on or its domain
// changes; the server runs it at startup. Each feed hosted here takes
// its owner's address, and each follow of a bot of ours that bot's. Of
// two follows of one pair that would collide, the one further along
// stays (accepted, then pending, then rejected; the older on a tie).
// Room names are left as they are.
func (s *Service) Reconcile(ctx context.Context) error {
	suffix := "" // a bot's address is handle@domain, the bare handle with federation off
	if s.FedDomain != "" {
		suffix = "@" + s.FedDomain
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rank := func(f string) string {
		return `CASE ` + f + `.status WHEN 'accepted' THEN 2 WHEN 'pending' THEN 1 ELSE 0 END`
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET feed = COALESCE((SELECT handle || ? FROM api_keys WHERE id = rooms.owner_key_id), feed)
		WHERE feed IS NOT NULL AND home_domain IS NULL`, suffix); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE target_key_id IS NOT NULL AND EXISTS (SELECT 1 FROM follows g
		WHERE g.follower_key_id = follows.follower_key_id AND g.target_key_id = follows.target_key_id AND g.id != follows.id
		AND (`+rank("g")+` > `+rank("follows")+` OR `+rank("g")+` = `+rank("follows")+` AND g.id < follows.id))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE follows SET target = COALESCE((SELECT handle || ? FROM api_keys WHERE id = follows.target_key_id), target)
		WHERE target_key_id IS NOT NULL`, suffix); err != nil {
		return err
	}
	return tx.Commit()
}

// list answers a page of the follows of where, which takes the
// request's bot as ?1 (0: every bot, for an admin key), under name: at
// most ?limit= (1 to 500, default 100) after id ?after=, with
// next_after while more follow. With hide, a bot sees its rejected
// follows as pending.
func (s *Service) list(name, where string, hide bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bot, ok := s.subject(w, r)
		if !ok {
			return
		}
		limit, after := 100, int64(0)
		var err error
		if v := r.URL.Query().Get("limit"); v != "" {
			if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 500 {
				writeErr(w, r, http.StatusBadRequest, "bad_request", "limit must be 1 to 500")
				return
			}
		}
		if v := r.URL.Query().Get("after"); v != "" {
			if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
				writeErr(w, r, http.StatusBadRequest, "bad_request", "after must be a follow id")
				return
			}
		}
		out, err := s.page(r.Context(), where, after, limit+1, bot)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "list query")
			return
		}
		if hide && !auth.IsAdmin(r.Context()) {
			for i := range out {
				out[i] = hidden(out[i])
			}
		}
		body := map[string]any{}
		if len(out) > limit {
			out = out[:limit]
			body["next_after"] = out[limit-1].ID
		}
		body[name] = out
		writeJSON(w, http.StatusOK, body)
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
		var fe *fedrooms.Error
		switch {
		case errors.Is(err, sql.ErrNoRows), errors.Is(err, errNotPending):
			writeErr(w, r, http.StatusNotFound, "not_found", "no such pending follow request")
			return
		case errors.As(err, &fe): // the follower's server refused the feed, or did not answer
			writeErr(w, r, fe.Status, fe.Code, fe.Message)
			return
		case err != nil:
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "answer request")
			return
		}
		v, err := s.views(ctx, `f.id = ?`, id)
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
	handle, domain, ok := s.handle(w, r, chi.URLParam(r, "to"))
	if !ok {
		return
	}
	s.end(w, r, caller, strings.TrimSuffix(handle+"@"+cmp.Or(domain, s.FedDomain), "@"))
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

// end ends the follow of follower toward target, whatever its status,
// and takes the follower out of the target's feed; 404 when there is
// none.
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
		if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) { // the host of the feed refused the leave, or did not answer
			writeErr(w, r, fe.Status, fe.Code, fe.Message)
		} else {
			writeErr(w, r, http.StatusInternalServerError, "internal_error", "remove follow")
		}
		return
	}
	// A pending follow of a bot of another server is withdrawn there too,
	// best effort: it is gone here either way.
	if _, domain, _ := strings.Cut(v[0].To, "@"); v[0].Status == "pending" && domain != "" && domain != s.FedDomain && s.Withdraw != nil {
		_ = s.Withdraw(context.WithoutCancel(r.Context()), v[0].From, v[0].To)
	}
	s.audit(r, "follow.remove", v[0])
	w.WriteHeader(http.StatusNoContent)
}

// remove deletes follow id, of follower toward target, and takes the
// follower out of target's active feed it is a member of, whatever the
// follow's status: a feed of ours, whose log tells a follower's server,
// or the replica of a feed of another server, whose host is told first,
// as a member removal does; a replica left without a bot of ours is
// archived. The membership is looked up again in the transaction, which
// holds the write lock: an accept that committed after the first look
// is undone with the follow.
func (s *Service) remove(ctx context.Context, id, follower int64, target string) error {
	var left string
	err := s.DB.QueryRowContext(ctx, `SELECT r.id FROM rooms r JOIN room_members m ON m.room_id = r.id AND m.key_id = ?
		WHERE r.feed = ? AND r.home_domain IS NOT NULL AND r.archived_at IS NULL`, follower, target).Scan(&left)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if left != "" && s.Leave != nil { // the host first: if it cannot be told, nothing changes here
		if err := s.Leave(ctx, left, follower); err != nil {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE id = ?`, id); err != nil {
		return err
	}
	var feed string
	var replica bool
	err = tx.QueryRowContext(ctx, `SELECT r.id, r.home_domain IS NOT NULL FROM rooms r JOIN room_members m ON m.room_id = r.id AND m.key_id = ?
		WHERE r.feed = ? AND r.archived_at IS NULL`, follower, target).Scan(&feed, &replica)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tx.Commit()
	case err != nil:
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
	if replica {
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET archived_at = COALESCE(archived_at, ?) WHERE id = ? AND NOT EXISTS
			(SELECT 1 FROM room_members m JOIN api_keys k ON k.id = m.key_id WHERE m.room_id = ? AND k.remote_domain IS NULL)`,
			storage.FormatTS(time.Now()), feed, feed); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

// RemoteWithdraw deletes the pending or rejected follow request of
// follower, a bot of another server (name@domain, its domain the
// signing server's: the federation listener checks it), toward our bot
// handle, and reports whether there was one. An accepted follow is left
// to the leave of its replica.
func (s *Service) RemoteWithdraw(ctx context.Context, follower, handle string) (bool, error) {
	h, err := bots.NormalizeHandle(handle)
	if err != nil {
		return false, nil
	}
	v, err := s.views(ctx, `k.label = ? AND k.remote_domain IS NOT NULL AND f.target = ? AND f.status != 'accepted'`,
		follower, h+"@"+s.FedDomain)
	if err != nil || len(v) == 0 {
		return false, err
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE id = ? AND status != 'accepted'`, v[0].ID)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	if err != nil || n == 0 {
		return false, err
	}
	var key int64
	_ = s.DB.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE label = ? AND remote_domain IS NOT NULL`, follower).Scan(&key)
	s.auditAs(ctx, key, "follow.remove", v[0])
	return true, nil
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

// views returns the follows matching where with args, oldest first;
// where names the follow f and its follower's key k.
func (s *Service) views(ctx context.Context, where string, args ...any) ([]followView, error) {
	return s.page(ctx, where, 0, -1, args...)
}

// page returns at most limit (-1: all) follows matching where with args
// after id after, oldest first. The follower's address comes from the
// join: a stand-in's label, a bot's handle@domain.
func (s *Service) page(ctx context.Context, where string, after int64, limit int, args ...any) ([]followView, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT f.id, f.target, f.status, f.created_at, COALESCE(f.decided_at, ''), k.id,
		k.remote_domain IS NOT NULL, CASE WHEN k.remote_domain IS NOT NULL THEN k.label ELSE COALESCE(k.handle, '') END
		FROM follows f JOIN api_keys k ON k.id = f.follower_key_id
		WHERE (`+where+`) AND f.id > ? ORDER BY f.id LIMIT ?`, append(args, after, limit)...)
	if err != nil {
		return nil, err
	}
	out, keys := []followView{}, []int64{}
	for rows.Next() {
		var v followView
		var k int64
		var remote bool
		if err := rows.Scan(&v.ID, &v.To, &v.Status, &v.CreatedAt, &v.DecidedAt, &k, &remote, &v.From); err != nil {
			rows.Close()
			return nil, err
		}
		if !remote && v.From != "" { // a bot of ours: handle@domain
			v.From = strings.TrimSuffix(v.From+"@"+s.FedDomain, "@")
		}
		out, keys = append(out, v), append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i, k := range keys {
		if out[i].From == "" { // a key without a handle: no bot follows as one, but its address is known
			if out[i].From, err = s.address(ctx, k); err != nil {
				return nil, err
			}
		}
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

// timeline answers the posts of the active feeds the request's bot is a
// member of, its own and those of the bots it follows, in (ts, id) order
// after since, as GET /v1/rooms/{id}/messages pages. An admin key without
// ?bot= reads every active feed hosted here. ?order=desc answers the
// newest posts after since first, as one page: next_since stays empty.
func (s *Service) timeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bot, ok := s.subject(w, r)
	if !ok {
		return
	}
	order := "m.ts, m.id"
	switch r.URL.Query().Get("order") {
	case "", "asc":
	case "desc":
		order = "m.ts DESC, m.id DESC"
	default:
		writeErr(w, r, http.StatusBadRequest, "bad_request", "order must be asc or desc")
		return
	}
	var err error
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
		WHERE m.deleted_at IS NULL AND m.ts > ?2
		AND (?1 = 0 AND r.home_domain IS NULL OR EXISTS (SELECT 1 FROM room_members me WHERE me.room_id = r.id AND me.key_id = ?1))
		ORDER BY `+order+` LIMIT ?3`, bot, since, limit)
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
	if len(out) > 0 && order == "m.ts, m.id" {
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

// audit writes action for follow v, after the change, as the caller.
func (s *Service) audit(r *http.Request, action string, v followView) {
	c, _ := auth.FromContext(r.Context())
	s.auditAs(r.Context(), c.KeyID, action, v)
}

// auditAs writes action for follow v as actor.
func (s *Service) auditAs(ctx context.Context, actor int64, action string, v followView) {
	detail, _ := json.Marshal(map[string]string{"from": v.From, "to": v.To, "status": v.Status})
	_ = audit.Write(ctx, s.DB, actor, action, "follow", strconv.FormatInt(v.ID, 10), "", "ok", string(detail))
}

// refuse audits a refused follow (reason: not a bot, policy, limit) and
// answers it.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, caller int64, to, reason string, status int, code, msg string) {
	s.refused(r.Context(), caller, "", to, reason, code)
	writeErr(w, r, status, code, msg)
}

// refused audits a refused follow of actor toward to; from is the
// address of a follower of another server, "" for a bot of ours.
func (s *Service) refused(ctx context.Context, actor int64, from, to, reason, code string) {
	d := map[string]string{"to": to, "reason": reason}
	if from != "" {
		d["from"] = from
	}
	detail, _ := json.Marshal(d)
	_ = audit.Write(ctx, s.DB, actor, "follow.refused", "bot", to, "", "error:"+code, string(detail))
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
