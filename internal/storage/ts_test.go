package storage

import (
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestFormatTS_SortsChronologically(t *testing.T) {
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	// RFC3339Nano trims trailing zeros, so ".1Z" sorted after ".12Z"
	// and a whole second after its own fractions.
	steps := []time.Duration{0, 100 * time.Millisecond, 120 * time.Millisecond,
		123450 * time.Microsecond, 123451 * time.Microsecond, time.Second}
	for i := 1; i < len(steps); i++ {
		a, b := FormatTS(base.Add(steps[i-1])), FormatTS(base.Add(steps[i]))
		if a >= b {
			t.Errorf("FormatTS out of order: %s >= %s", a, b)
		}
	}
}

func TestNormalizeTS(t *testing.T) {
	got, err := NormalizeTS("2026-11-07T11:00:00.5+01:00")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-11-07T10:00:00.500000000Z"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := NormalizeTS("m0"); err == nil {
		t.Error("NormalizeTS(m0): want error")
	}
}

func TestMigration0007_PadsLegacyTimestamps(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	full := osDirFSRepoRoot("migrations")
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(full, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() < "0007" {
			b, _ := fs.ReadFile(full, e.Name())
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := Migrate(ctx, db, before, slog.Default()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (1, 'a', 'x', 'p', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('r', 'r', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO messages(room_id, author_key_id, ts, content, content_type) VALUES
		 ('r', 1, '2026-11-07T10:00:00Z', 'a', 'text/plain'),
		 ('r', 1, '2026-11-07T10:00:00.1Z', 'b', 'text/plain'),
		 ('r', 1, '2026-11-07T10:00:00.123456789Z', 'c', 'text/plain')`,
		`INSERT INTO audit(ts, action, target_type, target_id, result) VALUES ('2026-11-07T10:00:00.12Z', 'a', 'b', 'c', 'ok')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := Migrate(ctx, db, full, slog.Default()); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := db.Query(`SELECT ts FROM messages UNION ALL SELECT ts FROM audit`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	want := []string{"2026-11-07T10:00:00.000000000Z", "2026-11-07T10:00:00.100000000Z",
		"2026-11-07T10:00:00.123456789Z", "2026-11-07T10:00:00.120000000Z"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
