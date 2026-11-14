package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

// runRoot runs the root command with args and returns stdout and the error.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// A backup taken while another connection writes opens and holds the
// rows; it is written with mode 0600; an existing file is refused (exit
// 2) and left alone; a missing database is an error (exit 1) and leaves
// no file behind.
func TestBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "data.db")
	db, closeDB, err := storage.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES ('backup_test', '0')`); err != nil {
		t.Fatal(err)
	}
	// The server keeps writing while the backup runs.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Exec(`UPDATE meta SET value = ? WHERE key = 'backup_test'`, i); err != nil {
				t.Errorf("writer: %v", err)
				return
			}
		}
	}()
	dest := filepath.Join(dir, "backup.db")
	out, err := runRoot(t, "backup", "--db", dbPath, dest)
	close(stop)
	wg.Wait()
	if err != nil || !strings.Contains(out, dest) || !strings.Contains(out, "bytes") {
		t.Fatalf("backup: err=%v out=%q", err, out)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 { // data/ and backup.db, no journal left over
		t.Errorf("files next to the backup: %v", entries)
	}
	copyDB, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var schema, keys int
	if err := copyDB.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&schema); err != nil || schema == 0 {
		t.Errorf("copy schema_version: %d %v", schema, err)
	}
	if err := copyDB.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = 'backup_test'`).Scan(&keys); err != nil || keys != 1 {
		t.Errorf("copy rows: %d %v", keys, err)
	}

	before, _ := os.ReadFile(dest)
	_, err = runRoot(t, "backup", "--db", dbPath, dest)
	if exitClassifier(err) != 2 || !strings.Contains(err.Error(), "exists") {
		t.Errorf("existing file: err=%v code=%d", err, exitClassifier(err))
	}
	if after, _ := os.ReadFile(dest); !bytes.Equal(before, after) {
		t.Error("existing file was changed")
	}

	// With the server stopped too (no -wal or -shm file left).
	if err := closeDB(); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "backup", "--db", dbPath, filepath.Join(dir, "stopped.db")); err != nil {
		t.Errorf("backup of a stopped server's database: %v %s", err, out)
	}

	missing := filepath.Join(dir, "missing.db")
	_, err = runRoot(t, "backup", "--db", filepath.Join(dir, "nope", "data.db"), missing)
	if exitClassifier(err) != 1 {
		t.Errorf("missing database: err=%v code=%d", err, exitClassifier(err))
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("missing database left a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(err) {
		t.Errorf("missing database made its directory: %v", err)
	}
}

// server prints GET /v1/server as text, or as JSON with --format json;
// a refusal is exit 1, an unreachable server exit 3.
func TestServerInfo(t *testing.T) {
	status, body := http.StatusOK, `{"version":"0.14.0","schema_version":16,"started_at":"2026-11-14T10:00:00Z",`+
		`"federation":{"enabled":true,"domain":"a.example","mode":"allowlist","protocol":"conclave-federation/1"}}`
	var req string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req = r.Method + " " + r.URL.RequestURI()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	flags := []string{"--server", srv.URL, "--token", "stub"}

	out, err := runRoot(t, append([]string{"server"}, flags...)...)
	if err != nil || req != "GET /v1/server" {
		t.Fatalf("server: err=%v req=%q", err, req)
	}
	for _, want := range []string{"version:         0.14.0\n", "schema version:  16\n", "started:         2026-11-14T10:00:00Z\n",
		"federation:      on, domain a.example, mode allowlist\n", "protocol:        conclave-federation/1\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
	out, err = runRoot(t, append([]string{"server", "--format", "json"}, flags...)...)
	if err != nil || !strings.Contains(out, `"schema_version": 16`) || !strings.Contains(out, `"protocol": "conclave-federation/1"`) {
		t.Errorf("json: err=%v out=%s", err, out)
	}
	body = `{"version":"0.14.0","schema_version":16,"started_at":"t","federation":{"enabled":false,"domain":"","mode":"","protocol":"conclave-federation/1"}}`
	if out, _ = runRoot(t, append([]string{"server"}, flags...)...); !strings.Contains(out, "federation:      off\n") {
		t.Errorf("federation off:\n%s", out)
	}
	if _, err = runRoot(t, append([]string{"server", "--format", "yaml"}, flags...)...); exitClassifier(err) != 2 {
		t.Errorf("bad format: err=%v", err)
	}
	status, body = http.StatusForbidden, `{"error":{"code":"forbidden","message":"admin key required"}}`
	if _, err = runRoot(t, append([]string{"server"}, flags...)...); exitClassifier(err) != 1 || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("403: err=%v code=%d", err, exitClassifier(err))
	}
	srv.Close()
	if _, err = runRoot(t, append([]string{"server"}, flags...)...); exitClassifier(err) != 3 {
		t.Errorf("unreachable: err=%v code=%d", err, exitClassifier(err))
	}
}
