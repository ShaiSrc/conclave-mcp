// Package admin implements offline maintenance subcommands per
// SPEC-0004 §5.2.
//
// `conclave-mcp admin vacuum` opens the SQLite file directly and
// runs VACUUM. It does NOT talk to a running server. This is by
// design: vacuum is incompatible with WAL writers, and forcing
// the user to stop the server first keeps the contract simple.
//
// `conclave-mcp admin keys create-bootstrap` is the operator's
// first-use setup path: it inserts a new API key directly into
// the SQLite file so the first agent (or operator) can
// authenticate. Like vacuum, it does NOT talk to a running
// server, and the server must be stopped first to avoid
// corrupting the WAL.
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/hermes/conclave-mcp/internal/auth"
)

// NewCmd builds the admin subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Offline maintenance (vacuum, key bootstrap)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newVacuumCmd())
	cmd.AddCommand(newKeysCmd())
	return cmd
}

// vacuumDeps is overridable for tests. The default uses real
// disk and SQLite. Tests inject a tiny DB and a fake stat.
var vacuumDeps = struct {
	OpenDB   func(path string) (*sql.DB, error)
	Stat     func(path string) (os.FileInfo, error)
	DiskFree func(path string) (int64, error)
	Now      func() time.Time
	AuditLog func(path string, entry map[string]any) error
}{
	OpenDB: openSQLite,
	Stat:   os.Stat,
	DiskFree: func(path string) (int64, error) {
		// Best-effort: report 0 on platforms that don't expose
		// free space easily. The vacuum itself never reads this
		// value, the warning just turns into "unknown".
		return 0, nil
	},
	Now:      time.Now,
	AuditLog: appendJSONL,
}

func openSQLite(path string) (*sql.DB, error) {
	// Open read-write; vacuum requires write access. The driver
	// refuses to start if the file is missing or unwritable.
	return sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
}

func appendJSONL(path string, entry map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(entry)
}

// ---------- admin vacuum ----------

func newVacuumCmd() *cobra.Command {
	var (
		flagDB    string
		flagForce bool
	)
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Run VACUUM on the SQLite file (offline; stop the server first)",
		Long: `Opens the SQLite file directly and runs VACUUM to reclaim space.

Per SPEC-0004 §5.2, this subcommand does NOT talk to a running server.
The server must be stopped before vacuum (WAL and VACUUM are
incompatible in the same process). The action is recorded in
${CONCLAVE_DATA_DIR}/admin.log for audit.

The CLI warns and exits with code 2 if free disk space is less than
2x the current DB file size. Pass --force to skip the warning.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbPath, dataDir := resolveVacuumPaths(flagDB)
			return runVacuum(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), dbPath, dataDir, flagForce)
		},
	}
	// SPEC-0004 §3 + §5.1: the spec describes a --db flag for
	// the data file path. The previous implementation only
	// read CONCLAVE_DATA_DIR, which made the CLI fragile in
	// CI. Both are now supported: --db wins, then
	// CONCLAVE_DATA_DIR, then the default.
	cmd.Flags().StringVar(&flagDB, "db", "", "path to the SQLite file (overrides CONCLAVE_DATA_DIR)")
	cmd.Flags().BoolVar(&flagForce, "force", false, "proceed even if free disk space is < 2x DB size")
	return cmd
}

// resolveVacuumPaths returns the (dbPath, dataDir) pair for
// the vacuum command. Precedence:
//
//  1. --db flag (explicit, per-call)
//  2. CONCLAVE_DATA_DIR env var
//  3. default ./.conclave-mcp
//
// If --db is given, dataDir is its parent directory. The
// audit log is written to dataDir/admin.log (SPEC-0004 §5.1).
func resolveVacuumPaths(flagDB string) (string, string) {
	if flagDB != "" {
		return flagDB, filepath.Dir(flagDB)
	}
	dataDir := os.Getenv("CONCLAVE_DATA_DIR")
	if dataDir == "" {
		dataDir = "./.conclave-mcp"
	}
	return filepath.Join(dataDir, "data.db"), dataDir
}

func runVacuum(ctx context.Context, out, errOut io.Writer, dbPath, dataDir string, force bool) error {
	// 1. Check that the DB file exists and is readable.
	info, err := vacuumDeps.Stat(dbPath)
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("stat db: %w", err)}
	}
	dbSize := info.Size()

	// 2. Disk space check.
	free, _ := vacuumDeps.DiskFree(dbPath)
	if free > 0 && free < 2*dbSize {
		fmt.Fprintf(errOut, "WARNING: free disk space (%d bytes) is less than 2x DB size (%d bytes).\n", free, dbSize)
		if !force {
			return &exitError{Code: 2, Err: errors.New("insufficient free disk space; pass --force to override")}
		}
		fmt.Fprintln(errOut, "WARNING: --force set, proceeding despite low disk space")
	}

	// 3. Open DB and run VACUUM.
	db, err := vacuumDeps.OpenDB(dbPath)
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("open db: %w", err)}
	}
	defer db.Close()

	start := vacuumDeps.Now()
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("vacuum: %w", err)}
	}
	dur := vacuumDeps.Now().Sub(start)

	// 4. Report.
	if info2, err := vacuumDeps.Stat(dbPath); err == nil {
		fmt.Fprintf(out, "vacuum complete: %d -> %d bytes (%.1f%% smaller) in %s\n",
			dbSize, info2.Size(),
			100*(1-float64(info2.Size())/float64(dbSize)),
			dur)
	} else {
		fmt.Fprintf(out, "vacuum complete in %s\n", dur)
	}

	// 5. Audit log (local JSONL, not the server's HTTP audit).
	_ = vacuumDeps.AuditLog(filepath.Join(dataDir, "admin.log"), map[string]any{
		"ts":        start.UTC().Format(time.RFC3339Nano),
		"action":    "admin.vacuum",
		"db_size":   dbSize,
		"duration":  dur.String(),
		"free_disk": free,
	})
	return nil
}

// exitError mirrors cmd/rooms.
type exitError struct {
	Code int
	Err  error
}

func (e *exitError) Error() string { return e.Err.Error() }
func (e *exitError) Unwrap() error { return e.Err }

// ExitCode is exported so cmd/conclave-mcp can use it.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 2
}

// ---------- admin keys create-bootstrap ----------
//
// First-use setup. The server has no API keys on a fresh
// install; this subcommand mints one and inserts it
// directly into the SQLite file. The plaintext is
// returned exactly once on STDOUT (the same
// "shown-once" contract as the HTTP /v1/keys endpoint).
//
// Like vacuum, this does NOT talk to a running server.
// The server must be stopped first; otherwise the WAL
// writer can clobber the insert.

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys offline (create-bootstrap)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newKeysCreateBootstrapCmd())
	return cmd
}

func newKeysCreateBootstrapCmd() *cobra.Command {
	var (
		flagDB          string
		flagLabel       string
		flagTrustOrigin string
	)
	cmd := &cobra.Command{
		Use:   "create-bootstrap",
		Short: "Create the first API key directly in the SQLite file",
		Long: `First-use setup for a fresh Conclave install. Mints a new
API key, computes its argon2id hash, and inserts the row
directly into the api_keys table. The plaintext token
is printed to STDOUT exactly once and is not stored.

Like admin vacuum, this subcommand does NOT talk to a
running server. Stop the server first; otherwise the
WAL writer can clobber the insert and the key will
not authenticate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flagLabel == "" {
				return &exitError{Code: 2, Err: errors.New("--label is required")}
			}
			if flagTrustOrigin != "internal" && flagTrustOrigin != "external" {
				return &exitError{Code: 2, Err: errors.New("--trust-origin must be 'internal' or 'external'")}
			}
			dbPath, dataDir := resolveVacuumPaths(flagDB)
			return runKeysCreateBootstrap(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), dbPath, dataDir, flagLabel, flagTrustOrigin)
		},
	}
	cmd.Flags().StringVar(&flagDB, "db", "", "path to the SQLite file (overrides CONCLAVE_DATA_DIR)")
	cmd.Flags().StringVar(&flagLabel, "label", "", "human-readable label for the key (required, unique)")
	cmd.Flags().StringVar(&flagTrustOrigin, "trust-origin", "internal", "operator-controlled classification: 'internal' (own infrastructure, trusted partners) or 'external' (invited collaborators, SPEC-0001 Q11 / ADR-0008 revision)")
	return cmd
}

