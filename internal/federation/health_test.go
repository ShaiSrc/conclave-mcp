package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// peerOf is svc's view of the peer domain.
func peerOf(t *testing.T, svc *Service, domain string) peerView {
	t.Helper()
	rec := adminDo(svc, admin, http.MethodGet, "/v1/federation/peers/"+domain, "")
	var v peerView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("peer %s: %d %s", domain, rec.Code, rec.Body)
	}
	return v
}

// lastOut is peer's last_out_at in db, "" while unset.
func lastOut(t *testing.T, db *sql.DB, peer string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(`SELECT last_out_at FROM federation_peers WHERE domain = ?`, peer).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func clearOut(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE federation_peers SET last_out_at = NULL`); err != nil {
		t.Fatal(err)
	}
}

// A successful outbound request sets last_out_at: an invite, a push, a
// ping; a failed one does not.
func TestPeerHealth_LastOut(t *testing.T) {
	ctx := context.Background()
	g, m, room, _ := fedRoom(t, "read")
	if lastOut(t, g.db, m.id.Domain) == "" {
		t.Error("not set by the invite")
	}
	clearOut(t, g.db)
	m.setReply(applyAll)
	g.svc.deliverDue(ctx)
	if lastOut(t, g.db, m.id.Domain) == "" {
		t.Error("not set by a push")
	}
	clearOut(t, g.db)
	m.setReply(func(string, []byte) (int, string) { return http.StatusInternalServerError, "" })
	g.localBot(t, room, "a1-key", "read")
	g.svc.deliverDue(ctx)
	if got := lastOut(t, g.db, m.id.Domain); got != "" {
		t.Errorf("set by a failed push: %s", got)
	}

	svc := newLocal(t, "allowlist")
	rm := newRemote(t)
	allow(t, svc, rm.id)
	wantStatus(t, adminDo(svc, admin, http.MethodPost, "/v1/federation/peers/"+rm.domain()+"/ping", ""), http.StatusOK)
	if lastOut(t, svc.db, rm.domain()) == "" || peerOf(t, svc, rm.domain()).LastOutAt == nil {
		t.Error("not set by a ping")
	}
}

// The view sums a peer's delivery cursors: unreachable when one is
// paused, failing when one fails, ok otherwise; failing_since is the
// earliest, with its error.
func TestPeerHealth_Rollup(t *testing.T) {
	ctx := context.Background()
	g, m, room, _ := fedRoom(t, "read")
	room2 := g.room(t, "two")
	g.invite(t, room2, m.addr("c2"), "read")
	peer := m.id.Domain
	m.setReply(applyAll)
	g.svc.deliverDue(ctx)
	v := peerOf(t, g.svc, peer)
	if v.Health != "ok" || v.Delivery.Rooms != 2 || v.Delivery.Pending != 0 || v.Delivery.Paused != 0 ||
		v.Delivery.FailingSince != nil || v.LastOutAt == nil {
		t.Errorf("delivered: %+v", v)
	}

	m.setReply(func(string, []byte) (int, string) { return http.StatusInternalServerError, "" })
	g.localBot(t, room, "a1-key", "read")
	g.svc.deliverDue(ctx)
	early, late := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err := g.db.Exec(`UPDATE fed_delivery SET failing_since = ?, last_error = 'boom' WHERE room_id = ?`, storage.FormatTS(early), room); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`UPDATE fed_delivery SET failing_since = ?, last_error = 'later' WHERE room_id = ?`, storage.FormatTS(late), room2); err != nil {
		t.Fatal(err)
	}
	v = peerOf(t, g.svc, peer)
	if v.Health != "failing" || v.Delivery.Pending != 1 || v.Delivery.Paused != 0 || v.Delivery.FailingSince == nil ||
		*v.Delivery.FailingSince != "2026-11-01T00:00:00Z" || v.Delivery.LastError != "boom" {
		t.Errorf("failing: %+v", v.Delivery)
	}

	if _, err := g.db.Exec(`UPDATE fed_delivery SET paused = 1 WHERE room_id = ?`, room2); err != nil {
		t.Fatal(err)
	}
	var l struct{ Peers []peerView }
	rec := adminDo(g.svc, admin, http.MethodGet, "/v1/federation/peers", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &l) != nil || len(l.Peers) != 1 ||
		l.Peers[0].Health != "unreachable" || l.Peers[0].Delivery.Paused != 1 || l.Peers[0].Delivery.Rooms != 2 {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}

	// A peer without cursors is ok; an unknown one is 404.
	other := testIdentity(t, "o.example")
	allow(t, g.svc, other)
	if v := peerOf(t, g.svc, "o.example"); v.Health != "ok" || v.Delivery.Rooms != 0 || v.LastOutAt != nil {
		t.Errorf("no cursors: %+v", v)
	}
	wantErr(t, adminDo(g.svc, admin, http.MethodGet, "/v1/federation/peers/x.example", ""), http.StatusNotFound, "peer_not_found")
}

// Retry unpauses a peer's cursors and makes them due at once.
func TestPeerRetry(t *testing.T) {
	ctx := context.Background()
	g, m, room, _ := fedRoom(t, "read")
	peer := m.id.Domain
	now := time.Now()
	if _, err := g.db.Exec(`UPDATE fed_delivery SET paused = 1, attempts = 9, next_try_at = ?, failing_since = ?, last_error = 'boom'`,
		storage.FormatTS(now.Add(time.Hour)), storage.FormatTS(now.Add(-8*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if v := peerOf(t, g.svc, peer); v.Health != "unreachable" {
		t.Errorf("before: %s", v.Health)
	}
	rec := adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+peer+"/retry", "")
	var v peerView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.Health != "failing" || v.Delivery.Paused != 0 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if _, attempts, next, _, paused := cursorOf(t, g, room, peer); paused || attempts != 0 || next > storage.FormatTS(time.Now()) {
		t.Errorf("cursor: attempts %d next %s paused %v", attempts, next, paused)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.delivery.retry' AND target_id = ? AND detail = '{"cursors":1}'`, peer); n != 1 {
		t.Errorf("retry audit rows %d", n)
	}
	m.setReply(applyAll)
	g.svc.deliverDue(ctx)
	if acked, _, _, failing, _ := cursorOf(t, g, room, peer); acked == 0 || failing != "" {
		t.Errorf("not delivered after the retry: acked %d failing %s", acked, failing)
	}
	if v := peerOf(t, g.svc, peer); v.Health != "ok" {
		t.Errorf("after delivery: %s", v.Health)
	}

	wantErr(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/x.example/retry", ""), http.StatusNotFound, "peer_not_found")
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+peer+"/block", ""), http.StatusOK)
	wantErr(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/peers/"+peer+"/retry", ""), http.StatusConflict, "peer_not_allowed")
}

