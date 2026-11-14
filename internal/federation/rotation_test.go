package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/auth"
)

// rotatedPeer is a peer on 127.0.0.1 that rotated its key from old to
// cur. It serves cur's document, changed by mutate when set; a mutate
// that returns false answers 500 instead.
func rotatedPeer(t *testing.T, mutate func(old, cur *Identity, doc *Document) bool) (old, cur *Identity, hits *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	db := testDB(t)
	if old, err = LoadOrCreateIdentity(ctx, db, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if cur, err = Rotate(ctx, db, old, time.Now()); err != nil {
		t.Fatal(err)
	}
	hits = new(atomic.Int32)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		doc := cur.Document(time.Now())
		if mutate != nil && !mutate(old, cur, &doc) {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return old, cur, hits
}

// docOf is svc's server document.
func docOf(t *testing.T, svc *Service) Document {
	t.Helper()
	rec := serve(svc, httptest.NewRequest(http.MethodGet, "/.well-known/conclave", nil))
	var doc Document
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
		t.Fatalf("document: %d %s", rec.Code, rec.Body)
	}
	return doc
}

// Rotation makes a new key current, endorsed by the old one, which the
// document lists as previous for 30 days; a second rotation within them
// is refused, and keys survive a restart.
func TestRotateKey(t *testing.T) {
	svc := newLocal(t, "allowlist")
	now := time.Now()
	svc.now = func() time.Time { return now }
	old := svc.id()
	rec := adminDo(svc, admin, http.MethodPost, "/v1/federation/keys/rotate", "")
	var out struct {
		KeyID       string `json:"key_id"`
		Fingerprint string `json:"fingerprint"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	cur := svc.id()
	if cur.KeyID == old.KeyID || out.KeyID != cur.KeyID || out.Fingerprint != cur.Fingerprint() {
		t.Fatalf("rotate answered %+v; current %s, old %s", out, cur.KeyID, old.KeyID)
	}

	doc := docOf(t, svc)
	if len(doc.Keys) != 2 {
		t.Fatalf("document keys: %+v", doc.Keys)
	}
	k, p := doc.Keys[0], doc.Keys[1]
	if k.KeyID != cur.KeyID || k.Status != "current" || k.Until != "" || k.EndorsedBy == nil || k.EndorsedBy.KeyID != old.KeyID {
		t.Errorf("current key: %+v", k)
	} else if sig, err := decodeB64(k.EndorsedBy.Sig); err != nil ||
		!ed25519.Verify(old.Public, EndorsementString(local, old.KeyID, cur.KeyID, b64.EncodeToString(cur.Public)), sig) {
		t.Errorf("endorsement does not verify with the old key: %+v", k.EndorsedBy)
	}
	if until := now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339); p.KeyID != old.KeyID || p.Status != "previous" ||
		p.Until != until || p.EndorsedBy != nil || p.Fingerprint != old.Fingerprint() {
		t.Errorf("previous key: %+v, want until %s", p, until)
	}

	var actor int64
	var targetType, targetID, detail string
	if err := svc.db.QueryRow(`SELECT actor_key_id, target_type, target_id, detail FROM audit WHERE action = 'federation.key.rotate'`).
		Scan(&actor, &targetType, &targetID, &detail); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"previous":%q,"current":%q}`, old.Fingerprint(), cur.Fingerprint()); actor != 1 ||
		targetType != "federation_key" || targetID != cur.KeyID || detail != want {
		t.Errorf("audit %d %s %s %s, want %s", actor, targetType, targetID, detail, want)
	}

	// GET /v1/federation lists the keys.
	var st struct {
		KeyID string   `json:"key_id"`
		Keys  []DocKey `json:"keys"`
	}
	rec = adminDo(svc, admin, http.MethodGet, "/v1/federation", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.KeyID != cur.KeyID || len(st.Keys) != 2 ||
		st.Keys[0].Status != "current" || st.Keys[1].Status != "previous" || st.Keys[1].Until != p.Until {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}

	// Within the overlap: 409.
	wantErr(t, adminDo(svc, admin, http.MethodPost, "/v1/federation/keys/rotate", ""), http.StatusConflict, "rotation_too_soon")
	if svc.id().KeyID != cur.KeyID || count(t, svc.db, `SELECT COUNT(*) FROM federation_keys`) != 2 {
		t.Error("a refused rotation changed the keys")
	}

	// After it, the previous key is gone from the document, and the next
	// rotation goes through.
	now = now.Add(30*24*time.Hour + time.Second)
	if doc := docOf(t, svc); len(doc.Keys) != 1 || doc.Keys[0].KeyID != cur.KeyID || doc.Keys[0].EndorsedBy == nil {
		t.Errorf("document after the overlap: %+v", doc.Keys)
	}
	if rec := adminDo(svc, admin, http.MethodPost, "/v1/federation/keys/rotate", ""); rec.Code != http.StatusOK {
		t.Fatalf("rotate after the overlap: %d %s", rec.Code, rec.Body)
	}
	third := svc.id()
	if doc := docOf(t, svc); len(doc.Keys) != 2 || doc.Keys[0].KeyID != third.KeyID || doc.Keys[0].EndorsedBy.KeyID != cur.KeyID ||
		doc.Keys[1].KeyID != cur.KeyID {
		t.Errorf("document after the second rotation: %+v", doc.Keys)
	}

	// A restart loads the current key.
	again, err := New(svc.db, Options{Domain: local, Mode: "allowlist"})
	if err != nil {
		t.Fatal(err)
	}
	if again.id().KeyID != third.KeyID || len(again.id().keys) != 3 {
		t.Errorf("after a restart: key %s, %d keys", again.id().KeyID, len(again.id().keys))
	}
}

// Readers of the identity run while a rotation swaps it (go test -race).
func TestRotateKey_ConcurrentReaders(t *testing.T) {
	svc := newLocal(t, "allowlist")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			serve(svc, httptest.NewRequest(http.MethodGet, "/.well-known/conclave", nil))
			_ = svc.id().SignMessage(MessageSig{Nonce: newNonce()})
		}
	}()
	wantStatus(t, adminDo(svc, admin, http.MethodPost, "/v1/federation/keys/rotate", ""), http.StatusOK)
	<-done
}

