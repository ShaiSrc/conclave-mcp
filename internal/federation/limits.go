package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"golang.org/x/time/rate"
)

// Per-peer limits: requests a minute (Verify), direct invites
// (DMInvitesPerHour) and follow requests (FollowRequestsPerHour) an
// hour. Each is its default or the operator's override in
// federation_peer_limits: NULL is the default, 0 refuses.
const (
	peerPerMinute = 120   // requests a minute of an allowed peer
	seenPerMinute = 30    // of a seen peer (open mode) or a first contact
	peerBurst     = 30    // the most a requests limiter's burst gets
	maxLimit      = 10000 // the highest override
)

// limitCols are the columns of federation_peer_limits, which name the
// limits in the API too, in the order of overrides and limits.
var limitCols = [3]string{"requests_per_minute", "dm_invites_per_hour", "follow_requests_per_hour"}

// overrides are a peer's overrides in limitCols order; nil is the
// default.
type overrides [3]*int

// limit is one effective limit, and whether the operator set it.
type limit struct {
	Value    int  `json:"value"`
	Override bool `json:"override"`
}

// limits are a peer's effective limits.
type limits struct {
	RequestsPerMinute     limit `json:"requests_per_minute"`
	DMInvitesPerHour      limit `json:"dm_invites_per_hour"`
	FollowRequestsPerHour limit `json:"follow_requests_per_hour"`
}

// effective returns the limits of a peer with status and overrides o.
func (o overrides) effective(status string) limits {
	perMinute := seenPerMinute
	if status == statusAllowed {
		perMinute = peerPerMinute
	}
	l := func(i, def int) limit {
		if o[i] != nil {
			return limit{*o[i], true}
		}
		return limit{def, false}
	}
	return limits{l(0, perMinute), l(1, DMInvitesPerHour), l(2, FollowRequestsPerHour)}
}

// overridesOf returns the overrides of domain, or of every peer for "",
// by domain.
func (s *Service) overridesOf(ctx context.Context, domain string) (map[string]overrides, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, requests_per_minute, dm_invites_per_hour, follow_requests_per_hour
		FROM federation_peer_limits WHERE ? IN ('', domain)`, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]overrides{}
	for rows.Next() {
		var d string
		var o overrides
		if err := rows.Scan(&d, &o[0], &o[1], &o[2]); err != nil {
			return nil, err
		}
		out[d] = o
	}
	return out, rows.Err()
}

// peerLimiters are a peer's limiters, built from its effective limits.
type peerLimiters struct{ requests, dmInvites, followRequests *rate.Limiter }

// newLimiters builds limiters of l. The requests limiter's burst is a
// third of its value a minute, rounded up, at most peerBurst: 30 for
// the default 120, 10 for a seen peer's 30. An hourly one's burst is
// its value. A 0 refuses everything.
func newLimiters(l limits) *peerLimiters {
	n := l.RequestsPerMinute.Value
	hourly := func(n int) *rate.Limiter { return rate.NewLimiter(rate.Limit(n)/3600, n) }
	return &peerLimiters{rate.NewLimiter(rate.Limit(n)/60, min((n+2)/3, peerBurst)),
		hourly(l.DMInvitesPerHour.Value), hourly(l.FollowRequestsPerHour.Value)}
}

// limitersOf returns origin's limiters, built on first use from its
// status and overrides; they stay until dropLimiters, so the next use
// builds them again (peers are bounded by the table). ponytail: a miss
// reads the database under s.mu, so limiters built from values older
// than a change cannot outlive its dropLimiters; split the lock if
// misses get frequent.
func (s *Service) limitersOf(ctx context.Context, origin string) (*peerLimiters, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.limiters[origin]; l != nil {
		return l, nil
	}
	p, err := s.peers.Get(ctx, origin)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		return nil, err
	}
	o, err := s.overridesOf(ctx, origin)
	if err != nil {
		return nil, err
	}
	l := newLimiters(o[origin].effective(p.Status))
	s.limiters[origin] = l
	return l, nil
}

// dropLimiters forgets domain's limiters, after a change of its status
// or overrides, or its removal.
func (s *Service) dropLimiters(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.limiters, domain)
}

// handlePatchPeer sets a peer's overrides: PATCH {"limits": {...}} with
// any of limitCols, each null (the default) or 0 to maxLimit; a field
// left out stays. It answers the peer's view.
func (s *Service) handlePatchPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain, ok := s.peerDomain(w, r, pathParam(r, "domain"))
	if !ok {
		return
	}
	var in struct {
		Limits map[string]json.RawMessage `json:"limits"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil || in.Limits == nil {
		writeErr(w, r, http.StatusBadRequest, "invalid_json", `body must be {"limits": {...}}`)
		return
	}
	set := map[int]*int{}
	for k, raw := range in.Limits {
		i := slices.Index(limitCols[:], k)
		var v *int
		if i < 0 || json.Unmarshal(raw, &v) != nil || v != nil && (*v < 0 || *v > maxLimit) {
			writeErr(w, r, http.StatusBadRequest, "invalid_limit",
				fmt.Sprintf("%q is not one of %v, or not null or an integer from 0 to %d", k, limitCols, maxLimit))
			return
		}
		set[i] = v
	}
	if _, err := s.peers.Get(ctx, domain); errors.Is(err, ErrPeerNotFound) {
		writeErr(w, r, http.StatusNotFound, "peer_not_found", "no such peer")
		return
	} else if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	o, err := s.setOverrides(ctx, domain, set)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	s.dropLimiters(domain)
	detail, _ := json.Marshal(struct {
		R *int `json:"requests_per_minute"`
		D *int `json:"dm_invites_per_hour"`
		F *int `json:"follow_requests_per_hour"`
	}{o[0], o[1], o[2]})
	s.audit(ctx, "federation.peer.limits", domain, "ok", string(detail))
	s.writePeer(w, r, domain, http.StatusOK)
}

// setOverrides applies set (by limitCols index) to domain's overrides
// and returns them; a row left with defaults only is deleted.
func (s *Service) setOverrides(ctx context.Context, domain string, set map[int]*int) (overrides, error) {
	var o overrides
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return o, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT requests_per_minute, dm_invites_per_hour, follow_requests_per_hour
		FROM federation_peer_limits WHERE domain = ?`, domain).Scan(&o[0], &o[1], &o[2]); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return o, err
	}
	args := []any{domain}
	for i := range o {
		if v, ok := set[i]; ok {
			o[i] = v
		}
		if o[i] != nil {
			args = append(args, *o[i])
		} else {
			args = append(args, nil)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_peer_limits(domain, requests_per_minute, dm_invites_per_hour, follow_requests_per_hour)
		VALUES (?, ?, ?, ?) ON CONFLICT(domain) DO UPDATE SET requests_per_minute = excluded.requests_per_minute,
		dm_invites_per_hour = excluded.dm_invites_per_hour, follow_requests_per_hour = excluded.follow_requests_per_hour`, args...); err != nil {
		return o, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_peer_limits WHERE domain = ? AND requests_per_minute IS NULL
		AND dm_invites_per_hour IS NULL AND follow_requests_per_hour IS NULL`, domain); err != nil {
		return o, err
	}
	return o, tx.Commit()
}
