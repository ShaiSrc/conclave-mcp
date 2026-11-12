package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

func TestVacuum_OK(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	// Pre-populate a DB with some data and free pages.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("DELETE FROM t"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Capture stat calls; we just trust the implementation but
	// make sure the file existed before and exists after.
	prev, _ := os.Stat(dbPath)
	prevSize := prev.Size()

	var out, errOut bytes.Buffer
	if err := runVacuum(context.Background(), &out, &errOut, dbPath, dir, false); err != nil {
		t.Fatalf("runVacuum: %v", err)
	}
	if !strings.Contains(out.String(), "vacuum complete") {
		t.Errorf("out missing 'vacuum complete': %s", out.String())
	}
	post, _ := os.Stat(dbPath)
	if post.Size() >= prevSize {
		t.Logf("warning: post-vacuum size %d >= pre %d (data may have been small enough to not benefit)", post.Size(), prevSize)
	}

	// Audit log should exist and have one entry.
	logBytes, err := os.ReadFile(filepath.Join(dir, "admin.log"))
	if err != nil {
		t.Fatalf("admin.log missing: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(logBytes, &entry); err != nil {
		t.Fatalf("admin.log not valid JSON line: %v\nraw: %s", err, logBytes)
	}
	if entry["action"] != "admin.vacuum" {
		t.Errorf("action=%v", entry["action"])
	}
}

func TestVacuum_MissingDBIsExit1(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	err := runVacuum(context.Background(), &out, &errOut, filepath.Join(dir, "nope.db"), dir, false)
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestVacuum_LowDiskAbortsWithoutForce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE t (x)")
	db.Close()

	// Stat the file to get its real size.
	info, _ := os.Stat(dbPath)
	realSize := info.Size()

	// Swap deps: pretend free disk is half the DB size.
	oldDisk := vacuumDeps.DiskFree
	vacuumDeps.DiskFree = func(_ string) (int64, error) {
		return realSize / 2, nil
	}
	defer func() { vacuumDeps.DiskFree = oldDisk }()

	var out, errOut bytes.Buffer
	err := runVacuum(context.Background(), &out, &errOut, dbPath, dir, false)
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
	if !strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("missing WARNING: %s", errOut.String())
	}
}

func TestVacuum_LowDiskForceOverrides(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE t (x)")
	db.Close()

	info, _ := os.Stat(dbPath)
	realSize := info.Size()

	oldDisk := vacuumDeps.DiskFree
	vacuumDeps.DiskFree = func(_ string) (int64, error) { return realSize / 2, nil }
	defer func() { vacuumDeps.DiskFree = oldDisk }()

	var out, errOut bytes.Buffer
	if err := runVacuum(context.Background(), &out, &errOut, dbPath, dir, true); err != nil {
		t.Errorf("with --force: %v", err)
	}
	if !strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("missing WARNING: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "--force") {
		t.Errorf("missing --force note: %s", errOut.String())
	}
}

func TestVacuum_NowIsUsed(t *testing.T) {
	// Ensure the timestamp in the audit log uses vacuumDeps.Now.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE t (x)")
	db.Close()

	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldNow := vacuumDeps.Now
	vacuumDeps.Now = func() time.Time { return fixed }
	defer func() { vacuumDeps.Now = oldNow }()

	var out, errOut bytes.Buffer
	if err := runVacuum(context.Background(), &out, &errOut, dbPath, dir, false); err != nil {
		t.Fatal(err)
	}
	logBytes, _ := os.ReadFile(filepath.Join(dir, "admin.log"))
	var entry map[string]any
	_ = json.Unmarshal(logBytes, &entry)
	if entry["ts"] != "2026-01-02T03:04:05Z" {
		t.Errorf("ts=%v", entry["ts"])
	}
}

func TestExitCode(t *testing.T) {
	if ec := ExitCode(nil); ec != 0 {
		t.Errorf("nil=%d", ec)
	}
	if ec := ExitCode(errors.New("plain")); ec != 2 {
		t.Errorf("plain=%d", ec)
	}
	if ec := ExitCode(&exitError{Code: 3, Err: errors.New("x")}); ec != 3 {
		t.Errorf("typed=%d", ec)
	}
}

