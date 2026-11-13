package bots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, closeDB, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDB() })
	if _, err := storage.Migrate(context.Background(), db, migrations.FS(), nil); err != nil {
		t.Fatal(err)
	}
	return NewStore(db), db
}

func addKey(t *testing.T, db *sql.DB, label string, admin bool) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, is_admin)
		VALUES (?, 'x', ?, '2026-01-01T00:00:00Z', ?)`, label, label, admin)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func revoke(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE api_keys SET revoked_at = '2026-01-02T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

func adminC(id int64) auth.Claims { return auth.Claims{KeyID: id, Scopes: []string{auth.ScopeAdmin}} }
func keyC(id int64) auth.Claims   { return auth.Claims{KeyID: id} }

func do(s *Store, c auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey{}, c)))
			})
		})
		s.Routes(r)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func want(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || (code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`)) {
		t.Errorf("got %d %s; want %d %s", rec.Code, rec.Body, status, code)
	}
}

func profileOf(t *testing.T, rec *httptest.ResponseRecorder) Profile {
	t.Helper()
	var p Profile
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("want a profile, got %d %s", rec.Code, rec.Body)
	}
	return p
}

func handlePath(id int64) string { return fmt.Sprintf("/v1/keys/%d/handle", id) }

// world has an admin key, a key without a handle and one bot per
// visibility, each named after its visibility.
type world struct {
	s                              *Store
	db                             *sql.DB
	admin, plain, priv, peers, pub int64
}

func newWorld(t *testing.T) world {
	t.Helper()
	s, db := testStore(t)
	w := world{s: s, db: db, admin: addKey(t, db, "root", true), plain: addKey(t, db, "plain", false)}
	for _, b := range []struct {
		id        *int64
		name, vis string
	}{{&w.priv, "priv", Private}, {&w.peers, "peers", Peers}, {&w.pub, "pub", Public}} {
		*b.id = addKey(t, db, b.name, false)
		want(t, do(s, adminC(w.admin), "PUT", handlePath(*b.id), `{"handle":"`+b.name+`"}`), 200, "")
		want(t, do(s, adminC(w.admin), "PATCH", "/v1/bots/"+b.name+"/profile", `{"visibility":"`+b.vis+`"}`), 200, "")
	}
	return w
}

