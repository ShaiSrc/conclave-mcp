package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestOpen_AppliesAllSpecPragmas verifies that after Open(), the
// connection has the SPEC-0002 §2.2 PRAGMAs set as defined.
func TestOpen_AppliesAllSpecPragmas(t *testing.T) {
	dir := t.TempDir()
	if err := chmodForTesting(t, dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := dir + string(filepath.Separator) + "data.db"

	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	// journal_mode is database-level; reading from any connection
	// is correct.
	assertPragmaText(t, db, "journal_mode", "wal")
	// synchronous=NORMAL == 1
	assertPragmaText(t, db, "synchronous", "1")
	// foreign_keys=ON == 1
	assertPragmaText(t, db, "foreign_keys", "1")
	// busy_timeout is 5 s
	assertPragmaInt(t, db, "busy_timeout", 5000)
	// temp_store=MEMORY == 2
	assertPragmaText(t, db, "temp_store", "2")
	// mmap_size = 256 MiB
	assertPragmaInt(t, db, "mmap_size", 268435456)
}

// TestOpen_ForeignKeysEnforcedAcrossConnections verifies that the
// per-connection foreign_keys pragma is set on EVERY pooled
// connection, not just the one that handled the Open ping. This is
// the bug that motivated the DSN-_pragma approach.
func TestOpen_ForeignKeysEnforcedAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	if err := chmodForTesting(t, dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := dir + string(filepath.Separator) + "fk.db"

	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	// Create a parent and child table where child references parent.
	if _, err := db.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE child (parent_id INTEGER REFERENCES parent(id))`); err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Force multiple pool connections by limiting max and acquiring
	// several in parallel.
	db.SetMaxOpenConns(2)
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := db.Exec("INSERT INTO child (parent_id) VALUES (999)")
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		err := <-done
		if err == nil {
			t.Errorf("expected FK violation, got nil")
		}
	}
}

func assertPragmaText(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s: got %q, want %q", name, got, want)
	}
}

func assertPragmaInt(t *testing.T, db *sql.DB, name string, want int) {
	t.Helper()
	var gotStr string
	if err := db.QueryRow("PRAGMA " + name).Scan(&gotStr); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	got, err := strconv.Atoi(gotStr)
	if err != nil {
		t.Fatalf("PRAGMA %s: not an int: %q", name, gotStr)
	}
	if got != want {
		t.Errorf("PRAGMA %s: got %d, want %d", name, got, want)
	}
}

func chmodForTesting(t *testing.T, path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}
