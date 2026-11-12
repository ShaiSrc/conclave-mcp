package rights

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

func TestLookup_RevokedKeyIsNotAMember(t *testing.T) {
	ctx := context.Background()
	db, closeFn, err := storage.Open(ctx, filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if _, err := storage.Migrate(ctx, db, os.DirFS(filepath.Join("..", "..", "migrations")), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES (1, 'agent', 'x', 'p1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('R', 'r', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('R', 1, 'writer', '2026-01-01T00:00:00Z', 'read')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cctx := context.WithValue(ctx, auth.ClaimsKey{}, auth.Claims{KeyID: 1})
	if _, ok, err := Lookup(cctx, db, "R"); err != nil || !ok {
		t.Fatalf("live key: ok=%v err=%v, want member", ok, err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET revoked_at = '2026-01-02T00:00:00Z' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Lookup(cctx, db, "R"); err != nil || ok {
		t.Fatalf("revoked key: ok=%v err=%v, want non-member", ok, err)
	}
}
