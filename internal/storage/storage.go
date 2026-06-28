// Package storage owns the SQLite database lifecycle for Conclave MCP.
//
// The connection is opened with strict filesystem permissions per
// SPEC-0001 C7 and the privacy-by-default model: the parent directory
// is created with mode 0700 and the database file is opened (and
// verified) with mode 0600. A path-traversal check rejects any
// DBPath that contains a ".." segment or starts with "-".
//
// On successful open, the function runs PRAGMA wal_checkpoint(TRUNCATE)
// per SPEC-0001 §6 Q1 to reclaim WAL space from a previous run, and
// applies the connection PRAGMAs from SPEC-0002 §2.2.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// Pure-Go SQLite driver; no CGO, no C toolchain required.
	_ "modernc.org/sqlite"
)

// Open opens the SQLite database at dbPath, applying the required
// file mode checks, post-open housekeeping, and the SPEC-0002 §2.2
// PRAGMAs. It returns a *sql.DB ready for queries and a close function
// the caller must invoke on shutdown.
//
// The returned close function is idempotent and safe to call after
// errors. The function never panics on filesystem state.
func Open(ctx context.Context, dbPath string) (*sql.DB, func() error, error) {
	if err := validatePath(dbPath); err != nil {
		return nil, noopClose, fmt.Errorf("db path: %w", err)
	}

	dir := filepath.Dir(dbPath)
	// The data directory must end up mode 0o700. We always chmod
	// it ourselves, even if it pre-existed, because the test
	// fixture (and many production setups) hand us a directory
	// whose mode is whatever t.TempDir() or the operator chose.
	// Trusting a pre-existing mode is a security regression:
	// somebody could plant a world-readable Conclave data
	// directory and we would happily use it.
	//
	// The mode check on a *pre-existing parent* like /tmp is the
	// source of the original CI-vs-local drift: on a developer
	// laptop /tmp is 755, on a CI container it is sometimes 1777,
	// and a strict mode check on the parent rejects both even
	// though our data dir underneath it is mode 700. The fix is
	// to chmod the data dir to 0o700 explicitly and to check
	// our own chmod took effect, without ever asking the
	// pre-existing parent for its mode.
	//
	// For paranoia: if CONCLAVE_STRICT_DIR_PERMS=1 is set, also
	// require the parent to be mode 0o700 or stricter.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, noopClose, fmt.Errorf("mkdir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, noopClose, fmt.Errorf("chmod data dir to 0700: %w", err)
	}
	if err := enforceDirMode(dir, 0o700); err != nil {
		return nil, noopClose, err
	}
	if os.Getenv("CONCLAVE_STRICT_DIR_PERMS") == "1" {
		parent := filepath.Dir(dir)
		if err := enforceDirMode(parent, 0o700); err != nil {
			return nil, noopClose, fmt.Errorf("strict mode: %w", err)
		}
	}

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, noopClose, fmt.Errorf("create db file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, noopClose, fmt.Errorf("close after create: %w", err)
		}
	} else if err != nil {
		return nil, noopClose, fmt.Errorf("stat db file: %w", err)
	} else {
		if err := os.Chmod(dbPath, 0o600); err != nil {
			return nil, noopClose, fmt.Errorf("chmod db file: %w", err)
		}
	}
	if err := enforceFileMode(dbPath, 0o600); err != nil {
		return nil, noopClose, err
	}

	// Open with DSN pragmas. modernc.org/sqlite applies each
	// _pragma value as a "PRAGMA <name>(<value>)" statement on
	// EVERY pooled connection at open time. This is the only
	// safe way to set per-connection pragmas like foreign_keys
	// in a Go *sql.DB pool.
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=mmap_size(268435456)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, noopClose, fmt.Errorf("sql.Open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*1e9)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, noopClose, fmt.Errorf("ping: %w", err)
	}

	if err := runWalCheckpointTruncate(ctx, db); err != nil {
		_ = db.Close()
		return nil, noopClose, fmt.Errorf("wal_checkpoint: %w", err)
	}

	return db, db.Close, nil
}

// ApplyPragmas is a no-op retained for API compatibility.
//
// modernc.org/sqlite applies the SPEC-0002 §2.2 pragmas via the DSN
// (see the _pragma query parameters set in Open), so every pooled
// connection gets them on open. This function is preserved so the
// migration runner (task #14) and tests can use a uniform name; it
// returns nil unconditionally.
func ApplyPragmas(ctx context.Context, db *sql.DB) error {
	return nil
}

func runWalCheckpointTruncate(ctx context.Context, db *sql.DB) error {
	row := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	var busy, log, checkpointed int
	if err := row.Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	return nil
}

// ErrPathTraversal is returned when the configured DB path contains
// a ".." segment or starts with "-".
var ErrPathTraversal = errors.New("db path must not contain '..' or start with '-'")

// ErrDirModeTooOpen is returned when the parent directory has a
// permission bit that is broader than the maximum allowed.
type ErrDirModeTooOpen struct {
	Path string
	Mode os.FileMode
}

func (e *ErrDirModeTooOpen) Error() string {
	return fmt.Sprintf("directory %s has mode %o, expected <= %o", e.Path, e.Mode, 0o700)
}

// ErrFileModeTooOpen is the file-equivalent of ErrDirModeTooOpen.
type ErrFileModeTooOpen struct {
	Path string
	Mode os.FileMode
}

func (e *ErrFileModeTooOpen) Error() string {
	return fmt.Sprintf("file %s has mode %o, expected <= %o", e.Path, e.Mode, 0o600)
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "-") {
		return ErrPathTraversal
	}
	// Reject any ".." segment in the raw input. We do NOT rely on
	// filepath.Clean here: it would silently resolve "dir/../bar"
	// to "bar", and we want the caller's intent to be visible.
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part == ".." {
			return ErrPathTraversal
		}
	}
	return nil
}

func enforceDirMode(dir string, max os.FileMode) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	if info.Mode().Perm() & ^max != 0 {
		return &ErrDirModeTooOpen{Path: dir, Mode: info.Mode().Perm()}
	}
	return nil
}

func enforceFileMode(file string, max os.FileMode) error {
	info, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("stat file %s: %w", file, err)
	}
	if info.Mode().Perm() & ^max != 0 {
		return &ErrFileModeTooOpen{Path: file, Mode: info.Mode().Perm()}
	}
	return nil
}

func noopClose() error { return nil }

// mkdirAll0700 is like os.MkdirAll(dir, 0o700) but reports whether
// any directory along the path was actually created. We need this
// signal so the caller can decide whether to enforce the 0o700
// mode (only our own directories) versus trusting a pre-existing
// shared parent like /tmp.
//
// The mode is applied at MkdirAll time, so any directory we do
// create ends up mode 700.
func mkdirAll0700(dir string) (bool, error) {
	if dir == "" {
		return false, fmt.Errorf("empty dir")
	}
	// Walk the path; if any component is missing, MkdirAll
	// creates it. We track whether we created the leaf or any
	// of its parents by stat-ing the leaf first.
	if _, err := os.Stat(dir); err == nil {
		// Pre-existed. MkdirAll is a no-op.
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	return true, nil
}
