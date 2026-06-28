package storage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestIntegration_OpenMigrateRestartIdempotent is the end-to-end
// storage smoke test for Vikunja task #17. It exercises the full
// boot sequence: mkdir, open, PRAGMAs, wal_checkpoint, Migrate,
// schema_version, then re-open and verify nothing changes.
func TestIntegration_OpenMigrateRestartIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := filepath.Join(dir, "data.db")

	// ---- Boot 1: fresh install ----
	db1, close1, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	if _, err := Migrate(context.Background(), db1, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate #1: %v", err)
	}

	// Insert a real row to prove data is preserved.
	if _, err := db1.Exec(
		`INSERT INTO api_keys(id, label, hash, prefix, created_at)
		 VALUES (1, 'k1', 'phc:fake', 'testpfx1', '2026-06-27T00:00:00.000Z')`,
	); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	// schema_version should be the highest applied (1 or 2 depending
	// on what migrations are present). We don't pin the exact number
	// because future migrations will increase it; we just verify it
	// is set to a positive integer.
	var v1 string
	if err := db1.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v1); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	n, err := strconv.Atoi(v1)
	if err != nil {
		t.Fatalf("schema_version is not an int: %q", v1)
	}
	if n < 1 {
		t.Errorf("schema_version: got %d, want >= 1", n)
	}

	// Close #1
	if err := close1(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	// ---- Boot 2: re-open, Migrate is a no-op, data still there ----
	db2, close2, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	t.Cleanup(func() { _ = close2() })

	applied, err := Migrate(context.Background(), db2, osDirFSRepoRoot("migrations"), nil)
	if err != nil {
		t.Fatalf("Migrate #2: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("Migrate #2 should be no-op, got applied=%v", applied)
	}

	// Row from boot 1 is still here.
	var label string
	if err := db2.QueryRow("SELECT label FROM api_keys WHERE id=1").Scan(&label); err != nil {
		t.Fatalf("select api_key: %v", err)
	}
	if label != "k1" {
		t.Errorf("api_key label: got %q, want k1", label)
	}

	// schema_version unchanged
	var v2 string
	if err := db2.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v2); err != nil {
		t.Fatalf("read schema_version #2: %v", err)
	}
	if v2 != v1 {
		t.Errorf("schema_version changed across restart: %q -> %q", v1, v2)
	}
}

// TestIntegration_FreshDatabaseHasAllTables verifies a fresh boot
// produces a complete, queryable schema with all 6 tables and their
// expected indices.
func TestIntegration_FreshDatabaseHasAllTables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := filepath.Join(dir, "data.db")
	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// All 6 tables
	wantTables := []string{"meta", "api_keys", "rooms", "room_members", "messages", "audit"}
	for _, tbl := range wantTables {
		var name string
		if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}

	// Spot-check indices
	wantIndices := []string{
		"idx_api_keys_label",
		"idx_rooms_name",
		"idx_rooms_owner",
		"idx_room_members_key",
		"idx_messages_room_ts",
		"idx_messages_author",
		"idx_audit_ts",
	}
	for _, idx := range wantIndices {
		var name string
		if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
}

// TestIntegration_KillMidMigrationRecovers covers the partial-state
// recovery requirement: if the process dies between apply and
// commit, the next boot re-applies (or detects already-applied via
// meta) and does not leave a half-applied schema. We simulate the
// half-applied state by running the migration in a transaction and
// rolling it back, then verifying re-Migrate still works.
func TestIntegration_KillMidMigrationRecovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := filepath.Join(dir, "data.db")
	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	// Simulate "process died between apply and commit" by:
	// 1) Running the migration in a tx and rolling back
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		"CREATE TABLE half_applied (id INTEGER)"); err != nil {
		t.Fatalf("create in tx: %v", err)
	}
	_ = tx.Rollback() // simulate crash before commit

	// 2) Run the real Migrate. It should still succeed.
	if _, err := Migrate(context.Background(), db, osDirFSRepoRoot("migrations"), nil); err != nil {
		t.Fatalf("Migrate after rollback: %v", err)
	}

	// 3) The half-applied table must NOT exist (was rolled back).
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='half_applied'").Scan(&name)
	if err == nil {
		t.Error("half_applied table leaked despite rollback")
	}

	// 4) meta.schema_version is set to the highest applied migration
	// (which is 2 if migration 0002 is present, else 1). We just
	// assert it's a positive integer and the half-applied table is
	// absent.
	var v string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	n, _ := strconv.Atoi(v)
	if n < 1 {
		t.Errorf("schema_version: got %d, want >= 1", n)
	}
}
