package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// Purging a blocked peer and the stand-in cap per host
// (docs/superpowers/specs/2026-11-14-moderation-operations-design.md).

// purge purges domain on g through the admin API.
func (g *roomRig) purge(domain string) *httptest.ResponseRecorder {
	return adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+domain+"/purge", "")
}

// blockPeer blocks domain on g through the admin API.
func (g *roomRig) blockPeer(t *testing.T, domain string) {
	t.Helper()
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+domain+"/block", ""), http.StatusOK)
}

func TestPurgePeer_RefusedUnlessBlocked(t *testing.T) {
	g, m, _, _ := fedRoom(t, "read")
	if _, err := g.svc.peers.RecordSeen(context.Background(), "s.example", testIdentity(t, "s.example").Public); err != nil {
		t.Fatal(err)
	}
	before := g.state(t)
	wantErr(t, g.purge("x.example"), http.StatusNotFound, "peer_not_found")
	wantErr(t, g.purge(m.id.Domain), http.StatusConflict, "peer_not_blocked")
	wantErr(t, g.purge("s.example"), http.StatusConflict, "peer_not_blocked")
	wantErr(t, adminDo(g.svc, []string{"read"}, http.MethodPost, "/v1/federation/peers/"+m.id.Domain+"/purge", ""),
		http.StatusForbidden, "forbidden")
	if got := g.state(t); got != before {
		t.Errorf("refused purges changed %s to %s", before, got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NOT NULL`) +
		count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.peer.purge'`); n != 0 {
		t.Errorf("refused purges left %d rows", n)
	}
}

// After a purge nothing of the peer is left here but its revoked
// stand-ins, soft-deleted messages and audit rows; the peer stays
// blocked. A third host cannot bring its bots back, and a second purge
// finds nothing.
func TestPurgePeer_NothingRemains(t *testing.T) {
	ctx := context.Background()
	g, m, room, _ := fedRoom(t, "read,answer,claim")
	d := m.id.Domain
	m.setReply(applyAll)
	// d's bot c1 posts and claims in the room hosted here.
	if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/messages",
		m.proposal(room, m.addr("c1"), "answer", "spam here", nil, nil))); rec.Code != http.StatusCreated {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+room+"/claims?author="+m.addr("c1"),
		map[string]any{"paths": []string{"x/"}})); rec.Code/100 != 2 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	// d hosts a room of our b1: the replica holds d's bot c2 and its
	// message, and b1's claim and read.
	b1 := g.bot(t, "b1")
	rep := ulid.Make().String()
	if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+rep+"/invite",
		map[string]any{"name": "theirs", "handle": "b1", "rights": []string{"read", "answer"}})); rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	if rec := g.fedDo(m.req(t, http.MethodPost, "/fed/v1/rooms/"+rep+"/events", map[string]any{"events": []fedrooms.Event{
		memberEv(1, m.addr("c2"), "read"), msg(2, 1, rep, d, m.addr("c2"), "spam there", nil, "", nil)}})); rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{
		`INSERT INTO claims(room_id, key_id, path, created_at, expires_at) VALUES (?, ?, 'y/', '2026-11-14T00:00:00Z', 4102444800)`,
		`INSERT INTO direct_reads(room_id, key_id, last_read_ts, last_read_id) VALUES (?, ?, '2026-11-14T00:00:00Z', 1)`,
	} {
		if _, err := g.db.Exec(q, rep, b1); err != nil {
			t.Fatal(err)
		}
	}
	// A third host e has d's bot c3 in its room, which b1 replicates.
	e := newFakeHost(t, g)
	third := ulid.Make().String()
	if rec := e.invite(t, g, third, "elsewhere", "b1", "read"); rec.Code != http.StatusCreated {
		t.Fatalf("invite of e: %d %s", rec.Code, rec.Body)
	}
	if got := e.push(t, g, third, memberEv(1, m.addr("c3"), "read"), msg(2, 1, third, e.id.Domain, m.addr("c3"), "spam elsewhere", nil, "", m.id)); got != 2 {
		t.Fatalf("e's push applied %d", got)
	}
	wantStatus(t, adminDo(g.svc, admin, http.MethodPatch, "/v1/federation/peers/"+d, `{"limits":{"requests_per_minute":5}}`), http.StatusOK)
	if _, err := g.db.Exec(`INSERT INTO federation_peer_keys(domain, key_id, public_key, fingerprint, pinned_at, retired_at)
		VALUES (?, 'ed25519:old', x'00', 'SHA256:old', '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z')`, d); err != nil {
		t.Fatal(err)
	}
	g.blockPeer(t, d)
	if _, err := g.svc.limitersOf(ctx, d); err != nil {
		t.Fatal(err)
	}
	// Follows from and to d's bots that the block left.
	a1 := g.bot(t, "a1")
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, target_key_id, status, created_at)
		SELECT id, 'a1@' || ?, ?, 'accepted', '2026-11-14T00:00:00Z' FROM api_keys WHERE label = ?`, g.svc.Domain(), a1, m.addr("c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1, m.addr("x")); err != nil {
		t.Fatal(err)
	}

	const want = `{"rooms":1,"messages":2,"members":2,"follows":2,"claims":1,"cursors":1,"stand_ins":4}`
	if rec := g.purge(d); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("purge: %d %s, want %s", rec.Code, rec.Body, want)
	}
	standIns := `(SELECT id FROM api_keys WHERE remote_domain = ?1)`
	for q, n := range map[string]int{
		`SELECT COUNT(*) FROM federation_peers WHERE domain = ?1 AND status = 'blocked'`:                                   1,
		`SELECT COUNT(*) FROM federation_peer_keys WHERE domain = ?1`:                                                      0,
		`SELECT COUNT(*) FROM federation_peer_limits WHERE domain = ?1`:                                                    0,
		`SELECT COUNT(*) FROM remote_bots WHERE domain = ?1`:                                                               0,
		`SELECT COUNT(*) FROM api_keys WHERE remote_domain = ?1 AND revoked_at IS NOT NULL`:                                4,
		`SELECT COUNT(*) FROM api_keys WHERE remote_domain = ?1 AND revoked_at IS NULL`:                                    0,
		`SELECT COUNT(*) FROM rooms WHERE home_domain = ?1 OR owner_key_id IN ` + standIns:                                 0,
		`SELECT COUNT(*) FROM room_members WHERE key_id IN ` + standIns:                                                    0,
		`SELECT COUNT(*) FROM messages WHERE deleted_at IS NULL AND author_key_id IN ` + standIns:                          0,
		`SELECT COUNT(*) FROM messages WHERE deleted_at IS NOT NULL AND author_key_id IN ` + standIns:                      2,
		`SELECT COUNT(*) FROM claims WHERE key_id IN ` + standIns:                                                          0,
		`SELECT COUNT(*) FROM follows WHERE follower_key_id IN ` + standIns + ` OR target LIKE '%@' || ?1`:                 0,
		`SELECT COUNT(*) FROM fed_delivery WHERE peer = ?1`:                                                                0,
		`SELECT COUNT(*) FROM fed_events WHERE payload LIKE '%spam%'`:                                                      0,
		`SELECT COUNT(*) FROM messages WHERE room_id NOT IN (SELECT id FROM rooms)`:                                        0,
		`SELECT COUNT(*) FROM room_members WHERE room_id NOT IN (SELECT id FROM rooms)`:                                    0,
		`SELECT COUNT(*) FROM claims WHERE room_id NOT IN (SELECT id FROM rooms)`:                                          0,
		`SELECT COUNT(*) FROM direct_reads WHERE room_id NOT IN (SELECT id FROM rooms)`:                                    0,
		`SELECT COUNT(*) FROM fed_events WHERE room_id NOT IN (SELECT id FROM rooms)`:                                      0,
		`SELECT COUNT(*) FROM fed_delivery WHERE room_id NOT IN (SELECT id FROM rooms)`:                                    0,
		`SELECT COUNT(*) FROM audit WHERE action = 'federation.peer.purge' AND target_id = ?1 AND detail = '` + want + `'`: 1,
	} {
		if got := count(t, g.db, q, d); got != n {
			t.Errorf("%s: %d, want %d", q, got, n)
		}
	}
	g.svc.mu.Lock()
	_, kept := g.svc.limiters[d]
	g.svc.mu.Unlock()
	if kept {
		t.Error("the peer's limiters are kept")
	}

	// A third host's events cannot bring a purged peer's bot back.
	if got := e.push(t, g, third, memberEv(3, m.addr("c3"), "read"), msg(4, 2, third, e.id.Domain, m.addr("c3"), "again", nil, "", m.id)); got != 4 {
		t.Fatalf("e's second push applied %d", got)
	}
	if got := members(t, g, third); got != "b1" {
		t.Errorf("members of e's room: %s", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM messages WHERE deleted_at IS NULL AND author_key_id IN `+standIns, d); n != 0 {
		t.Errorf("e brought back %d messages of the purged peer", n)
	}

	const zeros = `{"rooms":0,"messages":0,"members":0,"follows":0,"claims":0,"cursors":0,"stand_ins":0}`
	if rec := g.purge(d); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != zeros {
		t.Errorf("second purge: %d %s", rec.Code, rec.Body)
	}
}