// A known peer that signs with a new key its document lists, endorsed
// by the pinned key, is re-pinned and its request goes on; its old key
// goes to its history.
func TestVerify_Repin(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"allowlist", "open"} {
		svc := newLocal(t, mode)
		allowLoopback(svc)
		old, cur, hits := rotatedPeer(t, nil)
		status := statusAllowed
		if mode == "allowlist" {
			allow(t, svc, old)
		} else if _, err := svc.peers.RecordSeen(ctx, old.Domain, old.Public); err != nil {
			t.Fatal(err)
		} else {
			status = statusSeen
		}
		wantOK(t, serve(svc, signed(t, cur, local, "{}")))
		p, err := svc.peers.Get(ctx, old.Domain)
		if err != nil || p.Fingerprint != cur.Fingerprint() || !p.PublicKey.Equal(cur.Public) || p.Status != status {
			t.Errorf("%s: peer %+v %v", mode, p, err)
		}
		if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peer_keys WHERE domain = ? AND key_id = ? AND fingerprint = ?
			AND public_key = ? AND pinned_at != '' AND retired_at != ''`, old.Domain, old.KeyID, old.Fingerprint(), []byte(old.Public)); n != 1 {
			t.Errorf("%s: history rows %d", mode, n)
		}
		want := fmt.Sprintf(`{"previous":%q,"current":%q}`, old.Fingerprint(), cur.Fingerprint())
		if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.key_rotated' AND target_id = ? AND result = 'ok'
			AND detail = ?`, old.Domain, want); n != 1 {
			t.Errorf("%s: key_rotated rows %d", mode, n)
		}
		// The new key goes on without a fetch; the old one is refused.
		wantOK(t, serve(svc, signed(t, cur, local, "{}")))
		wantErr(t, serve(svc, signed(t, old, local, "{}")), http.StatusUnauthorized, "bad_signature")
		if n := hits.Load(); n != 1 {
			t.Errorf("%s: document fetched %d times", mode, n)
		}
		if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.key_changed'`); n != 0 {
			t.Errorf("%s: key_changed rows %d", mode, n)
		}
	}
}

