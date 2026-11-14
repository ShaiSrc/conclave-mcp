package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// fedPeer is a peer as GET /v1/federation/peers/{domain} answers it.
type fedPeer struct {
	Domain      string
	Status      string
	Fingerprint string
	KeyID       string  `json:"key_id"`
	LastSeenAt  *string `json:"last_seen_at"`
	LastOutAt   *string `json:"last_out_at"`
	Health      string
	Delivery    struct {
		Rooms        int
		Paused       int
		FailingSince *string `json:"failing_since"`
		LastError    string  `json:"last_error"`
	}
	Limits map[string]struct {
		Value    int
		Override bool
	}
}

// peerView sends method to the peer route of domain plus suffix on f
// and returns the view it answers with 200.
func (f *fedEnv) peerView(method, domain, suffix string) fedPeer {
	f.t.Helper()
	r := f.admin(method, "/v1/federation/peers/"+domain+suffix, nil)
	var v fedPeer
	if r.Status != 200 || json.Unmarshal(r.Body, &v) != nil || v.Domain != domain {
		f.t.Fatalf("%s peer %s%s on %s: %d %s", method, domain, suffix, f.domain, r.Status, r.Body)
	}
	return v
}

// memberIDs returns the key IDs of room's members on f, as its admin
// key lists them.
func (f *fedEnv) memberIDs(room string) []int64 {
	f.t.Helper()
	r := f.admin("GET", "/v1/rooms/"+room+"/members", nil)
	var out struct {
		Members []struct {
			KeyID int64 `json:"key_id"`
		}
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		f.t.Fatalf("members on %s: %d %s", f.domain, r.Status, r.Body)
	}
	ids := []int64{}
	for _, m := range out.Members {
		ids = append(ids, m.KeyID)
	}
	return ids
}