// A third member server of a room hosted here gets the purged peer's
// retractions: its bot's removal and the deletes of its messages.
func TestPurgePeer_ThirdServerRetracts(t *testing.T) {
	ctx := context.Background()
	a, b, room, b1 := pair(t, "read,answer")
	c := newLiveRig(t)
	allow(t, a.svc, c.svc.id())
	allow(t, c.svc, a.svc.id())
	c.bot(t, "c1")
	a.invite(t, room, "c1@"+c.svc.Domain(), "read")
	post(t, b, b1, room, `{"content":"from b","message_type":"answer"}`)
	a.svc.deliverDue(ctx)
	live := `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'from b' AND deleted_at IS NULL`
	if n := count(t, c.db, live, room); n != 1 {
		t.Fatalf("c has %d copies of b's message", n)
	}
	a.blockPeer(t, b.svc.Domain())
	wantStatus(t, a.purge(b.svc.Domain()), http.StatusOK)
	a.svc.deliverDue(ctx)
	if n := count(t, c.db, live, room); n != 0 {
		t.Error("c keeps b's message")
	}
	if got := members(t, c, room); strings.Contains(got, b.svc.Domain()) {
		t.Errorf("c's members: %s", got)
	}
	if applied, total := appliedSeq(t, c, room), int64(len(a.events(t, room))); applied != total {
		t.Errorf("c applied %d of %d", applied, total)
	}
}