// Without a valid endorsement by the pinned key, a new key is refused
// and audited, and nothing is re-pinned.
func TestVerify_RepinRefused(t *testing.T) {
	ctx := context.Background()
	attacker := testIdentity(t, "x.example")
	for _, tc := range []struct {
		name   string
		mutate func(old, cur *Identity, doc *Document) bool
		// pin pins the peer; nil pins old.
		pin func(svc *Service, old, cur *Identity)
		// fromOld signs the request with old instead of cur.
		fromOld bool
		reason  string
	}{
		{name: "forged endorsement", reason: "bad_endorsement", mutate: func(old, cur *Identity, doc *Document) bool {
			doc.Keys[0].EndorsedBy.Sig = b64.EncodeToString(attacker.Sign(
				EndorsementString(doc.Domain, old.KeyID, cur.KeyID, b64.EncodeToString(cur.Public))))
			return true
		}},
		{name: "no endorsement", reason: "not_endorsed", mutate: func(_, _ *Identity, doc *Document) bool {
			doc.Keys[0].EndorsedBy = nil
			return true
		}},
		{name: "endorsed by another key", reason: "not_endorsed", pin: func(svc *Service, old, _ *Identity) {
			other := testIdentity(t, old.Domain)
			allow(t, svc, other)
		}},
		{name: "key from the history", reason: "retired_key", fromOld: true,
			pin: func(svc *Service, old, cur *Identity) { allow(t, svc, old); allow(t, svc, cur) },
			mutate: func(old, cur *Identity, doc *Document) bool {
				k := doc.Keys[1] // old, endorsed by cur, which it would replace
				k.Status, k.Until = "current", ""
				k.EndorsedBy = &Endorsement{KeyID: cur.KeyID, Sig: b64.EncodeToString(cur.Sign(
					EndorsementString(doc.Domain, cur.KeyID, old.KeyID, k.PublicKey)))}
				doc.Keys = []DocKey{k}
				return true
			}},
		{name: "document fetch failure", reason: "unreachable", mutate: func(_, _ *Identity, _ *Document) bool { return false }},
	} {
		svc := newLocal(t, "allowlist")
		old, cur, _ := rotatedPeer(t, tc.mutate)
		if tc.pin == nil {
			allow(t, svc, old)
		} else {
			tc.pin(svc, old, cur)
		}
		pinned, err := svc.peers.Get(ctx, old.Domain)
		if err != nil {
			t.Fatal(err)
		}
		history := count(t, svc.db, `SELECT COUNT(*) FROM federation_peer_keys`)
		signer := cur
		if tc.fromOld {
			signer = old
		}
		wantErr(t, serve(svc, signed(t, signer, local, "{}")), http.StatusUnauthorized, "bad_signature")
		presented := signer.Fingerprint()
		if tc.reason == "unreachable" {
			presented = signer.KeyID
		}
		want := fmt.Sprintf(`{"presented":%q,"expected":%q,"reason":%q}`, presented, pinned.Fingerprint, tc.reason)
		var result, detail string
		if err := svc.db.QueryRow(`SELECT result, detail FROM audit WHERE action = 'federation.key_changed' AND target_id = ?`, old.Domain).
			Scan(&result, &detail); err != nil || result != "error:bad_signature" || detail != want {
			t.Errorf("%s: key_changed %s %s %v, want %s", tc.name, result, detail, err, want)
		}
		if p, err := svc.peers.Get(ctx, old.Domain); err != nil || p.Fingerprint != pinned.Fingerprint {
			t.Errorf("%s: re-pinned to %s", tc.name, p.Fingerprint)
		}
		if n := count(t, svc.db, `SELECT COUNT(*) FROM federation_peer_keys`); n != history {
			t.Errorf("%s: history rows %d, want %d", tc.name, n, history)
		}
		if n := count(t, svc.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.key_rotated'`); n != 0 {
			t.Errorf("%s: key_rotated rows %d", tc.name, n)
		}
	}
}