// A and B share a room hosted on A, a room hosted on B and a follow.
// A rotates its key and B follows the rotation with no operator step;
// A's view of B shows health, and a retry answers. A blocks B's bot b1:
// it leaves A's rooms, and its DM and follow are refused. A blocks B and
// purges it: A keeps nothing of B but revoked stand-ins, soft-deleted
// messages, the operator's blocks and audit rows.
func TestModeration(t *testing.T) {
	a, b := newFedEnv(t), newFedEnv(t)
	a.peer(b)
	b.peer(a)
	a1, b1, b2 := a.botKey("a1"), b.botKey("b1"), b.botKey("b2")
	a1Addr, b1Addr, b2Addr := "a1@"+a.domain, "b1@"+b.domain, "b2@"+b.domain
	for _, p := range []struct {
		f     *fedEnv
		bot   string
		patch map[string]string
	}{
		{a, "a1", map[string]string{"follow_policy": "open", "dm_policy": "peers"}},
		{b, "b1", map[string]string{"dm_policy": "peers"}},
	} {
		if r := p.f.admin("PATCH", "/v1/bots/"+p.bot+"/profile", p.patch); r.Status != 200 {
			t.Fatalf("profile of %s: %d %s", p.bot, r.Status, r.Body)
		}
	}
	newRoom := func(f *fedEnv, name string) string {
		t.Helper()
		r := f.admin("POST", "/v1/rooms", map[string]string{"name": name})
		var room struct{ ID string }
		if r.Status != 201 || json.Unmarshal(r.Body, &room) != nil {
			t.Fatalf("create %s on %s: %d %s", name, f.domain, r.Status, r.Body)
		}
		return room.ID
	}
	member := func(f *fedEnv, room string, body map[string]any) {
		t.Helper()
		if r := f.admin("POST", "/v1/rooms/"+room+"/members", body); r.Status != 201 {
			t.Fatalf("member %v on %s: %d %s", body, f.domain, r.Status, r.Body)
		}
	}
	say := func(f *fedEnv, k apiKey, room, content string) {
		t.Helper()
		r := f.callAs("POST", "/v1/rooms/"+room+"/messages", k, map[string]string{"content": content, "message_type": "answer"})
		if r.Status != 201 {
			t.Fatalf("post %q on %s: %d %s", content, f.domain, r.Status, r.Body)
		}
	}
	arrived := func(f *fedEnv, k apiKey, room, content string) {
		t.Helper()
		waitFor(t, content+" on "+f.domain, func() bool { _, ok := f.messages(k, room)[content]; return ok })
	}

	// 1. A hosts proj with a1, b1 and b2; both of B's bots write to it,
	// and b2 claims a path. B hosts theirs with b2 and a1. b1 follows a1,
	// whose policy is open.
	proj := newRoom(a, "proj")
	member(a, proj, map[string]any{"key_id": a1.ID, "rights": []string{"all"}})
	member(a, proj, map[string]any{"address": b1Addr, "rights": []string{"all"}})
	member(a, proj, map[string]any{"address": b2Addr, "rights": []string{"all"}})
	say(b, b1, proj, "b1 in proj")
	say(b, b2, proj, "b2 in proj")
	arrived(a, a1, proj, "b1 in proj")
	arrived(a, a1, proj, "b2 in proj")
	if r := b.callAs("POST", "/v1/rooms/"+proj+"/claims", b2, map[string]any{"paths": []string{"src/"}}); r.Status != 201 {
		t.Fatalf("b2 claims: %d %s", r.Status, r.Body)
	}
	theirs := newRoom(b, "theirs")
	member(b, theirs, map[string]any{"key_id": b2.ID, "rights": []string{"all"}, "allow_multi_room": true})
	member(b, theirs, map[string]any{"address": a1Addr, "rights": []string{"read"}})
	say(b, b2, theirs, "b2 in theirs")
	arrived(a, a1, theirs, "b2 in theirs")
	if r := b.callAs("POST", "/v1/follows", b1, map[string]string{"to": a1Addr}); r.Status != 201 {
		t.Fatalf("b1 follows a1: %d %s", r.Status, r.Body)
	}
	waitFor(t, "b1's follow accepted", func() bool {
		fs := b.follows(b1, "/v1/follows", "follows")
		return len(fs) == 1 && fs[0].To == a1Addr && fs[0].Status == "accepted"
	})
	a.post(a1, "before the rotation", "followers")
	waitFor(t, "the first post on B", func() bool { _, ok := b.timeline(b1)["before the rotation"]; return ok })

	// 2. A rotates its key. B's next request from A re-pins it, with no
	// operator step on B: a post, a room message and a new DM all reach B.
	oldFP := a.fingerprint()
	r := a.admin("POST", "/v1/federation/keys/rotate", nil)
	var rot struct {
		KeyID       string `json:"key_id"`
		Fingerprint string
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &rot) != nil || rot.Fingerprint == oldFP || rot.Fingerprint != a.fingerprint() {
		t.Fatalf("rotate: %d", r.Status)
	}
	r = a.admin("GET", "/v1/federation", nil)
	var st struct {
		Keys []struct {
			KeyID       string `json:"key_id"`
			Fingerprint string
			Status      string
			Until       string
			EndorsedBy  *struct {
				KeyID string `json:"key_id"`
			} `json:"endorsed_by"`
		}
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &st) != nil || len(st.Keys) != 2 ||
		st.Keys[0].Fingerprint != rot.Fingerprint || st.Keys[0].Status != "current" || st.Keys[0].EndorsedBy == nil ||
		st.Keys[0].EndorsedBy.KeyID != st.Keys[1].KeyID || st.Keys[1].Fingerprint != oldFP ||
		st.Keys[1].Status != "previous" || st.Keys[1].Until == "" {
		t.Fatalf("A's keys after the rotation: %d %+v", r.Status, st.Keys)
	}
	if r := a.admin("POST", "/v1/federation/keys/rotate", nil); r.Status != 409 || !strings.Contains(string(r.Body), "rotation_too_soon") {
		t.Fatalf("second rotation: %d", r.Status)
	}
	a.post(a1, "after the rotation", "followers")
	var p tlPost
	waitFor(t, "the post after the rotation on B", func() bool { var ok bool; p, ok = b.timeline(b1)["after the rotation"]; return ok })
	if p.AuthorAddress != a1Addr || p.Unverified {
		t.Fatalf("post on B: %+v", p)
	}
	if v := b.peerView("GET", a.domain, ""); v.Fingerprint != rot.Fingerprint || v.KeyID != rot.KeyID || v.Status != "allowed" {
		t.Fatalf("B's view of A: %+v, want %s", v, rot.Fingerprint)
	}
	say(a, a1, proj, "proj after the rotation")
	arrived(b, b1, proj, "proj after the rotation")
	if r := a.dmSend(a1, b1Addr, "dm after the rotation"); r.Status != 201 {
		t.Fatalf("DM after the rotation: %d %s", r.Status, r.Body)
	}
	waitFor(t, "the DM on B", func() bool { c := b.dmList(b1); return len(c) == 1 && c[0].Address == a1Addr && c[0].Unread == 1 })
	// ponytail: filtered by the prefix federation.key and counted here, as
	// ?action= does not match an action with an underscore: its LIKE escapes
	// _ without an ESCAPE clause.
	keyAudit := string(b.admin("GET", "/v1/audit?action=federation.key&limit=1000", nil).Body)
	if n, m := strings.Count(keyAudit, `"federation.key_rotated"`), strings.Count(keyAudit, `"federation.key_changed"`); n != 1 || m != 0 {
		t.Fatalf("B audited %d key_rotated, %d key_changed; want 1, 0", n, m)
	}

	// 3. A's view of B, a reachable peer: ok, with A's last request out and
	// B's last one in, three cursors (proj, a1's feed, the DM) and the
	// default limits. A retry answers the view. With B down, a push fails
	// and B shows failing; back up and retried, B is ok again.
	v := a.peerView("GET", b.domain, "")
	if v.Status != "allowed" || v.Health != "ok" || v.LastOutAt == nil || v.LastSeenAt == nil || v.Delivery.Rooms != 3 ||
		v.Delivery.Paused != 0 || v.Limits["requests_per_minute"].Value != 120 || v.Limits["requests_per_minute"].Override {
		t.Fatalf("A's view of B: %+v", v)
	}
	if v := a.peerView("POST", b.domain, "/retry"); v.Health != "ok" {
		t.Fatalf("retry: %+v", v)
	}
	b.close()
	say(a, a1, proj, "while B was down")
	waitFor(t, "B failing on A", func() bool { v = a.peerView("GET", b.domain, ""); return v.Health == "failing" })
	if v.Delivery.FailingSince == nil || v.Delivery.LastError == "" || v.Delivery.Paused != 0 {
		t.Fatalf("A's view of B down: %+v", v)
	}
	b.start()
	a.peerView("POST", b.domain, "/retry")
	waitFor(t, "B ok again on A", func() bool { return a.peerView("GET", b.domain, "").Health == "ok" })
	arrived(b, b1, proj, "while B was down")
	if n := a.auditCount("federation.delivery.retry"); n != 2 {
		t.Fatalf("A audited %d retries", n)
	}

	// 4. A blocks b1. Its stand-in leaves proj, the DM and a1's feed on A;
	// the member.remove events reach B, so b1 leaves B's replicas and its
	// follow ends there. A new DM and a new follow of b1 are refused by A.
	r = a.admin("POST", "/v1/federation/blocked-bots", map[string]string{"address": b1Addr, "note": "e2e"})
	if r.Status != 201 || !strings.Contains(string(r.Body), `"address":"`+b1Addr+`"`) {
		t.Fatalf("block b1: %d %s", r.Status, r.Body)
	}
	if r := a.admin("GET", "/v1/rooms/"+proj+"/members", nil); r.Status != 200 || strings.Contains(string(r.Body), b1Addr) ||
		!strings.Contains(string(r.Body), b2Addr) {
		t.Fatalf("proj on A after the block: %d %s", r.Status, r.Body)
	}
	waitFor(t, "b1 out of B's replica of proj", func() bool { return !slices.Contains(b.memberIDs(proj), b1.ID) })
	waitFor(t, "b1's DM gone on B", func() bool { return len(b.dmList(b1)) == 0 })
	waitFor(t, "b1's follow ended on B", func() bool { return len(b.follows(b1, "/v1/follows", "follows")) == 0 })
	if !slices.Contains(b.memberIDs(proj), b2.ID) {
		t.Fatal("b2 left B's replica of proj")
	}
	if r := b.dmSend(b1, a1Addr, "let me in"); r.Status != 403 || !strings.Contains(string(r.Body), "bot_blocked") {
		t.Fatalf("b1's DM: %d %s", r.Status, r.Body)
	}
	if r := b.callAs("POST", "/v1/follows", b1, map[string]string{"to": a1Addr}); r.Status != 403 || !strings.Contains(string(r.Body), "bot_blocked") {
		t.Fatalf("b1's follow: %d %s", r.Status, r.Body)
	}
	if n := a.auditCount("federation.bot.block"); n != 1 {
		t.Fatalf("A audited %d bot blocks", n)
	}

	// 5. A purges B: refused while B is allowed. Blocked, B has no retry;
	// the purge counts what the block left: the replica of theirs and
	// b2's membership there, b1's and b2's messages in proj, b2's claim,
	// proj's cursor (those of the DM and a1's feed went once b1's removal
	// was delivered), and the stand-ins of b2 and of operator@B, the owner
	// of the replica (b1's is revoked already). The block ended every
	// follow.
	if r := a.admin("POST", "/v1/federation/peers/"+b.domain+"/purge", nil); r.Status != 409 || !strings.Contains(string(r.Body), "peer_not_blocked") {
		t.Fatalf("purge of an allowed peer: %d %s", r.Status, r.Body)
	}
	if v := a.peerView("POST", b.domain, "/block"); v.Status != "blocked" {
		t.Fatalf("block B: %+v", v)
	}
	if r := a.admin("POST", "/v1/federation/peers/"+b.domain+"/retry", nil); r.Status != 409 || !strings.Contains(string(r.Body), "peer_not_allowed") {
		t.Fatalf("retry of a blocked peer: %d %s", r.Status, r.Body)
	}
	type counts struct {
		Rooms, Messages, Members, Follows, Claims, Cursors int
		StandIns                                           int `json:"stand_ins"`
	}
	purge := func() counts {
		t.Helper()
		r := a.admin("POST", "/v1/federation/peers/"+b.domain+"/purge", nil)
		var n counts
		if r.Status != 200 || json.Unmarshal(r.Body, &n) != nil {
			t.Fatalf("purge: %d %s", r.Status, r.Body)
		}
		return n
	}
	if n, want := purge(), (counts{Rooms: 1, Messages: 2, Members: 1, Claims: 1, Cursors: 1, StandIns: 2}); n != want {
		t.Fatalf("purge: %+v, want %+v", n, want)
	}
	if n := a.auditCount("federation.peer.purge"); n != 1 {
		t.Fatalf("A audited %d purges", n)
	}

	// What A keeps of B: its blocked row, its revoked stand-ins, its bots'
	// soft-deleted messages, the operator's block of b1, audit rows.
	if v := a.peerView("GET", b.domain, ""); v.Status != "blocked" || v.Delivery.Rooms != 0 {
		t.Fatalf("A's view of B after the purge: %+v", v)
	}
	if r := a.admin("GET", "/v1/rooms?include_archived=1", nil); r.Status != 200 || strings.Contains(string(r.Body), `"home":"`+b.domain) {
		t.Fatalf("A's rooms after the purge: %d %s", r.Status, r.Body)
	}
	if got := a.messages(a1, proj); len(got) != 2 || got["proj after the rotation"].ID == 0 || got["while B was down"].ID == 0 {
		t.Fatalf("proj on A after the purge: %v", got)
	}
	const standIns = `(SELECT id FROM api_keys WHERE remote_domain = ?1)`
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM federation_peers WHERE domain = ?1 AND status = 'blocked'`:                                        1,
		`SELECT COUNT(*) FROM api_keys WHERE remote_domain = ?1 AND revoked_at IS NOT NULL`:                                     3,
		`SELECT COUNT(*) FROM api_keys WHERE remote_domain = ?1 AND revoked_at IS NULL`:                                         0,
		`SELECT COUNT(*) FROM messages WHERE deleted_at IS NOT NULL AND author_key_id IN ` + standIns:                           2,
		`SELECT COUNT(*) FROM messages WHERE deleted_at IS NULL AND author_key_id IN ` + standIns:                               0,
		`SELECT COUNT(*) FROM blocked_bots WHERE substr(address, instr(address, '@') + 1) = ?1`:                                 1,
		`SELECT COUNT(*) FROM rooms WHERE home_domain = ?1 OR owner_key_id IN ` + standIns:                                      0,
		`SELECT COUNT(*) FROM room_members WHERE key_id IN ` + standIns:                                                         0,
		`SELECT COUNT(*) FROM claims WHERE key_id IN ` + standIns:                                                               0,
		`SELECT COUNT(*) FROM follows WHERE follower_key_id IN ` + standIns + ` OR substr(target, instr(target, '@') + 1) = ?1`: 0,
		`SELECT COUNT(*) FROM fed_delivery WHERE peer = ?1`:                                                                     0,
		`SELECT COUNT(*) FROM federation_peer_keys WHERE domain = ?1`:                                                           0,
		`SELECT COUNT(*) FROM federation_peer_limits WHERE domain = ?1`:                                                         0,
		`SELECT COUNT(*) FROM remote_bots WHERE domain = ?1`:                                                                    0,
		`SELECT COUNT(*) FROM fed_events WHERE payload LIKE '%b1 in proj%' OR payload LIKE '%b2 in proj%'`:                      0,
	} {
		var n int
		if err := a.db.QueryRow(q, b.domain).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		} else if n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	if n := purge(); n != (counts{}) {
		t.Errorf("second purge: %+v", n)
	}
}
