package fedrooms

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	return db
}

// standIn runs StandIn in its own transaction and commits on success.
func standIn(t *testing.T, db *sql.DB, address string) (int64, error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := StandIn(context.Background(), tx, address)
	if err == nil {
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return id, err
}

func addKey(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestStandIn(t *testing.T) {
	db := testDB(t)
	id, err := standIn(t, db, "b1@b.example")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := standIn(t, db, "b1@b.example"); err != nil || again != id {
		t.Errorf("second call: %d %v, want %d", again, err, id)
	}
	// Every stand-in has an empty prefix; they must not collide.
	if other, err := standIn(t, db, "c1@c.example:8443"); err != nil || other == id {
		t.Errorf("second stand-in: %d %v", other, err)
	}
	var (
		label, hash, prefix, trust, remote string
		admin                              bool
	)
	if err := db.QueryRow(`SELECT label, hash, prefix, trust_origin, remote_domain, is_admin FROM api_keys WHERE id = ?`, id).
		Scan(&label, &hash, &prefix, &trust, &remote, &admin); err != nil {
		t.Fatal(err)
	}
	if label != "b1@b.example" || hash != "" || prefix != "" || trust != "external" || remote != "b.example" || admin {
		t.Errorf("stand-in row: label %q hash %q prefix %q trust %q remote %q admin %v", label, hash, prefix, trust, remote, admin)
	}
	for _, bad := range []string{"", "b1", "@b.example", "b1@", "B1@b.example", "b1@b@c.example", "b1 @b.example", "b1@b.example\n", "-b@b.example"} {
		if _, err := standIn(t, db, bad); err == nil {
			t.Errorf("address %q accepted", bad)
		}
	}
	// A local key that holds the label stays local and is not returned.
	addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('x@b.example', 'h', 'p1', '2026-01-01T00:00:00Z')`)
	if got, err := standIn(t, db, "x@b.example"); err == nil {
		t.Errorf("label of a local key: got stand-in %d", got)
	}
}

func TestAddress(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	remote, err := standIn(t, db, "b1@b.example")
	if err != nil {
		t.Fatal(err)
	}
	bot := addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at, handle) VALUES ('bot', 'h', 'p1', '2026-01-01T00:00:00Z', 'bot1')`)
	operator := addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('operator', 'h', 'p2', '2026-01-01T00:00:00Z', 1)`)
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, OperatorMetaKey, strconv.FormatInt(operator, 10)); err != nil {
		t.Fatal(err)
	}
	admin := addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('root', 'h', 'p3', '2026-01-01T00:00:00Z', 1)`)
	plain := addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('plain', 'h', 'p4', '2026-01-01T00:00:00Z')`)
	for _, c := range []struct {
		key  int64
		want string
	}{
		{remote, "b1@b.example"},
		{bot, "bot1@a.example"},
		{operator, "operator@a.example"},
		{admin, "key-" + strconv.FormatInt(admin, 10) + "@a.example"},
		{plain, "key-" + strconv.FormatInt(plain, 10) + "@a.example"},
	} {
		if got, err := Address(ctx, db, c.key, "a.example"); err != nil || got != c.want {
			t.Errorf("key %d: %q %v, want %q", c.key, got, err, c.want)
		}
	}
	if got, err := Address(ctx, db, 999, "a.example"); err == nil {
		t.Errorf("unknown key: %q, want an error", got)
	}
}

func TestMessagesRemoteIDUniquePerRoom(t *testing.T) {
	db := testDB(t)
	key := addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('k', 'h', 'p1', '2026-01-01T00:00:00Z')`)
	for _, room := range []string{"R1", "R2"} {
		if _, err := db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z')`, room, room, key); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(room string, remoteID any) error {
		_, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, remote_id) VALUES (?, ?, '2026-01-01T00:00:00Z', 'x', 'text/plain', ?)`, room, key, remoteID)
		return err
	}
	for _, c := range []struct {
		room   string
		remote any
		ok     bool
	}{
		{"R1", nil, true}, {"R1", nil, true}, // local messages have none
		{"R1", 7, true}, {"R1", 7, false}, {"R2", 7, true},
	} {
		if err := insert(c.room, c.remote); (err == nil) != c.ok {
			t.Errorf("room %s remote_id %v: err %v, want ok=%v", c.room, c.remote, err, c.ok)
		}
	}
}

// A stand-in's domain is in the canonical form of federation domains:
// no trailing dot, no empty label, no :443.
func TestStandIn_CanonicalDomain(t *testing.T) {
	db := testDB(t)
	for _, bad := range []string{"b1@b.example.", "b1@b..example", "b1@.b.example", "b1@b.example:443", "b1@-b.example", "b1@b.example:0080"} {
		if _, err := standIn(t, db, bad); err == nil {
			t.Errorf("address %q accepted", bad)
		}
	}
}
