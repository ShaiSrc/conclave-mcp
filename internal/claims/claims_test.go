package claims

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/rights"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// env: keys 1 agent-a (admin key, every right), 2 agent-b (every right), 3 reader (read), 4 outsider (no member) in room "R"; the clock is pinned.
type env struct {
	svc *Service
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, closeFn, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	_, thisFile, _, _ := runtime.Caller(0)
	migs := os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"))
	if _, err := storage.Migrate(context.Background(), db, migs, slog.Default()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO api_keys(id, label, hash, prefix, created_at) VALUES
		 (1,'agent-a','x','pa000001',?),(2,'agent-b','x','pb000002',?),(3,'reader','x','pc000003',?),(4,'outsider','x','pd000004',?)`,
		`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('R','repo',1,?)`,
		fmt.Sprintf(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES
		 ('R',1,'owner',?,'%[1]s'),('R',2,'writer',?,'%[1]s'),('R',3,'reader',?,'read')`, rights.All().Storage()),
	} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = ts
		}
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	e := &env{now: time.Unix(1_800_000_000, 0)}
	e.svc = NewService(db)
	e.svc.Now = func() time.Time { return e.now }
	return e
}

func (e *env) do(caller int64, method, path, body string) (int, map[string]any) {
	var scopes []string
	if caller == 1 {
		scopes = []string{auth.ScopeAdmin}
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: caller, Scopes: scopes})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	e.svc.Routes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func claimPaths(out map[string]any) []string {
	var ps []string
	list, _ := out["claims"].([]any)
	for _, c := range list {
		ps = append(ps, c.(map[string]any)["path"].(string))
	}
	return ps
}

func TestNormalizeAndOverlaps(t *testing.T) {
	for in, want := range map[string]string{
		"src/auth/": "src/auth/", "./src//a.go": "src/a.go", `src\b.go`: "src/b.go", "a/../b.go": "b.go",
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "/etc/passwd", "../x", "a/../../x"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) accepted", bad)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"src/a.go", "src/a.go", true}, {"src/", "src/a.go", true}, {"src/a.go", "src/", true},
		{"src/", "srcx/a.go", false}, {"src/a.go", "src/b.go", false}, {"src/auth", "src/auth/x.go", false},
	} {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestClaim_ConflictIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	if code, out := e.do(1, "POST", "/rooms/R/claims", `{"paths":["src/auth/"]}`); code != 201 {
		t.Fatalf("claim: %d %v", code, out)
	}
	code, out := e.do(2, "POST", "/rooms/R/claims", `{"paths":["docs/x.md","src/auth/login.go"]}`)
	if code != 409 {
		t.Fatalf("overlapping claim: got %d, want 409", code)
	}
	if msg := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "agent-a") {
		t.Errorf("conflict message should name the holder: %q", msg)
	}
	_, list := e.do(2, "GET", "/rooms/R/claims", "")
	if got := claimPaths(list); len(got) != 1 || got[0] != "src/auth/" {
		t.Errorf("docs/x.md must not be claimed after a conflict: %v", got)
	}
}

func TestClaim_RefreshAndExplicitUpgradesAuto(t *testing.T) {
	e := newEnv(t)
	e.do(1, "POST", "/rooms/R/claims", `{"paths":["a.go"],"auto":true,"ttl_seconds":60}`)
	e.do(1, "POST", "/rooms/R/claims", `{"paths":["b.go"],"ttl_seconds":600}`) // refreshes a.go too
	e.now = e.now.Add(120 * time.Second)
	_, list := e.do(1, "GET", "/rooms/R/claims", "")
	if got := claimPaths(list); len(got) != 2 {
		t.Fatalf("a.go must have been refreshed by the second claim: %v", got)
	}
	e.do(1, "POST", "/rooms/R/claims", `{"paths":["a.go"]}`)
	_, list = e.do(1, "GET", "/rooms/R/claims", "")
	for _, c := range list["claims"].([]any) {
		if m := c.(map[string]any); m["path"] == "a.go" && m["auto"] != false {
			t.Errorf("explicit claim must clear auto: %v", m)
		}
	}
}

func TestClaim_ExpiredClaimFreesPath(t *testing.T) {
	e := newEnv(t)
	e.do(1, "POST", "/rooms/R/claims", `{"paths":["src/"],"ttl_seconds":60}`)
	if code, _ := e.do(2, "POST", "/rooms/R/claims", `{"paths":["src/x.go"]}`); code != 409 {
		t.Fatalf("before expiry: got %d, want 409", code)
	}
	e.now = e.now.Add(61 * time.Second)
	if code, out := e.do(2, "POST", "/rooms/R/claims", `{"paths":["src/x.go"]}`); code != 201 {
		t.Fatalf("after expiry: got %d %v, want 201", code, out)
	}
}

func TestRelease_OwnAndAdminForce(t *testing.T) {
	e := newEnv(t)
	e.do(1, "POST", "/rooms/R/claims", `{"paths":["a.go","b.go"]}`)
	e.do(2, "POST", "/rooms/R/claims", `{"paths":["c.go"]}`)
	if code, _ := e.do(2, "POST", "/rooms/R/claims/release", `{"key_id":1}`); code != 403 {
		t.Errorf("writer force-releasing: got %d, want 403", code)
	}
	for _, c := range []struct{ body, want string }{
		{`{"paths":["a.go"]}`, "[a.go]"},
		{`{"key_id":2}`, "[c.go]"}, // an admin key may release others' claims
		{``, "[b.go]"},             // empty body: all own claims
	} {
		if _, out := e.do(1, "POST", "/rooms/R/claims/release", c.body); fmt.Sprint(out["released"]) != c.want {
			t.Errorf("release %s: got %v, want %s", c.body, out["released"], c.want)
		}
	}
}

func TestAccess(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		caller       int64
		method, path string
		body         string
		want         int
	}{
		{3, "POST", "/rooms/R/claims", `{"paths":["a.go"]}`, 403}, // reader
		{3, "POST", "/rooms/R/claims/heartbeat", "", 403},         // reader: no claim right
		{3, "POST", "/rooms/R/claims/release", "", 403},
		{3, "GET", "/rooms/R/claims", "", 200},
		{4, "GET", "/rooms/R/claims", "", 404}, // not a member
		{1, "POST", "/rooms/R/claims", `{"paths":["../x"]}`, 400},
	} {
		if code, _ := e.do(c.caller, c.method, c.path, c.body); code != c.want {
			t.Errorf("key %d %s %s %s: got %d, want %d", c.caller, c.method, c.path, c.body, code, c.want)
		}
	}
}

func TestList_CarriesHolderTrustOrigin(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.DB.Exec(`UPDATE api_keys SET trust_origin = 'external' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(2, "POST", "/rooms/R/claims", `{"paths":["a.go"]}`); code != 201 {
		t.Fatalf("claim: %d", code)
	}
	_, out := e.do(1, "GET", "/rooms/R/claims", "")
	list, _ := out["claims"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["trust_origin"] != "external" {
		t.Errorf("want trust_origin external: %v", out)
	}
}
