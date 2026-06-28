package storage

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestPruner_DeletesOldRows(t *testing.T) {
	db := newTestDB(t)
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert 3 old + 2 new audit rows. "New" means "very recent"
	// (within the retention window); "old" means well before it.
	now := time.Now().UTC()
	oldTimes := []string{
		now.Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-180 * 24 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	newTimes := []string{
		now.Add(-1 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
	}
	for _, ts := range append(oldTimes, newTimes...) {
		if _, err := db.Exec(
			`INSERT INTO audit (ts, action, target_type, target_id, result) VALUES (?, 'a', 'b', 'c', 'ok')`,
			ts,
		); err != nil {
			t.Fatalf("insert audit: %v", err)
		}
	}

	// 90d retention -> only the 2 new rows survive.
	pr, err := NewPruner(PrunerConfig{
		DB:        db,
		Retention: 90 * 24 * time.Hour,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewPruner: %v", err)
	}
	if err := pr.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var remaining int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Errorf("remaining: got %d, want 2 (3 old deleted, 2 new kept)", remaining)
	}
}

func TestPruner_RespectsRetentionBoundary(t *testing.T) {
	db := newTestDB(t)
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert a row 1 hour INSIDE the retention window (i.e. well
	// newer than the cutoff). It must survive.
	now := time.Now().UTC()
	safe := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO audit (ts, action, target_type, target_id, result) VALUES (?, 'a', 'b', 'c', 'ok')`,
		safe,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// And a row well OLDER than the retention window. It must go.
	old := now.Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO audit (ts, action, target_type, target_id, result) VALUES (?, 'a', 'b', 'c', 'ok')`,
		old,
	); err != nil {
		t.Fatalf("insert old: %v", err)
	}

	pr, err := NewPruner(PrunerConfig{
		DB:        db,
		Retention: 90 * 24 * time.Hour,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewPruner: %v", err)
	}
	if err := pr.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Only the "safe" row remains.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 row (safe), got %d", n)
	}
}

func TestPruner_BatchesLargeBacklog(t *testing.T) {
	db := newTestDB(t)
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert 2500 old rows. With batch=1000, pruner should need 3
	// iterations: 1000 + 1000 + 500.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO audit (ts, action, target_type, target_id, result) VALUES (?, 'a', 'b', 'c', 'ok')`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < 2500; i++ {
		if _, err := stmt.Exec("2020-01-01T00:00:00.000Z"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	pr, err := NewPruner(PrunerConfig{
		DB:        db,
		Retention: 30 * 24 * time.Hour,
		BatchSize: 1000,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewPruner: %v", err)
	}
	if err := pr.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows after 2500-row prune, got %d", n)
	}
}

func TestPruner_SingleflightPreventsOverlap(t *testing.T) {
	db := newTestDB(t)
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := db.Exec(
			`INSERT INTO audit (ts, action, target_type, target_id, result) VALUES ('2020-01-01T00:00:00.000Z', 'a', 'b', 'c', 'ok')`,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	pr, err := NewPruner(PrunerConfig{
		DB:        db,
		Retention: 30 * 24 * time.Hour,
		BatchSize: 100,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewPruner: %v", err)
	}

	// Fire 5 concurrent RunOnce calls. With singleflight, only one
	// actual cycle should run; the others return that result.
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { done <- pr.RunOnce(context.Background()) }()
	}
	for i := 0; i < 5; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent RunOnce: %v", err)
		}
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows after concurrent prune: got %d, want 0", n)
	}
}

func TestPruner_StopsOnContextCancel(t *testing.T) {
	db := newTestDB(t)
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pr, err := NewPruner(PrunerConfig{
		DB:        db,
		Retention: 30 * 24 * time.Hour,
		Interval:  10 * time.Millisecond,
		Logger:    slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewPruner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pr.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

func TestNewPruner_ValidatesConfig(t *testing.T) {
	if _, err := NewPruner(PrunerConfig{}); err == nil {
		t.Error("expected error for nil DB, got nil")
	}
	if _, err := NewPruner(PrunerConfig{DB: newTestDB(t)}); err != nil {
		t.Errorf("valid config: unexpected error: %v", err)
	}
}
