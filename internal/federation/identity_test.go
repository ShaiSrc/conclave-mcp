package federation

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

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

func TestLoadOrCreateIdentity_StableAcrossReload(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	first, err := LoadOrCreateIdentity(ctx, db, "a.example")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateIdentity(ctx, db, "a.example")
	if err != nil {
		t.Fatal(err)
	}
	if again.KeyID != first.KeyID || !again.Public.Equal(first.Public) {
		t.Errorf("reload gave key %s, want %s", again.KeyID, first.KeyID)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM federation_keys`).Scan(&n); err != nil || n != 1 {
		t.Errorf("federation_keys rows: %d, %v; want 1", n, err)
	}
	if again.Domain != "a.example" {
		t.Errorf("Domain = %q", again.Domain)
	}
	msg := []byte("hello")
	if !ed25519.Verify(again.Public, msg, again.Sign(msg)) {
		t.Error("a reloaded identity's signature does not verify")
	}
}

// Starts racing on one database all end with the one stored key.
func TestLoadOrCreateIdentity_Race(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	const n = 16
	ids := make([]*Identity, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = LoadOrCreateIdentity(ctx, db, "a.example")
		}()
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[i].KeyID != ids[0].KeyID {
			t.Errorf("call %d got key %s, want %s", i, ids[i].KeyID, ids[0].KeyID)
		}
	}
	if got := count(t, db, `SELECT COUNT(*) FROM federation_keys`); got != 1 {
		t.Errorf("federation_keys rows: %d, want 1", got)
	}
}

func TestLoadOrCreateIdentity_SkipsRetired(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	old, err := LoadOrCreateIdentity(ctx, db, "a.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE federation_keys SET retired_at = ?`, "2026-11-13T10:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadOrCreateIdentity(ctx, db, "a.example")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.KeyID == old.KeyID {
		t.Error("a retired key was loaded")
	}
}

func TestFingerprintAndKeyID(t *testing.T) {
	zero := make(ed25519.PublicKey, ed25519.PublicKeySize)
	if got, want := Fingerprint(zero), "SHA256:Zmh6rfhivXdsj8GLjp-OIAiXFIVu4jOzkCpZHQ1fKSU"; got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
	if got, want := KeyIDFor(zero), "ed25519:Zmh6rfhivXdsj8GL"; got != want {
		t.Errorf("KeyIDFor = %q, want %q", got, want)
	}

	id, err := LoadOrCreateIdentity(context.Background(), testDB(t), "a.example")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^SHA256:[A-Za-z0-9_-]{43}$`).MatchString(id.Fingerprint()) {
		t.Errorf("fingerprint format: %q", id.Fingerprint())
	}
	if want := "ed25519:" + id.Fingerprint()[len("SHA256:"):len("SHA256:")+16]; id.KeyID != want {
		t.Errorf("KeyID = %q, want %q", id.KeyID, want)
	}
}

func TestNew_RefusesBadDomain(t *testing.T) {
	if _, err := New(testDB(t), Options{Domain: "bad..example"}); err == nil {
		t.Error("New accepted an invalid domain")
	}
}

// The server document is exactly the spec's JSON shape.
func TestServerDocument(t *testing.T) {
	svc, err := New(testDB(t), Options{Domain: "Conclave.Example.org", Mode: "allowlist"})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc.PublicRoutes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/conclave", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	id := svc.id
	want := fmt.Sprintf(`{"protocol":"conclave-federation/1","domain":"conclave.example.org","keys":[{"key_id":%q,"alg":"ed25519","public_key":%q,"fingerprint":%q}]}`,
		id.KeyID, base64.RawURLEncoding.EncodeToString(id.Public), id.Fingerprint())
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("document:\n got %s\nwant %s", got, want)
	}
}
