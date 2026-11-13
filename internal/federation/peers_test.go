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
	if ok, err := ps.RecordSeen(ctx, "a.example", "SHA256:one"); err != nil || !ok {
		t.Fatalf("RecordSeen: %v, %v", ok, err)
	}
	if ok, err := ps.RecordSeen(ctx, "a.example", "SHA256:two"); err != nil || ok {
		t.Fatalf("second RecordSeen: %v, %v", ok, err)
	}
	if p := get("a.example"); p.Status != "seen" || p.Fingerprint != "SHA256:one" || p.AddedAt.IsZero() || p.LastSeenAt != nil {
		t.Errorf("seen peer: %+v", p)
	}

	// Allow turns it into an allowed peer with the new fingerprint.
	if err := ps.Allow(ctx, "a.example", "SHA256:three", "lab"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "allowed" || p.Fingerprint != "SHA256:three" || p.Note != "lab" {
		t.Errorf("allowed peer: %+v", p)
	}

	// Block keeps the fingerprint; Allow clears the block.
	if err := ps.Block(ctx, "a.example"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "blocked" || p.Fingerprint != "SHA256:three" {
		t.Errorf("blocked peer: %+v", p)
	}
	if err := ps.Allow(ctx, "a.example", "SHA256:three", "lab"); err != nil {
		t.Fatal(err)
	}
	if p := get("a.example"); p.Status != "allowed" {
		t.Errorf("Allow did not clear the block: %+v", p)
	}

	// Block of an unknown domain inserts it; RecordSeen leaves it blocked.
	if err := ps.Block(ctx, "c.example"); err != nil {
		t.Fatal(err)
	}
	if ok, err := ps.RecordSeen(ctx, "c.example", "SHA256:x"); err != nil || ok {
		t.Errorf("RecordSeen over a blocked peer: %v, %v", ok, err)
	}
	if p := get("c.example"); p.Status != "blocked" || p.Fingerprint != "" {
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
