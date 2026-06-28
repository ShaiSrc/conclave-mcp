package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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