// Blocking a peer deletes the pending and rejected follow requests of
// its bots; another peer's stay.
func TestBlockPeer_EndsFollowRequests(t *testing.T) {
	g := newRoomRig(t)
	h, other := newFakeHost(t, g), newFakeHost(t, g)
	g.followBot(t, "a1", "approve")
	for _, f := range []struct {
		h    *fakeHost
		name string
	}{{h, "c1"}, {h, "c2"}, {other, "c1"}} {
		if rec := f.h.follow(t, g, f.h.addr(f.name), "a1"); rec.Code != http.StatusCreated {
			t.Fatalf("follow of %s: %d %s", f.h.addr(f.name), rec.Code, rec.Body)
		}
	}
	if _, err := g.db.Exec(`UPDATE follows SET status = 'rejected' WHERE follower_key_id = (SELECT id FROM api_keys WHERE label = ?)`,
		h.addr("c2")); err != nil {
		t.Fatal(err)
	}
	g.blockPeer(t, h.id.Domain)
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows f JOIN api_keys k ON k.id = f.follower_key_id WHERE k.remote_domain = ?`, h.id.Domain); n != 0 {
		t.Errorf("%d follow requests of the blocked peer's bots left", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM follows`); n != 1 {
		t.Errorf("%d follows, want the other peer's 1", n)
	}
}

