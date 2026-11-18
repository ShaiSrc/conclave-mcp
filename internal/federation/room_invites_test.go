package federation

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/hermes/conclave-mcp/internal/bots"
)

// A bot whose room invites are none answers a room invite from another
// server exactly as an unknown bot; the audit row tells them apart. A
// bot with peers takes it, and direct and feed invites of a bot with
// none still follow its DM policy and its follows.
func TestRoomInvites_None(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	b1 := g.dmBot(t, "b1", "peers")
	g.bot(t, "b2")
	ctx, none, peers := context.Background(), bots.InvitesNone, bots.Peers
	if _, err := g.svc.botStore.Update(ctx, "b1", bots.Patch{RoomInvites: &none, Visibility: &peers}, true); err != nil {
		t.Fatal(err)
	}
	room := ulid.Make().String()
	unknown := h.invite(t, g, room, "proj", "nobody", "read")
	refused := h.invite(t, g, room, "proj", "b1", "read")
	wantErr(t, unknown, http.StatusNotFound, "not_found")
	if refused.Code != unknown.Code || refused.Body.String() != unknown.Body.String() {
		t.Errorf("refused: %d %s, unknown: %d %s", refused.Code, refused.Body, unknown.Code, unknown.Body)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms after the refusal", n)
	}
	if n := count(t, g.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.room.invite' AND target_id = ? AND room_id = ?
		AND result = 'error:room_invites' AND detail LIKE '%"handle":"b1"%'`, h.id.Domain, room); n != 1 {
		t.Errorf("room_invites audit rows: %d", n)
	}
	if rec := h.invite(t, g, room, "proj", "b2", "read"); rec.Code != http.StatusCreated {
		t.Errorf("b2 (peers): %d %s", rec.Code, rec.Body)
	}

	if rec := h.directInvite(t, g, ulid.Make().String(), h.pairOf(g, "b1"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("direct invite: %d %s", rec.Code, rec.Body)
	}
	if _, err := g.db.Exec(`INSERT INTO follows(follower_key_id, target, status, created_at) VALUES (?, ?, 'pending', '2026-11-14T00:00:00Z')`,
		b1.KeyID, h.addr("x")); err != nil {
		t.Fatal(err)
	}
	if rec := h.feedInvite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), "b1"); rec.Code != http.StatusCreated {
		t.Errorf("feed invite: %d %s", rec.Code, rec.Body)
	}

	// Peers never read the setting.
	for _, path := range []string{"/fed/v1/bots/b1", "/fed/v1/directory"} {
		rec := g.fedDo(signedReq(t, h.id, g.svc.Domain(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"handle":"b1"`) || strings.Contains(rec.Body.String(), "room_invites") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}