func TestHandle_SetChangeRemove(t *testing.T) {
	s, db := testStore(t)
	admin, k := addKey(t, db, "root", true), addKey(t, db, "agent", false)
	rec := do(s, adminC(admin), "PUT", handlePath(k), `{"handle":"Agent-C"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"handle":"agent-c"`) {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	rec = do(s, adminC(admin), "GET", "/v1/bots/agent-c", "")
	if p := profileOf(t, rec); p.Handle != "agent-c" || p.Visibility != Private || p.DisplayName != "" ||
		!strings.Contains(rec.Body.String(), `"skills":[]`) || strings.Contains(rec.Body.String(), "updated_at") {
		t.Errorf("a handle without a profile row must read as an empty private profile: %s", rec.Body)
	}
	want(t, do(s, adminC(admin), "PUT", handlePath(k), `{"handle":"agent-d"}`), 200, "")
	want(t, do(s, adminC(admin), "GET", "/v1/bots/agent-c", ""), 404, "not_found")
	want(t, do(s, adminC(admin), "GET", "/v1/bots/agent-d", ""), 200, "")
	want(t, do(s, adminC(admin), "DELETE", handlePath(k), ""), 204, "")
	want(t, do(s, adminC(admin), "GET", "/v1/bots/agent-d", ""), 404, "not_found")
	want(t, do(s, adminC(admin), "DELETE", handlePath(k), ""), 404, "not_found")

	want(t, do(s, keyC(k), "PUT", handlePath(k), `{"handle":"x"}`), 403, "forbidden")
	want(t, do(s, keyC(k), "DELETE", handlePath(k), ""), 403, "forbidden")
	for _, body := range []string{`{"handle":"-x"}`, `{"handle":"admin"}`, `{"handle":"well-known"}`, `{}`, `nope`} {
		want(t, do(s, adminC(admin), "PUT", handlePath(k), body), 400, "bad_request")
	}
	want(t, do(s, adminC(admin), "PUT", "/v1/keys/999/handle", `{"handle":"x"}`), 404, "not_found")
	want(t, do(s, adminC(admin), "PUT", "/v1/keys/abc/handle", `{"handle":"x"}`), 400, "bad_request")
}

func TestHandle_AdminKeyRefused(t *testing.T) {
	s, db := testStore(t)
	admin := addKey(t, db, "root", true)
	want(t, do(s, adminC(admin), "PUT", handlePath(admin), `{"handle":"root"}`), 400, "admin_key")
	if err := s.SetHandle(context.Background(), admin, "root"); !errors.Is(err, ErrAdminKey) {
		t.Errorf("SetHandle on an admin key: %v", err)
	}
}

func TestHandle_UniqueIncludingRevoked(t *testing.T) {
	s, db := testStore(t)
	admin, a, b := addKey(t, db, "root", true), addKey(t, db, "a", false), addKey(t, db, "b", false)
	want(t, do(s, adminC(admin), "PUT", handlePath(a), `{"handle":"bob"}`), 200, "")
	want(t, do(s, adminC(admin), "PUT", handlePath(a), `{"handle":"bob"}`), 200, "")
	want(t, do(s, adminC(admin), "PUT", handlePath(b), `{"handle":"bob"}`), 409, "handle_taken")
	want(t, do(s, adminC(admin), "PUT", handlePath(b), `{"handle":"BOB"}`), 409, "handle_taken")
	revoke(t, db, a)
	want(t, do(s, adminC(admin), "PUT", handlePath(b), `{"handle":"bob"}`), 409, "handle_taken")
	want(t, do(s, adminC(admin), "PUT", handlePath(a), `{"handle":"other"}`), 404, "not_found")
	want(t, do(s, adminC(admin), "DELETE", handlePath(a), ""), 404, "not_found")
	want(t, do(s, adminC(admin), "GET", "/v1/bots/bob", ""), 404, "not_found")
}

func TestHandle_RemoveDropsProfile(t *testing.T) {
	w := newWorld(t)
	want(t, do(w.s, adminC(w.admin), "PATCH", "/v1/bots/pub/profile", `{"display_name":"Pub"}`), 200, "")
	want(t, do(w.s, adminC(w.admin), "DELETE", handlePath(w.pub), ""), 204, "")
	want(t, do(w.s, adminC(w.admin), "PUT", handlePath(w.pub), `{"handle":"pub2"}`), 200, "")
	if p := profileOf(t, do(w.s, adminC(w.admin), "GET", "/v1/bots/pub2", "")); p.Visibility != Private || p.DisplayName != "" {
		t.Errorf("a new handle must not bring back the old public profile: %+v", p)
	}
}

func rep(s string, n int) string { return strings.Repeat(s, n) }

func skillsJSON(n int) string {
	var parts []string
	for i := range n {
		b, _ := json.Marshal(Skill{ID: fmt.Sprintf("%032d", i), Name: rep("n", 64), Description: rep("d", 300)})
		parts = append(parts, string(b))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func skillJSON(id, name, desc string) string {
	b, _ := json.Marshal([]Skill{{ID: id, Name: name, Description: desc}})
	return string(b)
}

func TestProfile_Limits(t *testing.T) {
	w := newWorld(t)
	path := "/v1/bots/peers/profile"
	for _, body := range []string{
		`{"display_name":"` + rep("a", 65) + `"}`,
		`{"description":"` + rep("a", 1001) + `"}`,
		`{"skills":` + skillsJSON(21) + `}`,
		`{"skills":` + skillJSON("", "n", "d") + `}`,
		`{"skills":` + skillJSON("Bad", "n", "d") + `}`,
		`{"skills":` + skillJSON("a b", "n", "d") + `}`,
		`{"skills":` + skillJSON(rep("x", 33), "n", "d") + `}`,
		`{"skills":` + skillJSON("s", rep("n", 65), "d") + `}`,
		`{"skills":` + skillJSON("s", "n", rep("d", 301)) + `}`,
		`{"visibility":"world"}`,
		`{"display_name":"x","colour":"red"}`,
		`not json`,
	} {
		want(t, do(w.s, adminC(w.admin), "PATCH", path, body), 400, "bad_request")
	}
	for _, body := range []string{
		`{"display_name":"` + rep("é", 64) + `"}`,
		`{"display_name":"` + rep("a", 64) + "\u200B\u202E" + `"}`,
		`{"description":"` + rep("a", 1000) + `"}`,
		`{"skills":` + skillsJSON(20) + `}`,
	} {
		want(t, do(w.s, adminC(w.admin), "PATCH", path, body), 200, "")
	}
}

func TestProfile_TextCleanedOnSave(t *testing.T) {
	w := newWorld(t)
	p := profileOf(t, do(w.s, adminC(w.admin), "PATCH", "/v1/bots/peers/profile",
		`{"display_name":"\u202Eevil\u200B name\n","description":" a\r\nb\u0007 ","skills":[{"id":"s","name":"\u2066x\u2069","description":"l1\nl2\u200D"}]}`))
	if p.DisplayName != "evil name" || p.Description != "a\nb" || len(p.Skills) != 1 ||
		p.Skills[0].Name != "x" || p.Skills[0].Description != "l1\nl2" {
		t.Errorf("not cleaned: %+v", p)
	}
	if got := profileOf(t, do(w.s, adminC(w.admin), "GET", "/v1/bots/peers", "")); got.DisplayName != "evil name" || got.UpdatedAt.IsZero() {
		t.Errorf("stored: %+v", got)
	}
}

func TestProfile_WhoEdits(t *testing.T) {
	w := newWorld(t)
	p := profileOf(t, do(w.s, keyC(w.peers), "PATCH", "/v1/bots/peers/profile",
		`{"display_name":"Peers Bot","description":"d","skills":[{"id":"x","name":"X","description":""}]}`))
	if p.DisplayName != "Peers Bot" || p.Description != "d" || len(p.Skills) != 1 || p.Visibility != Peers {
		t.Errorf("own text: %+v", p)
	}
	want(t, do(w.s, keyC(w.peers), "PATCH", "/v1/bots/peers/profile", `{"visibility":"public"}`), 403, "forbidden")
	want(t, do(w.s, keyC(w.peers), "PATCH", "/v1/bots/peers/profile", `{"display_name":"Y","visibility":"peers"}`), 403, "forbidden")
	if p := profileOf(t, do(w.s, adminC(w.admin), "GET", "/v1/bots/peers", "")); p.DisplayName != "Peers Bot" || p.Visibility != Peers {
		t.Errorf("a refused patch changed the profile: %+v", p)
	}

	want(t, do(w.s, keyC(w.peers), "PATCH", "/v1/bots/pub/profile", `{"display_name":"hijack"}`), 403, "forbidden")
	want(t, do(w.s, keyC(w.plain), "PATCH", "/v1/bots/pub/profile", `{"display_name":"hijack"}`), 403, "forbidden")
	hidden := do(w.s, keyC(w.peers), "PATCH", "/v1/bots/priv/profile", `{"display_name":"x"}`)
	unknown := do(w.s, keyC(w.peers), "PATCH", "/v1/bots/nobody/profile", `{"display_name":"x"}`)
	want(t, hidden, 404, "not_found")
	if hidden.Body.String() != unknown.Body.String() {
		t.Errorf("private and unknown differ: %s vs %s", hidden.Body, unknown.Body)
	}

	if p := profileOf(t, do(w.s, adminC(w.admin), "PATCH", "/v1/bots/priv/profile", `{"display_name":"P","visibility":"peers"}`)); p.DisplayName != "P" || p.Visibility != Peers {
		t.Errorf("admin edit: %+v", p)
	}
}

// A bot's save writes only the fields it sent: it keeps the visibility
// and the other fields an admin set, also when the bot read the profile
// before the admin wrote it. A first save fills the rest with defaults.
func TestProfile_BotSaveKeepsAdminChanges(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	pub, desc, name := Public, "set by the admin", "Bot name"
	if _, err := w.s.Update(ctx, "peers", Patch{Visibility: &pub}, true); err != nil {
		t.Fatal(err)
	}
	if p, err := w.s.Update(ctx, "peers", Patch{DisplayName: &name}, false); err != nil || p.Visibility != Public {
		t.Errorf("after the admin: %+v, %v", p, err)
	}

	priv := Private
	w.s.afterRead = func() {
		w.s.afterRead = nil
		if _, err := w.s.Update(ctx, "peers", Patch{Visibility: &priv, Description: &desc}, true); err != nil {
			t.Error(err)
		}
	}
	name = "Renamed"
	p, err := w.s.Update(ctx, "peers", Patch{DisplayName: &name}, false)
	if err != nil || p.Visibility != Private || p.Description != desc || p.DisplayName != name {
		t.Errorf("returned %+v, %v", p, err)
	}
	if p, err := w.s.Get(ctx, "peers"); err != nil || p.Visibility != Private || p.Description != desc || p.DisplayName != name {
		t.Errorf("stored %+v, %v", p, err)
	}

	fresh := addKey(t, w.db, "fresh", false)
	if err := w.s.SetHandle(ctx, fresh, "fresh"); err != nil {
		t.Fatal(err)
	}
	if p, err := w.s.Update(ctx, "fresh", Patch{Description: &desc}, false); err != nil || p.Visibility != Private ||
		p.Description != desc || p.DisplayName != "" || p.Skills == nil || len(p.Skills) != 0 {
		t.Errorf("first save: %+v, %v", p, err)
	}
}

// Every local profile carries its key's trust origin.
func TestProfile_TrustOrigin(t *testing.T) {
	w := newWorld(t)
	if _, err := w.db.Exec(`UPDATE api_keys SET trust_origin = 'external' WHERE id = ?`, w.pub); err != nil {
		t.Fatal(err)
	}
	for h, origin := range map[string]string{"pub": "external", "peers": "internal"} {
		if p := profileOf(t, do(w.s, keyC(w.plain), "GET", "/v1/bots/"+h, "")); p.TrustOrigin != origin {
			t.Errorf("%s: trust origin %q, want %q", h, p.TrustOrigin, origin)
		}
	}
	if body := do(w.s, keyC(w.plain), "GET", "/v1/bots", "").Body.String(); !strings.Contains(body, `"handle":"pub"`) ||
		!strings.Contains(body, `"trust_origin":"external"`) || !strings.Contains(body, `"trust_origin":"internal"`) {
		t.Errorf("list: %s", body)
	}
	if p := profileOf(t, do(w.s, keyC(w.pub), "PATCH", "/v1/bots/pub/profile", `{"description":"d"}`)); p.TrustOrigin != "external" {
		t.Errorf("patch answer: %+v", p)
	}
}

func TestVisibility_Matrix(t *testing.T) {
	w := newWorld(t)
	gone := addKey(t, w.db, "gone", false)
	want(t, do(w.s, adminC(w.admin), "PUT", handlePath(gone), `{"handle":"gone"}`), 200, "")
	want(t, do(w.s, adminC(w.admin), "PATCH", "/v1/bots/gone/profile", `{"visibility":"public"}`), 200, "")
	revoke(t, w.db, gone)

	for _, c := range []struct {
		name   string
		viewer auth.Claims
		sees   []string
	}{
		{"admin", adminC(w.admin), []string{"peers", "priv", "pub"}},
		{"key without handle", keyC(w.plain), []string{"peers", "pub"}},
		{"private bot", keyC(w.priv), []string{"peers", "priv", "pub"}},
		{"peers bot", keyC(w.peers), []string{"peers", "pub"}},
	} {
		var l struct{ Bots []Profile }
		rec := do(w.s, c.viewer, "GET", "/v1/bots", "")
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		var got []string
		for _, p := range l.Bots {
			got = append(got, p.Handle)
		}
		if rec.Code != 200 || !slices.Equal(got, c.sees) {
			t.Errorf("%s: GET /v1/bots = %d %v, want %v", c.name, rec.Code, got, c.sees)
		}
		unknown := do(w.s, c.viewer, "GET", "/v1/bots/nobody", "")
		want(t, unknown, 404, "not_found")
		for _, h := range []string{"priv", "peers", "pub", "gone"} {
			rec := do(w.s, c.viewer, "GET", "/v1/bots/"+h, "")
			if slices.Contains(c.sees, h) {
				if p := profileOf(t, rec); p.Handle != h {
					t.Errorf("%s: GET %s gave %s", c.name, h, p.Handle)
				}
			} else if rec.Code != 404 || rec.Body.String() != unknown.Body.String() {
				t.Errorf("%s: GET %s = %d %s, want the unknown-name 404", c.name, h, rec.Code, rec.Body)
			}
		}
	}

	ctx := context.Background()
	for min, sees := range map[string][]string{Private: {"peers", "priv", "pub"}, Peers: {"peers", "pub"}, Public: {"pub"}} {
		ps, err := w.s.List(ctx, min)
		var got []string
		for _, p := range ps {
			got = append(got, p.Handle)
		}
		if err != nil || !slices.Equal(got, sees) {
			t.Errorf("List(%s) = %v, %v; want %v", min, got, err, sees)
		}
	}
	if p, err := w.s.ByKey(ctx, w.pub); err != nil || p.Handle != "pub" {
		t.Errorf("ByKey(pub) = %+v, %v", p, err)
	}
	for _, id := range []int64{w.plain, gone, 999} {
		if _, err := w.s.ByKey(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("ByKey(%d) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestAudit_Rows(t *testing.T) {
	s, db := testStore(t)
	admin, k := addKey(t, db, "root", true), addKey(t, db, "agent", false)
	want(t, do(s, adminC(admin), "PUT", handlePath(k), `{"handle":"bob"}`), 200, "")
	want(t, do(s, keyC(k), "PATCH", "/v1/bots/bob/profile", `{"description":"d"}`), 200, "")
	want(t, do(s, adminC(admin), "PATCH", "/v1/bots/bob/profile", `{"display_name":"B","visibility":"public"}`), 200, "")
	want(t, do(s, adminC(admin), "PATCH", "/v1/bots/bob/profile", `{"display_name":"C","description":"d","visibility":"public"}`), 200, "")
	want(t, do(s, adminC(admin), "DELETE", handlePath(k), ""), 204, "")

	rows, err := db.Query(`SELECT actor_key_id, action, target_type, target_id, result, detail FROM audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var actor int64
		var action, typ, id, result, detail string
		if err := rows.Scan(&actor, &action, &typ, &id, &result, &detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %s %s %s %s %s", actor, action, typ, id, result, detail))
	}
	wantRows := []string{
		fmt.Sprintf(`%d bot.handle.set key %d ok {"handle":"bob"}`, admin, k),
		fmt.Sprintf(`%d bot.profile.update bot bob ok {"fields":["description"],"visibility":"private"}`, k),
		fmt.Sprintf(`%d bot.profile.update bot bob ok {"fields":["display_name","visibility"],"visibility":"public"}`, admin),
		fmt.Sprintf(`%d bot.profile.update bot bob ok {"fields":["display_name"],"visibility":"public"}`, admin),
		fmt.Sprintf(`%d bot.handle.remove key %d ok {"handle":"bob"}`, admin, k),
	}
	if !slices.Equal(got, wantRows) {
		t.Errorf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantRows, "\n"))
	}
}

// A stand-in of a remote bot never gets a local handle: it would appear
// as one of this server's bots.
func TestSetHandle_RefusesStandIn(t *testing.T) {
	s, db := testStore(t)
	res, err := db.Exec(`INSERT INTO api_keys(label, hash, prefix, created_at, trust_origin, remote_domain)
		VALUES ('bob@b.example', '', '', '2026-01-01T00:00:00Z', 'external', 'b.example')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if err := s.SetHandle(context.Background(), id, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetHandle on a stand-in: %v, want ErrNotFound", err)
	}
}