// Unblocking a bot whose server is a blocked peer keeps its stand-in
// revoked.
func TestUnblockBot_PeerBlocked(t *testing.T) {
	g, m, _, c1 := fedRoom(t, "read")
	g.block(t, m.addr("c1"))
	g.blockPeer(t, m.id.Domain)
	wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, blockedPath+"/"+m.addr("c1"), ""), http.StatusNoContent)
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE id = ? AND revoked_at IS NOT NULL`, c1); n != 1 {
		t.Error("the stand-in is no longer revoked")
	}
}

// The peer and blocked-bot routes decode their path parameter, which
// the web UI encodes: a port's : as %3A, an address's @ as %40.
func TestAdmin_PathDecoding(t *testing.T) {
	g, m, _, _ := fedRoom(t, "read")
	peer := "/v1/federation/peers/" + strings.ReplaceAll(m.id.Domain, ":", "%3A")
	for _, c := range []struct {
		method, suffix, body string
		status               int
	}{
		{http.MethodGet, "", "", http.StatusOK},
		{http.MethodPatch, "", `{"limits":{"requests_per_minute":5}}`, http.StatusOK},
		{http.MethodPost, "/retry", "", http.StatusOK},
		{http.MethodPost, "/ping", "", http.StatusBadGateway}, // the member server answers no ping
		{http.MethodPost, "/block", "", http.StatusOK},
		{http.MethodPost, "/purge", "", http.StatusOK},
		{http.MethodDelete, "", "", http.StatusNoContent},
	} {
		if rec := adminDo(g.svc, admin, c.method, peer+c.suffix, c.body); rec.Code != c.status {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.suffix, rec.Code, rec.Body, c.status)
		}
	}
	g.block(t, "c1@a.example:8443")
	wantStatus(t, adminDo(g.svc, admin, http.MethodDelete, blockedPath+"/c1%40a.example%3A8443", ""), http.StatusNoContent)
}

// A host introduces at most MaxStandInsPerHost stand-ins of other
// domains: the next is refused as a bad event. Its own domain's and
// those it did not introduce do not count; a direct invite's other bot
// is introduced by its host too.
func TestStandInCap_PerHost(t *testing.T) {
	g, h, room, _ := replica(t, "read")
	host := h.id.Domain
	add := func(n int, name, domain string, via any) {
		t.Helper()
		if _, err := g.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?1)
			INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain, remote_via)
			SELECT ?2 || i || '@' || ?3, '', '', '2026-01-01T00:00:00Z', 'external', ?3, ?4 FROM n`, n, name, domain, via); err != nil {
			t.Fatal(err)
		}
	}
	add(fedrooms.MaxStandInsPerHost-1, "t", "x.example", host)
	add(5, "o", host, host)
	add(5, "n", "x.example", nil)
	if got := h.push(t, g, room, memberEv(1, "t200@y.example", "read"), memberEv(2, "t201@y.example", "read"),
		memberEv(3, h.addr("own"), "read")); got != 3 {
		t.Fatalf("applied %d, want 3", got)
	}
	if got := members(t, g, room); got != "b1 "+h.addr("own")+" t200@y.example" {
		t.Errorf("members: %s", got)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE label IN ('t200@y.example', ?) AND remote_via = ?`, h.addr("own"), host); n != 2 {
		t.Errorf("%d new stand-ins record their host, want 2", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.event_refused' AND detail LIKE '%"count":1%'`); n != 1 {
		t.Error("the refusal is not audited")
	}
	g.dmBot(t, "b2", "peers")
	if rec := h.directInvite(t, g, ulid.Make().String(), h.pairOf(g, "b2"), "b2"); rec.Code != http.StatusCreated {
		t.Fatalf("direct invite: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM api_keys WHERE label = ? AND remote_via = ?`, h.addr("x"), host); n != 1 {
		t.Error("the direct invite's bot does not record its host")
	}
}