// A seen peer gets 30 requests a minute (burst 10), an allowed one 120
// (burst 30). An override (0 refuses, null resets) replaces the peer's
// limiters at once; removing the peer drops them and its overrides.
func TestPeerLimits(t *testing.T) {
	svc := newLocal(t, "open")
	allowLoopback(svc)
	rm := newRemote(t)
	path := "/v1/federation/peers/" + rm.domain()
	ping := func() *http.Request { return signed(t, rm.id, local, "{}") }
	burst := func(n int) {
		t.Helper()
		for range n {
			wantOK(t, serve(svc, ping()))
		}
		wantErr(t, serve(svc, ping()), http.StatusTooManyRequests, "rate_limited")
	}
	patch := func(body string) peerView {
		t.Helper()
		rec := adminDo(svc, admin, http.MethodPatch, path, body)
		var v peerView
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("patch %s: %d %s", body, rec.Code, rec.Body)
		}
		return v
	}

	burst(10)
	if l := peerOf(t, svc, rm.domain()).Limits; l != (limits{limit{30, false}, limit{100, false}, limit{100, false}}) {
		t.Errorf("seen limits %+v", l)
	}
	// Allowed by the operator: the seen limiter is replaced.
	wantStatus(t, adminDo(svc, admin, http.MethodPost, "/v1/federation/peers",
		`{"domain":"`+rm.domain()+`","fingerprint":"`+rm.id.Fingerprint()+`"}`), http.StatusCreated)
	burst(30)
	if l := peerOf(t, svc, rm.domain()).Limits; l != (limits{limit{120, false}, limit{100, false}, limit{100, false}}) {
		t.Errorf("allowed limits %+v", l)
	}

	// An override applies at once; a field left out stays.
	if v := patch(`{"limits":{"requests_per_minute":6,"follow_requests_per_hour":5}}`); v.Limits != (limits{limit{6, true}, limit{100, false}, limit{5, true}}) {
		t.Errorf("override %+v", v.Limits)
	}
	burst(2)
	if v := patch(`{"limits":{"dm_invites_per_hour":0}}`); v.Limits != (limits{limit{6, true}, limit{0, true}, limit{5, true}}) {
		t.Errorf("second override %+v", v.Limits)
	}
	patch(`{"limits":{"requests_per_minute":0}}`)
	rec := serve(svc, ping())
	wantErr(t, rec, http.StatusTooManyRequests, "rate_limited")
	if ra := rec.Header().Get("Retry-After"); ra != "3600" {
		t.Errorf("Retry-After %q", ra)
	}
	// null resets; a row of defaults only is deleted.
	if v := patch(`{"limits":{"requests_per_minute":null,"dm_invites_per_hour":null,"follow_requests_per_hour":null}}`); v.Limits !=
		(limits{limit{120, false}, limit{100, false}, limit{100, false}}) {
		t.Errorf("reset %+v", v.Limits)
	}
	if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peer_limits`); n != 0 {
		t.Errorf("%d override rows after the reset", n)
	}
	burst(30)
	if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.peer.limits' AND target_id = ?`, rm.domain()); n != 4 {
		t.Errorf("limits audit rows %d", n)
	}
	var detail string
	if err := svc.db.QueryRow(`SELECT detail FROM audit WHERE action = 'federation.peer.limits' ORDER BY id LIMIT 1`).Scan(&detail); err != nil ||
		detail != `{"requests_per_minute":6,"dm_invites_per_hour":null,"follow_requests_per_hour":5}` {
		t.Errorf("audit detail %s %v", detail, err)
	}

	for _, body := range []string{`not json`, `{}`, `{"limits":null}`} {
		wantErr(t, adminDo(svc, admin, http.MethodPatch, path, body), http.StatusBadRequest, "invalid_json")
	}
	for _, body := range []string{`{"limits":{"requests_per_minute":-1}}`, `{"limits":{"requests_per_minute":10001}}`,
		`{"limits":{"dm_invites_per_hour":1.5}}`, `{"limits":{"follow_requests_per_hour":"5"}}`, `{"limits":{"burst":5}}`} {
		wantErr(t, adminDo(svc, admin, http.MethodPatch, path, body), http.StatusBadRequest, "invalid_limit")
	}
	wantErr(t, adminDo(svc, admin, http.MethodPatch, "/v1/federation/peers/x.example", `{"limits":{}}`), http.StatusNotFound, "peer_not_found")

	patch(`{"limits":{"requests_per_minute":10000}}`)
	if rec := adminDo(svc, admin, http.MethodDelete, path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rec.Code)
	}
	svc.mu.Lock()
	_, kept := svc.limiters[rm.domain()]
	svc.mu.Unlock()
	if kept || count(t, svc.db, `SELECT COUNT(*) FROM federation_peer_limits`) != 0 {
		t.Errorf("after remove: limiters kept %v", kept)
	}
}