// The operator's re-pin keeps the replaced key in the history.
func TestPeers_AllowKeepsHistory(t *testing.T) {
	ctx := context.Background()
	ps := &Peers{db: testDB(t)}
	one, two := testIdentity(t, "a.example"), testIdentity(t, "a.example")
	for _, id := range []*Identity{one, one, two} {
		if err := ps.Allow(ctx, "a.example", id.Public, ""); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := ps.History(ctx, "a.example")
	if err != nil || len(keys) != 1 || !keys[0].Equal(one.Public) {
		t.Errorf("history %v %v, want the first key", keys, err)
	}
}

// After a re-pin, author signatures made with the peer's old key still
// verify, as do our own made with our old key after our rotation.
func TestReplicaEvents_OldAuthorSignatures(t *testing.T) {
	g, h, room, _ := replica(t, "read", "answer")
	old, cur, _ := rotatedPeer(t, nil)
	allow(t, g.svc, old)
	ours := g.svc.id()
	wantStatus(t, adminDo(g.svc, admin, http.MethodPost, "/v1/federation/keys/rotate", ""), http.StatusOK)
	wantOK(t, g.fedDo(signed(t, cur, local, "{}")))
	c1 := "c1@" + old.Domain
	for i, tc := range []struct {
		name, author string
		signer       *Identity
		stored       bool
	}{
		{"peer's old key", c1, old, true},
		{"peer's new key", c1, cur, true},
		{"our old key", "b1@" + local, ours, true},
		{"our new key", "b1@" + local, g.svc.id(), true},
		{"another key", c1, testIdentity(t, old.Domain), false},
	} {
		seq, id := int64(i+1), int64(100+i)
		if got := h.push(t, g, room, msg(seq, id, room, h.id.Domain, tc.author, tc.name, nil, "", tc.signer)); got != seq {
			t.Fatalf("%s: applied %d", tc.name, got)
		}
		n := count(t, g.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND remote_id = ? AND unverified = 0`, room, id)
		if (n == 1) != tc.stored {
			t.Errorf("%s: stored %d", tc.name, n)
		}
	}
}

// Server a rotates its key: b follows through the endorsement on a's
// next request, room delivery and forwarded messages go on, and a's
// bot's message signed with a's old key still verifies in a pull.
func TestKeyRotation_TwoServers(t *testing.T) {
	ctx := context.Background()
	a, b, room, b1 := pair(t, "read,answer")
	a1 := a.localBot(t, room, "a1-key", "read,answer")
	// b hosts back, with a's bot a2, which posts before the rotation.
	a2 := auth.Claims{KeyID: a.bot(t, "a2")}
	back := b.room(t, "back")
	b.invite(t, back, "a2@"+a.svc.Domain(), "read,answer")
	b.svc.deliverDue(ctx)
	post(t, a, a2, back, `{"content":"signed before","message_type":"answer"}`)
	oldFP := a.svc.id().Fingerprint()

	wantStatus(t, adminDo(a.svc, admin, http.MethodPost, "/v1/federation/keys/rotate", ""), http.StatusOK)
	newFP := a.svc.id().Fingerprint()

	// a's next request, a push, re-pins a on b and is applied.
	post(t, a, a1, room, `{"content":"after rotation","message_type":"answer"}`)
	a.svc.deliverDue(ctx)
	if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'after rotation'`, room); n != 1 {
		t.Fatalf("b has %d copies of a's message", n)
	}
	if p, err := b.svc.peers.Get(ctx, a.svc.Domain()); err != nil || p.Fingerprint != newFP {
		t.Errorf("b pins %s %v, want %s", p.Fingerprint, err, newFP)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.key_rotated' AND target_id = ? AND detail = ?`,
		a.svc.Domain(), fmt.Sprintf(`{"previous":%q,"current":%q}`, oldFP, newFP)); n != 1 {
		t.Errorf("b's key_rotated rows %d", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM federation_peer_keys WHERE domain = ? AND fingerprint = ?`, a.svc.Domain(), oldFP); n != 1 {
		t.Errorf("b's history of a: %d rows", n)
	}
	if n := count(t, b.db, `SELECT COUNT(*) FROM audit WHERE action = 'federation.key_changed'`); n != 0 {
		t.Errorf("b's key_changed rows %d", n)
	}

	// b1's message is forwarded to a and comes back once.
	post(t, b, b1, room, `{"content":"from b","message_type":"answer"}`)
	if n := count(t, a.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'from b'`, room); n != 1 {
		t.Errorf("a has %d copies of b's message", n)
	}
	a.svc.deliverDue(ctx)
	if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'from b'`, room); n != 1 {
		t.Errorf("b has %d copies of its message", n)
	}

	// a pulls back again: a2's message, signed with a's old key, verifies.
	if _, err := a.db.Exec(`DELETE FROM messages WHERE room_id = ?`, back); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE rooms SET applied_seq = 0 WHERE id = ?`, back); err != nil {
		t.Fatal(err)
	}
	a.svc.catchUp(ctx)
	if n := count(t, a.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'signed before' AND unverified = 0`, back); n != 1 {
		t.Errorf("a's pull stored %d copies of a2's old message", n)
	}
	post(t, a, a2, back, `{"content":"signed after","message_type":"answer"}`)
	if n := count(t, b.db, `SELECT COUNT(*) FROM messages WHERE room_id = ? AND content = 'signed after'`, back); n != 1 {
		t.Errorf("b has %d copies of a2's new message", n)
	}
}
