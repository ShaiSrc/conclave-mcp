package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hermes/conclave-mcp/internal/bots"
	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// Follows across servers (docs/superpowers/specs/2026-11-14-follow-timelines-design.md):
// a bot of ours asks to follow a bot of a peer with a signed follow
// request; the peer's server decides by its bot's policy and, when it
// accepts, invites our bot into the bot's feed with a feed invite
// (handleRoomInvite), which makes the follow accepted here.

// FollowRequestsPerHour is how many follow requests a server takes from
// one peer an hour.
const FollowRequestsPerHour = 100

// kindFeed is the kind the federation invite and the API give a feed,
// a room whose rooms.feed is set; its kind column stays room.
const kindFeed = "feed"

// FollowRemote sends the follow request of follower, our bot's address,
// to the server of address (name@domain), an allowed peer, and returns
// nil when that server stored or accepted it: one that accepts invites
// our bot into the feed before it answers. A refusal is a
// *fedrooms.Error: the peer's 4xx status and code (its text does not
// reach the follower), or 502 host_unreachable without an answer.
func (s *Service) FollowRemote(ctx context.Context, follower, address string) error {
	handle, domain, _ := strings.Cut(address, "@")
	status, out, err := s.sendSigned(ctx, http.MethodPost, domain, "/fed/v1/follows",
		rawJSON(map[string]string{"follower": follower, "handle": handle}), maxBody)
	switch err := s.unreachable(domain, status, err); {
	case err != nil:
		return err
	case status == http.StatusCreated:
		return nil
	case status == http.StatusNotFound:
		return &fedrooms.Error{Status: status, Code: "not_found", Message: "no bot " + handle + " on " + domain}
	case status/100 == 4:
		return &fedrooms.Error{Status: status, Code: errCode(out, "follow_refused"), Message: domain + " refused the follow"}
	}
	return &fedrooms.Error{Status: http.StatusBadGateway, Code: "host_unreachable", Message: fmt.Sprintf("%s answered with status %d", domain, status)}
}

// WithdrawFollow tells the server of address (name@domain), an allowed
// peer, that follower, our bot's address, withdraws its pending follow
// request. A refusal is a *fedrooms.Error; the caller goes on without
// it.
func (s *Service) WithdrawFollow(ctx context.Context, follower, address string) error {
	handle, domain, _ := strings.Cut(address, "@")
	status, out, err := s.sendSigned(ctx, http.MethodPost, domain, "/fed/v1/follows/withdraw",
		rawJSON(map[string]string{"follower": follower, "handle": handle}), maxBody)
	switch err := s.unreachable(domain, status, err); {
	case err != nil:
		return err
	case status == http.StatusNoContent:
		return nil
	}
	return hostError(domain, status, out)
}

// followRequest reads the {follower, handle} body of a follow request
// or withdrawal from an allowed peer, checks that follower is a bot
// address on the origin's domain, and takes one of the origin's
// FollowRequestsPerHour (429). It answers the request itself otherwise.
func (s *Service) followRequest(w http.ResponseWriter, r *http.Request) (follower, handle string, ok bool) {
	ctx, origin := r.Context(), OriginFrom(r.Context())
	if !s.allowedOrigin(w, r) {
		return "", "", false
	}
	var in struct {
		Follower string `json:"follower"`
		Handle   string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON")
		return "", "", false
	}
	name, domain, _ := strings.Cut(in.Follower, "@")
	if h, err := bots.NormalizeHandle(name); err != nil || h != name || domain != origin {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "follower must be the address of a bot of the signing server")
		return "", "", false
	}
	now := s.now()
	if lim := s.hourly(&s.followLimits, origin, FollowRequestsPerHour); !lim.AllowN(now, 1) {
		w.Header().Set("Retry-After", retryAfterAt(lim, now))
		detail, _ := json.Marshal(map[string]string{"from": in.Follower, "to": in.Handle[:min(len(in.Handle), 300)], "reason": "limit"})
		s.auditRoom(ctx, "follow.refused", origin, "", "error:rate_limited", string(detail))
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many follow requests, try again later")
		return "", "", false
	}
	return in.Follower, in.Handle, true
}

// handleFollow answers POST /fed/v1/follows {follower, handle} from an
// allowed peer: its bot follower (an address on the origin's domain)
// asks to follow our bot handle, whose policy decides (RoomHooks.Follow:
// 404 for no such bot, 403 follow_refused, 429 when the bot has too many
// pending requests or the origin too many stand-ins here). It takes at
// most FollowRequestsPerHour from each peer (429) and answers 201
// {"status": "pending"|"accepted"}.
func (s *Service) handleFollow(w http.ResponseWriter, r *http.Request) {
	follower, handle, ok := s.followRequest(w, r)
	if !ok {
		return
	}
	status, err := s.Rooms.Follow(r.Context(), follower, handle)
	if fe := (*fedrooms.Error)(nil); errors.As(err, &fe) {
		if fe.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "3600")
		}
		writeErr(w, r, fe.Status, fe.Code, fe.Message)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": status})
}

// handleFollowWithdraw answers POST /fed/v1/follows/withdraw {follower,
// handle} from an allowed peer, within the follow requests it may make
// an hour: its bot follower withdraws its pending (or rejected) request
// toward our bot handle (RoomHooks.Withdraw), 204, or 404 when it has
// none. An accepted follow ends through the replica's leave.
func (s *Service) handleFollowWithdraw(w http.ResponseWriter, r *http.Request) {
	follower, handle, ok := s.followRequest(w, r)
	if !ok {
		return
	}
	switch done, err := s.Rooms.Withdraw(r.Context(), follower, handle); {
	case err != nil:
		s.fail(w, r, err)
	case !done:
		writeErr(w, r, http.StatusNotFound, "not_found", "no such follow request")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
