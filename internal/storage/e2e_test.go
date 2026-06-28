package storage

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpen_EndToEndFilesystemModes is the e2e counterpart to the
// unit tests: it opens a real DB on disk and asserts the OS-level
// mode bits, owner, and DB query path. Run via `go test ./internal/storage`.
func TestOpen_EndToEndFilesystemModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	dbPath := filepath.Join(dir, "sub", "data.db")

	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	// Parent dir must be 0700
	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode: got %o, want 0700", got)
	}

	// DB file must be 0600
	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode: got %o, want 0600", got)
	}

	// File must be owned by the current user (no privilege giveaway)
	if stat, ok := fileInfo.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != uint32(os.Getuid()) {
			t.Errorf("file uid: got %d, want %d", stat.Uid, os.Getuid())
		}
	}

	// Connection must work: SELECT 1
	var v int
	if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Errorf("SELECT 1: %v", err)
	}
	if v != 1 {
		t.Errorf("SELECT 1 returned %d", v)
	}

	// WAL journal mode is active per the DSN we pass.
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Errorf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: got %q, want wal", mode)
	}
}