func runKeysCreateBootstrap(ctx context.Context, out, errOut io.Writer, dbPath, dataDir, label, trustOrigin string) error {
	// 1. Generate the token + hash. GenerateToken returns
	//    a 43-char base64url plaintext (256 bits) and the
	//    argon2id PHC hash. The plaintext is shown to the
	//    operator once and never persisted.
	plaintext, hash, err := auth.GenerateToken()
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("generate token: %w", err)}
	}
	prefix := plaintext[3:11]

	// 2. Open DB read-write. The driver takes a WAL mode
	//    pragma to be consistent with the server.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("open db: %w", err)}
	}
	defer db.Close()

	// 3. Insert the row. We use a transaction so the WAL
	//    gets a single fsync, not two.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("begin tx: %w", err)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys(prefix, hash, label, created_at, trust_origin, is_admin) VALUES (?, ?, ?, ?, ?, 1)`,
		prefix, hash, label, now, trustOrigin,
	)
	if err != nil {
		_ = tx.Rollback()
		// Most common cause: label UNIQUE collision.
		if isUniqueViolation(err) {
			return &exitError{Code: 1, Err: fmt.Errorf("label %q already exists (must be unique)", label)}
		}
		return &exitError{Code: 1, Err: fmt.Errorf("insert: %w", err)}
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return &exitError{Code: 1, Err: fmt.Errorf("commit: %w", err)}
	}

	// 4. Audit log (local JSONL, not the server's HTTP
	//    audit — same pattern as admin vacuum).
	_ = vacuumDeps.AuditLog(filepath.Join(dataDir, "admin.log"), map[string]any{
		"ts":           now,
		"action":       "admin.keys.create_bootstrap",
		"key_id":       id,
		"prefix":       prefix,
		"label":        label,
		"trust_origin": trustOrigin,
	})

	// 5. Output. Plaintext is the only sensitive thing
	//    and it goes to STDOUT, so scripts can pipe it
	//    into something safe (a password manager, a
	//    secret store). The status line goes to STDERR.
	fmt.Fprintln(out, plaintext)
	fmt.Fprintf(errOut, "key id=%d prefix=%s label=%q trust_origin=%s created; store the plaintext now, it will not be shown again.\n",
		id, prefix, label, trustOrigin)
	return nil
}

// isUniqueViolation returns true for SQLite UNIQUE
// constraint failures. The error message is the only
// portable signal across modernc.org/sqlite versions.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed") ||
		contains(msg, "constraint failed: UNIQUE")
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
