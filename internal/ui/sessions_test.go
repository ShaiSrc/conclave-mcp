package ui

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

// testDB is a fresh, fully migrated database.
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

// clock is a settable time source for store.now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore(t *testing.T) (*store, *clock) {
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	return &store{db: testDB(t), now: c.now}, c
}

func TestSessions_IdleAndAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, err := s.createSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM ui_sessions`).Scan(&stored); err != nil || stored != hashToken(tok) || stored == tok {
		t.Fatalf("stored %q (%v): want the SHA-256 of the token, not the token", stored, err)
	}
	for i := 0; i < 12; i++ { // used every 59 minutes: never idle, 11 h 48 min in total
		c.advance(59 * time.Minute)
		if ok, err := s.session(ctx, tok); err != nil || !ok {
			t.Fatalf("after %d uses: session = %v, %v; want live", i+1, ok, err)
		}
	}
	c.advance(59 * time.Minute) // 12 h 47 min after login, 59 minutes idle
	if ok, _ := s.session(ctx, tok); ok {
		t.Error("session still live 12 h after login")
	}

	idle, _ := s.createSession(ctx)
	c.advance(61 * time.Minute)
	if ok, _ := s.session(ctx, idle); ok {
		t.Error("session still live after 61 idle minutes")
	}
	if ok, _ := s.session(ctx, "not-a-token"); ok {
		t.Error("unknown token accepted")
	}
}

func TestSessions_DeleteAndPurge(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, _ := s.createSession(ctx)
	if err := s.deleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.session(ctx, tok); ok {
		t.Error("session live after delete")
	}
	_, _ = s.createSession(ctx)
	c.advance(2 * time.Hour)
	_, _ = s.createSession(ctx) // purges the idle one
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ui_sessions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("sessions after purge: %d (%v), want 1", n, err)
	}
}

func TestSetupTokens_SingleUseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s, c := newTestStore(t)
	tok, expires, err := s.createSetupToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := c.t.Add(10 * time.Minute); !expires.Equal(want) {
		t.Errorf("expires %v, want %v", expires, want)
	}
	if ok, _ := s.setupTokenValid(ctx, tok); !ok {
		t.Fatal("fresh token not valid")
	}
	if ok, _ := s.useSetupToken(ctx, tok); !ok {
		t.Fatal("first use refused")
	}
	if ok, _ := s.useSetupToken(ctx, tok); ok {
		t.Error("second use accepted")
	}
	if ok, _ := s.setupTokenValid(ctx, tok); ok {
		t.Error("used token still valid")
	}

	late, _, _ := s.createSetupToken(ctx)
	c.advance(10*time.Minute + time.Second)
	if ok, _ := s.setupTokenValid(ctx, late); ok {
		t.Error("token valid after 10 minutes")
	}
	if ok, _ := s.useSetupToken(ctx, late); ok {
		t.Error("expired token used")
	}
	if ok, _ := s.setupTokenValid(ctx, "unknown"); ok {
		t.Error("unknown token valid")
	}
}
