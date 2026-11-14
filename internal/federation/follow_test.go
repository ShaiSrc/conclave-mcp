package federation

import (
	"net/http"
	"testing"

	"github.com/oklog/ulid/v2"
)

// The feed: prefix is the feeds': a room invite with such a name is
// refused.
func TestRoomInvite_FeedPrefixReserved(t *testing.T) {
	g := newRoomRig(t)
	h := newFakeHost(t, g)
	g.bot(t, "b1")
	wantErr(t, h.invite(t, g, ulid.Make().String(), "feed:"+h.addr("x"), "b1", "read"), http.StatusBadRequest, "bad_request")
	if n := count(t, g.db, `SELECT COUNT(*) FROM rooms`); n != 0 {
		t.Errorf("%d rooms", n)
	}
}
