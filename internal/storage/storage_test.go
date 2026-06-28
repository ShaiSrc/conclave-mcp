package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_CreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "data.db")

	db, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	if db == nil {
		t.Fatal("db is nil")
	}

	// Parent dir must exist with mode 0700
	info, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("parent is not a directory")
	}
	if got := info.Mode().Perm(); got > 0o700 {
		t.Errorf("dir mode: got %o, want <= 0700", got)
	}

	// DB file must exist with mode 0600
	info, err = os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode: got %o, want 0600", got)
	}
}

func TestOpen_ReusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")

	db1, close1, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Insert something to prove data persists.
	if _, err := db1.Exec("CREATE TABLE x (id INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := close1(); err != nil {
		t.Fatalf("close1: %v", err)
	}

	db2, close2, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = close2() })

	// Data should be there.
	var name string
	if err := db2.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='x'").Scan(&name); err != nil {
		t.Fatalf("query master: %v", err)
	}
	if name != "x" {
		t.Errorf("table x missing after reopen")
	}
}

func TestOpen_TightensExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")

	// Pre-create with a too-open mode.
	if err := os.WriteFile(dbPath, []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode after Open: got %o, want 0600", got)
	}
}

func TestOpen_RejectsPathTraversal(t *testing.T) {
	// All test paths live under t.TempDir() so we can satisfy the
	// "parent dir must be 0700" check. The point of these tests is
	// the path-traversal rejection, not the dir mode.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"dotdot", "../etc/passwd"},
		// Construct manually because filepath.Join would resolve
		// the ".." segment away before our validatePath sees it.
		{"embedded", dir + string(filepath.Separator) + "foo" + string(filepath.Separator) + ".." + string(filepath.Separator) + "bar.db"},
		{"leading-dash", "-leading-dash.db"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Open(context.Background(), tt.path)
			if err == nil {
				t.Fatalf("Open(%q): expected error, got nil", tt.path)
			}
			if tt.path == "" {
				if !strings.Contains(err.Error(), "empty path") {
					t.Errorf("empty path error: got %v, want 'empty path'", err)
				}
				return
			}
			if !errors.Is(err, ErrPathTraversal) {
				t.Errorf("Open(%q): got %v, want ErrPathTraversal", tt.path, err)
			}
		})
	}
}

func TestOpen_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	_, closeFn, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Errorf("first close: %v", err)
	}
	// Closing a second time should not panic. We don't assert on
	// the error because the stdlib sql.DB may return ErrConnDone
	// or nil depending on the driver; the contract is "no panic".
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("second close panicked: %v", r)
		}
	}()
	_ = closeFn()
}

func TestOpen_RejectsNonDirectoryParent(t *testing.T) {
	dir := t.TempDir()
	// Create a regular file at the parent path.
	parent := filepath.Join(dir, "iam-a-file")
	if err := os.WriteFile(parent, []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, closeFn, err := Open(context.Background(), filepath.Join(parent, "data.db"))
	if err == nil {
		t.Fatal("Open: expected error when parent is a file, got nil")
	}
	_ = closeFn // safe even on error
}

func TestEnforceDirMode_RejectsTooOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Skipf("can't chmod (maybe rootless): %v", err)
	}
	err := enforceDirMode(dir, 0o700)
	var bad *ErrDirModeTooOpen
	if !errors.As(err, &bad) {
		t.Fatalf("got %v, want ErrDirModeTooOpen", err)
	}
}

func TestEnforceFileMode_AcceptsExact(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := enforceFileMode(f, 0o600); err != nil {
		t.Errorf("enforceFileMode(0600): %v", err)
	}
}
