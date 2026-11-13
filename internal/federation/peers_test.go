package federation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPeers(t *testing.T) {
	ctx := context.Background()
	ps := &Peers{db: testDB(t)}
	one, two, three := testIdentity(t, "a.example"), testIdentity(t, "a.example"), testIdentity(t, "a.example")
	get := func(domain string) Peer {
		t.Helper()
		p, err := ps.Get(ctx, domain)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := ps.Get(ctx, "a.example"); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("Get unknown: %v", err)
	}

	// RecordSeen inserts only if absent.
	if ok, err := ps.RecordSeen(ctx, "a.example", one.Public); err != nil || !ok {
		t.Fatalf("RecordSeen: %v, %v", ok, err)
	}
	if ok, err := ps.RecordSeen(ctx, "a.example", two.Public); err != nil || ok {
		t.Fatalf("second RecordSeen: %v, %v", ok, err)
	}
	if p := get("a.example"); p.Status != "seen" || p.Fingerprint != one.Fingerprint() || !p.PublicKey.Equal(one.Public) ||
		p.AddedAt.IsZero() || p.LastSeenAt != nil {
		t.Errorf("seen peer: %+v", p)
	}

	// Allow turns it into an allowed peer with the new key.
	if err := ps.Allow(ctx, "a.example", three.Public, "lab"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "allowed" || p.Fingerprint != three.Fingerprint() || !p.PublicKey.Equal(three.Public) || p.Note != "lab" {
		t.Errorf("allowed peer: %+v", p)
	}

	// Block keeps the key; Allow clears the block.
	if err := ps.Block(ctx, "a.example"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "blocked" || p.Fingerprint != three.Fingerprint() || !p.PublicKey.Equal(three.Public) {
		t.Errorf("blocked peer: %+v", p)
	}
	if err := ps.Allow(ctx, "a.example", three.Public, "lab"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "allowed" {
		t.Errorf("Allow did not clear the block: %+v", p)
	}

	// Block of an unknown domain inserts it; RecordSeen leaves it blocked.
	if err := ps.Block(ctx, "c.example"); err != nil {
		t.Fatal(err)
	}
	if ok, err := ps.RecordSeen(ctx, "c.example", one.Public); err != nil || ok {
		t.Errorf("RecordSeen over a blocked peer: %v, %v", ok, err)
	}
	if p := get("c.example"); p.Status != "blocked" || p.Fingerprint != "" || p.PublicKey != nil {
		t.Errorf("blocked unknown peer: %+v", p)
	}

	// Touch sets last_seen_at.
	if err := ps.Touch(ctx, "a.example"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.LastSeenAt == nil || time.Since(*p.LastSeenAt) > time.Minute {
		t.Errorf("LastSeenAt after Touch: %v", p.LastSeenAt)
	}

	list, err := ps.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Domain != "a.example" || list[1].Domain != "c.example" {
		t.Errorf("List: %+v", list)
	}

	if err := ps.Remove(ctx, "a.example"); err != nil {
		t.Fatal(err)
	}
	if err := ps.Remove(ctx, "a.example"); !errors.Is(err, ErrPeerNotFound) {
		t.Errorf("Remove unknown: %v", err)
	}
}
