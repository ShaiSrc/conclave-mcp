package storage

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
)

// newTestDB opens a real on-disk DB in t.TempDir(). Migration tests
// need a real DB to exercise the actual SQLite transaction semantics.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	if err := chmodForTesting(t, dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	db, closeFn, err := Open(context.Background(), dir+"/data.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return db
}

func TestMigrate_AppliesAllOnFreshDB(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"0001_initial.sql": &fstest.MapFile{
			Data: []byte(`
				CREATE TABLE meta (
					key TEXT PRIMARY KEY,
					value TEXT NOT NULL
				);
			`),
		},
	}

	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) < 1 || applied[0] != 1 {
		t.Errorf("applied: got %v, want first element 1", applied)
	}

	// meta row recorded
	var v string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v); err != nil {
		t.Fatalf("query meta: %v", err)
	}
	if v != "1" {
		t.Errorf("schema_version: got %q, want 1", v)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"0001_initial.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		`)},
	}

	if _, err := Migrate(context.Background(), db, filesys, nil); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second call should be no-op, got applied=%v", applied)
	}
}

func TestMigrate_AppliesOnlyNew(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"0001_initial.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		`)},
		"0002_add_x.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE x (id INTEGER);
		`)},
	}

	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(applied) != 2 {
		t.Errorf("first applied: got %v, want [1 2]", applied)
	}

	// Now drop table x and re-run; should be a no-op
	if _, err := db.Exec("DROP TABLE x"); err != nil {
		t.Fatalf("drop x: %v", err)
	}
	applied, err = Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second should be no-op, got %v", applied)
	}

	// And table x is still gone (we did NOT re-apply)
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='x'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("table x should be gone, got err=%v", err)
	}
}

func TestMigrate_LexOrder(t *testing.T) {
	db := newTestDB(t)
	// Add 10 in the middle to verify lex (not numeric) order is NOT used.
	// 0001, 0002, 0010, 0003 should apply in numeric order: 1, 2, 3, 10.
	filesys := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			INSERT INTO meta(key, value) VALUES('1', 'ok');
		`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`
			INSERT INTO meta(key, value) VALUES('2', 'ok');
		`)},
		"0010_j.sql": &fstest.MapFile{Data: []byte(`
			INSERT INTO meta(key, value) VALUES('10', 'ok');
		`)},
		"0003_c.sql": &fstest.MapFile{Data: []byte(`
			INSERT INTO meta(key, value) VALUES('3', 'ok');
		`)},
	}

	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := []int{1, 2, 3, 10}
	if len(applied) != len(want) {
		t.Fatalf("applied: got %v, want %v", applied, want)
	}
	for i, v := range want {
		if applied[i] != v {
			t.Errorf("applied[%d]: got %d, want %d", i, applied[i], v)
		}
	}
}

func TestMigrate_RejectsBadFilename(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"not-a-version.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	if _, err := Migrate(context.Background(), db, filesys, nil); err == nil {
		t.Error("expected error for bad migration filename, got nil")
	}
}

func TestMigrate_RejectsFailedSQL(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"0001_bad.sql": &fstest.MapFile{Data: []byte(`THIS IS NOT VALID SQL;`)},
	}
	_, err := Migrate(context.Background(), db, filesys, nil)
	if err == nil {
		t.Error("expected error for invalid SQL, got nil")
	}
	// meta should not have been written
	var v string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v); err == nil {
		t.Errorf("schema_version should not be set after failed migration, got %q", v)
	}
}

func TestMigrate_AppliesReal0001(t *testing.T) {
	// Use the actual 0001_initial.sql from disk. Path is relative
	// to the test's working directory, which `go test` sets to the
	// package directory; we walk up to the repo root.
	db := newTestDB(t)
	dirfs := osDirFSRepoRoot("migrations")

	applied, err := Migrate(context.Background(), db, dirfs, slog.Default())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) < 1 || applied[0] != 1 {
		t.Errorf("applied: got %v, want first element 1", applied)
	}

	// All 6 tables should exist
	wantTables := []string{"meta", "api_keys", "rooms", "room_members", "messages", "audit"}
	for _, tbl := range wantTables {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", tbl, err)
		}
		if name != tbl {
			t.Errorf("table %s: got name %q", tbl, name)
		}
	}

	// FK from messages to rooms should be enforced.
	if _, err := db.Exec("INSERT INTO messages (room_id, author_key_id, ts, content, content_type, message_type) VALUES ('nope', 999, '2026-01-01T00:00:00.000Z', 'x', 'text/plain', 'system')"); err == nil {
		t.Error("expected FK violation on messages insert with bad room_id, got nil")
	}
}

func TestMigrate_Real0001_OwnerTriggerFires(t *testing.T) {
	db := newTestDB(t)
	dirfs := osDirFSRepoRoot("migrations")
	if _, err := Migrate(context.Background(), db, dirfs, slog.Default()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert an api_key, a room owned by it, the owner in room_members.
	if _, err := db.Exec(`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (1, 'owner', 'phc:fake', 'testpfx2', '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('01JABC', 'r1', 1, '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES ('01JABC', 1, 'owner', '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("insert owner member: %v", err)
	}
	// Now try to remove the only owner. The trigger should fire.
	_, err := db.Exec(`DELETE FROM room_members WHERE room_id='01JABC' AND key_id=1`)
	if err == nil {
		t.Error("expected trigger to prevent removal of last owner, got nil")
	}
}

func TestMigrate_EmptyDirIsNoop(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{}
	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("Migrate empty: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("expected no applied, got %v", applied)
	}
}

func TestMigrate_IgnoresNonSQL(t *testing.T) {
	db := newTestDB(t)
	filesys := fstest.MapFS{
		"0001_a.sql":   &fstest.MapFile{Data: []byte(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)},
		"README.md":    &fstest.MapFile{Data: []byte(`# not a migration`)},
		"_helpers.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	applied, err := Migrate(context.Background(), db, filesys, nil)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 applied, got %v", applied)
	}
}

// (intentionally no helper; tests use os.DirFS directly)
// osDirFSRepoRoot is a test-only convenience that locates the repo
// root from the test's source file location. The function uses
// runtime.Caller(0) to get the path of migrate_test.go, then walks
// up to the directory two levels above (internal/storage -> repo root).
// This is robust to "go test" being run from any working directory.
func osDirFSRepoRoot(subpath string) fs.FS {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("osDirFSRepoRoot: runtime.Caller failed")
	}
	// thisFile is something like
	// /workspace/conclave-mcp/internal/storage/migrate_test.go
	// We want /workspace/conclave-mcp/migrations. So go up two dirs.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return os.DirFS(filepath.Join(repoRoot, subpath))
}
