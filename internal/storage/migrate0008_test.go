package storage

import (
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
)

// Migration 0008 gives owner/writer members every right, reader
// members read, and rows inserted later without rights none.
func TestMigration0008_RightsFromRoles(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	full := osDirFSRepoRoot("migrations")
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(full, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() < "0008" {
			b, _ := fs.ReadFile(full, e.Name())
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := Migrate(ctx, db, before, slog.Default()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES
		 (1, 'o', 'x', 'p1', '2026-01-01T00:00:00Z'), (2, 'w', 'x', 'p2', '2026-01-01T00:00:00Z'), (3, 'r', 'x', 'p3', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('R', 'r', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES
		 ('R', 1, 'owner', '2026-01-01T00:00:00Z'), ('R', 2, 'writer', '2026-01-01T00:00:00Z'), ('R', 3, 'reader', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, db, full, slog.Default()); err != nil {
		t.Fatal(err)
	}
	const all = "read,claim,question,answer,decision,handoff,status_update,task_assignment,alert,system"
	for key, want := range map[int]string{1: all, 2: all, 3: "read"} {
		var got string
		if err := db.QueryRow(`SELECT rights FROM room_members WHERE room_id = 'R' AND key_id = ?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("key %d: rights %q, want %q", key, got, want)
		}
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (4, 'n', 'x', 'p4', '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at) VALUES ('R', 4, 'reader', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var fresh string
	if err := db.QueryRow(`SELECT rights FROM room_members WHERE key_id = 4`).Scan(&fresh); err != nil || fresh != "" {
		t.Errorf("a row inserted without rights has %q (%v), want ''", fresh, err)
	}
}