func TestUIReset(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, closeDB, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys (label, hash, prefix, created_at, is_admin) VALUES ('operator', 'x', 'p1', '2026-11-12T10:00:00Z', 1)`,
		`INSERT INTO meta (key, value) VALUES ('ui_operator_key_id', (SELECT CAST(id AS TEXT) FROM api_keys WHERE prefix = 'p1'))`,
		`INSERT INTO ui_setup_tokens (token_hash, expires_at) VALUES ('t', '2026-11-12T10:10:00Z')`,
		`INSERT INTO ui_credentials (id, public_key, created_at) VALUES ('c1', x'01', '2026-11-12T10:00:00Z'), ('c2', x'02', '2026-11-12T10:00:00Z')`,
		`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES ('h', '2026-11-12T10:00:00Z', '2026-11-12T10:00:00Z', '2026-11-12T22:00:00Z')`,
		// A label made beforehand must not block the rename.
		`INSERT INTO api_keys (label, hash, prefix, created_at) VALUES ('operator-revoked-1', 'y', 'p2', '2026-11-12T10:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = closeDB()

	var errOut bytes.Buffer
	if err := runUIReset(ctx, &errOut, dbPath); err != nil {
		t.Fatalf("runUIReset: %v", err)
	}
	if !strings.Contains(errOut.String(), "removed 2 passkey(s) and 1 session(s)") {
		t.Errorf("stderr: %q", errOut.String())
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var creds, sessions, tokens, metaRows, free int
	var label string
	var revokedAt sql.NullString
	var detail string
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM ui_credentials), (SELECT COUNT(*) FROM ui_sessions), (SELECT COUNT(*) FROM ui_setup_tokens),
		(SELECT COUNT(*) FROM meta WHERE key = 'ui_operator_key_id'), (SELECT COUNT(*) FROM api_keys WHERE label = 'operator'),
		(SELECT label FROM api_keys WHERE prefix = 'p1'), (SELECT revoked_at FROM api_keys WHERE prefix = 'p1')`).
		Scan(&creds, &sessions, &tokens, &metaRows, &free, &label, &revokedAt); err != nil {
		t.Fatal(err)
	}
	if creds != 0 || sessions != 0 || tokens != 0 || metaRows != 0 || free != 0 || !regexp.MustCompile(`^operator-revoked-1-[0-9a-f]{8}$`).MatchString(label) || !revokedAt.Valid {
		t.Errorf("after reset: %d passkeys, %d sessions, %d tokens, %d meta rows, %d operator labels, label %q, revoked %v", creds, sessions, tokens, metaRows, free, label, revokedAt)
	}
	if err := db.QueryRow(`SELECT detail FROM audit WHERE action = 'ui.reset' AND actor_key_id IS NULL`).Scan(&detail); err != nil || detail != `{"operator_key_revoked":true,"passkeys":2,"sessions":1}` {
		t.Errorf("audit detail %q (%v)", detail, err)
	}
}

// The documented way out for a key labelled operator that the UI did not
// create: ui-reset leaves it alone while active; once revoked (keys
// revoke), ui-reset renames it and the label is free for setup.
func TestUIReset_ForeignOperatorKeyAfterRevoke(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, closeDB, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (label, hash, prefix, created_at, is_admin) VALUES ('operator', 'x', 'hand', '2026-11-12T10:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = closeDB()
	label := func() (string, bool) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var l string
		var revoked sql.NullString
		if err := db.QueryRow(`SELECT label, revoked_at FROM api_keys WHERE prefix = 'hand'`).Scan(&l, &revoked); err != nil {
			t.Fatal(err)
		}
		return l, revoked.Valid
	}

	if err := runUIReset(ctx, &bytes.Buffer{}, dbPath); err != nil {
		t.Fatalf("runUIReset: %v", err)
	}
	if l, revoked := label(); l != "operator" || revoked {
		t.Fatalf("active foreign key after reset: label %q, revoked %v; want it left alone", l, revoked)
	}

	db, err = sql.Open("sqlite", dbPath) // what keys revoke does on the server
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET revoked_at = '2026-11-12T11:00:00Z' WHERE prefix = 'hand'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := runUIReset(ctx, &bytes.Buffer{}, dbPath); err != nil {
		t.Fatalf("runUIReset after revoke: %v", err)
	}
	if l, _ := label(); !strings.HasPrefix(l, "operator-revoked-") {
		t.Errorf("revoked foreign key after reset: label %q, want operator-revoked-...", l)
	}
}

func TestUIReset_MissingDBIsExit1(t *testing.T) {
	err := runUIReset(context.Background(), &bytes.Buffer{}, filepath.Join(t.TempDir(), "missing.db"))
	if ExitCode(err) != 1 {
		t.Errorf("missing db: %v (exit %d), want exit 1", err, ExitCode(err))
	}
}
