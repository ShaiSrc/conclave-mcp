package rooms

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
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
	"github.com/hermes/conclave-mcp/internal/storage"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Skipf("can't chmod: %v", err)
	}
	db, closeFn, err := storage.Open(context.Background(), filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if _, err := storage.Migrate(context.Background(), db, migrationsDir(), slog.Default()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func migrationsDir() fs.FS {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return os.DirFS(filepath.Join(repoRoot, "migrations"))
}

func seedKey(t *testing.T, db *sql.DB, label string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO api_keys(label, hash, prefix, created_at) VALUES (?, 'phc:fake', ?, ?)`,
		label, label+"-prefix", time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// newServer returns a chi router that mounts the rooms service
// behind an auth-injection middleware. The middleware places the
// given key id and scopes into the request context as auth.Claims.
func newServer(t *testing.T, db *sql.DB, callerID int64, scopes ...string) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), auth.ClaimsKey{}, auth.Claims{KeyID: callerID, Scopes: scopes})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	NewService(db).Routes(r)
	return r
}

// setupAs creates a DB, seeds an admin key, and returns a server that
// authenticates as that key plus the DB handle and the key id.
func setupAs(t *testing.T) (*chi.Mux, *sql.DB, int64) {
	t.Helper()
	db := setupDB(t)
	id := seedKey(t, db, "k-"+t.Name())
	return newServer(t, db, id, auth.ScopeAdmin), db, id
}

// do sends one request to h and returns the recorder.
func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// createRoom creates a room through h, which must be an admin server.
func createRoom(t *testing.T, h http.Handler, name string) roomView {
	t.Helper()
	rec := do(h, http.MethodPost, "/rooms", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
	}
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)
	return rv
}

const allRights = "read,claim,question,answer,decision,handoff,status_update,task_assignment,alert,system"

func TestRooms_CreateReturnsIDAndAssignsOwner(t *testing.T) {
	r, db, caller := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1","description":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)
	if rv.Name != "r1" {
		t.Errorf("name: %q", rv.Name)
	}
	if rv.OwnerKeyID != caller {
		t.Errorf("owner: got %d, want %d", rv.OwnerKeyID, caller)
	}
	if rv.ID == "" {
		t.Error("id not set")
	}
	var role, stored string
	if err := db.QueryRow("SELECT role, rights FROM room_members WHERE room_id=? AND key_id=?", rv.ID, caller).Scan(&role, &stored); err != nil {
		t.Fatalf("owner member: %v", err)
	}
	if role != "owner" || stored != allRights {
		t.Errorf("creator row: role %q rights %q, want owner and every right", role, stored)
	}
}

func TestRooms_CreateRejectsBadName(t *testing.T) {
	r, _, _ := setupAs(t)
	for _, name := range []string{"", strings.Repeat("x", 129)} {
		body := bytes.NewBufferString(`{"name":"` + name + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/rooms", body)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q: got %d, want 400", name, rec.Code)
		}
	}
}

func TestRooms_CreateDuplicateNameIs409(t *testing.T) {
	r, _, _ := setupAs(t)
	for i := 0; i < 2; i++ {
		body := bytes.NewBufferString(`{"name":"dup"}`)
		req := httptest.NewRequest(http.MethodPost, "/rooms", body)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first: %d", rec.Code)
		}
		if i == 1 && rec.Code != http.StatusConflict {
			t.Errorf("second: %d, want 409", rec.Code)
		}
	}
}

func TestRooms_ListShowsMemberRooms(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	// Owner sees the room.
	req = httptest.NewRequest(http.MethodGet, "/rooms", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), rv.ID) {
		t.Errorf("list missing room: %s", rec.Body.String())
	}

	// A fresh user with a fresh DB sees nothing.
	r2, _, _ := setupAs(t)
	req = httptest.NewRequest(http.MethodGet, "/rooms", nil)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	body2 := rec.Body.String()
	if !strings.Contains(body2, `"rooms":[]`) && !strings.Contains(body2, `"rooms":null`) {
		t.Errorf("non-member list should be empty, got: %s", body2)
	}
}

func TestRooms_GetReturns404ForNonMember(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	r2, _, _ := setupAs(t)
	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-member get: got %d, want 404", rec.Code)
	}
}

func TestRooms_ArchiveAdminOnly(t *testing.T) {
	r, db, _ := setupAs(t)
	rv := createRoom(t, r, "r1")
	agent := newServer(t, db, seedKey(t, db, "agent"))
	if rec := do(agent, http.MethodPost, "/rooms/"+rv.ID+"/archive", ""); rec.Code != http.StatusForbidden {
		t.Errorf("agent archive: got %d, want 403", rec.Code)
	}
	if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/archive", ""); rec.Code != http.StatusNoContent {
		t.Errorf("admin archive: got %d, want 204", rec.Code)
	}
	if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/archive", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second archive: got %d, want 404", rec.Code)
	}
}

func TestRooms_InviteAndRemoveMember(t *testing.T) {
	r, db, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	// Seed a second key (guest) in the same DB; build a guest server
	// that shares the same DB.
	guest := seedKey(t, db, "guest")
	rGuest := newServer(t, db, guest)

	body = bytes.NewBufferString(`{"key_id":` + itoa(guest) + `,"role":"writer"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d; body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	rGuest.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("guest get: %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/rooms/"+rv.ID+"/members/"+itoa(guest), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("remove: %d, want 204", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/rooms/"+rv.ID, nil)
	rec = httptest.NewRecorder()
	rGuest.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("guest get after remove: %d, want 404", rec.Code)
	}
}

func TestRooms_RemoveLastOwnerIs409(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	req = httptest.NewRequest(http.MethodDelete, "/rooms/"+rv.ID+"/members/"+itoa(rv.OwnerKeyID), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("remove last owner: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRooms_InviteRejectsBadRole(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	body = bytes.NewBufferString(`{"key_id":999,"role":"admin"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d, want 400", rec.Code)
	}
}

func TestRooms_InviteRejectsMissingKeyID(t *testing.T) {
	r, _, _ := setupAs(t)
	body := bytes.NewBufferString(`{"name":"r1"}`)
	req := httptest.NewRequest(http.MethodPost, "/rooms", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var rv roomView
	_ = json.Unmarshal(rec.Body.Bytes(), &rv)

	body = bytes.NewBufferString(`{"role":"reader"}`)
	req = httptest.NewRequest(http.MethodPost, "/rooms/"+rv.ID+"/members", body)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing key_id: %d, want 400", rec.Code)
	}
}

func TestRooms_NoAuthClaimsReturns401(t *testing.T) {
	db := setupDB(t)
	r := chi.NewRouter()
	NewService(db).Routes(r)
	req := httptest.NewRequest(http.MethodPost, "/rooms", bytes.NewBufferString(`{"name":"r1"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: %d, want 401", rec.Code)
	}
}

func TestRooms_ManagementIsAdminOnly(t *testing.T) {
	r, db, _ := setupAs(t)
	rv := createRoom(t, r, "r1")
	agentID := seedKey(t, db, "agent")
	agent := newServer(t, db, agentID)
	member := "/rooms/" + rv.ID + "/members/" + itoa(agentID)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/rooms", `{"name":"r2"}`},
		{http.MethodPost, "/rooms/" + rv.ID + "/archive", ""},
		{http.MethodPost, "/rooms/" + rv.ID + "/invite", `{"key_id":` + itoa(agentID) + `,"rights":["read"]}`},
		{http.MethodPost, "/rooms/" + rv.ID + "/members", `{"key_id":` + itoa(agentID) + `,"rights":["read"]}`},
		{http.MethodPatch, member, `{"rights":["read"]}`},
		{http.MethodDelete, member, ""},
	} {
		rec := do(agent, c.method, c.path, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin key required") {
			t.Errorf("%s %s as agent: %d %s, want 403 admin key required", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRooms_InviteStoresRightsAndTranslatesRole(t *testing.T) {
	r, db, _ := setupAs(t)
	rv := createRoom(t, r, "r1")
	for i, c := range []struct{ fields, want string }{
		{`"rights":["handoff","read"]`, "read,handoff"},
		{`"rights":["all"]`, allRights},
		{``, ""}, // no rights: the invitation grants nothing
		{`"rights":[]`, ""},
		{`"role":"writer"`, allRights}, // a client from before rights
		{`"role":"reader"`, "read"},
		{`"role":"writer","rights":["read"]`, "read"}, // rights wins
	} {
		key := seedKey(t, db, "k"+itoa(int64(i)))
		body := `{"key_id":` + itoa(key)
		if c.fields != "" {
			body += "," + c.fields
		}
		if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/invite", body+"}"); rec.Code != http.StatusCreated {
			t.Fatalf("invite %s: %d %s", body, rec.Code, rec.Body.String())
		}
		var role, stored string
		if err := db.QueryRow(`SELECT role, rights FROM room_members WHERE room_id = ? AND key_id = ?`, rv.ID, key).Scan(&role, &stored); err != nil {
			t.Fatal(err)
		}
		if role != "reader" || stored != c.want {
			t.Errorf("%s: role %q rights %q, want reader and %q", body, role, stored, c.want)
		}
	}
	var detail string
	_ = db.QueryRow(`SELECT detail FROM audit WHERE action = 'room.invite' ORDER BY id LIMIT 1`).Scan(&detail)
	if want := `{"rights":["read","handoff"],"room_id":"` + rv.ID + `"}`; detail != want {
		t.Errorf("room.invite detail = %s, want %s", detail, want)
	}
	if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/invite", `{"key_id":`+itoa(seedKey(t, db, "bad"))+`,"rights":["read","fly"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown right: got %d, want 400", rec.Code)
	}
	if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/invite", `{"key_id":9999,"rights":["read"]}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key: got %d, want 404", rec.Code)
	}
}

func TestRooms_OneRoomPerKey(t *testing.T) {
	r, db, _ := setupAs(t)
	a, b := createRoom(t, r, "a"), createRoom(t, r, "b")
	invite := func(room string, key int64, extra string) *httptest.ResponseRecorder {
		return do(r, http.MethodPost, "/rooms/"+room+"/invite", `{"key_id":`+itoa(key)+`,"rights":["read"]`+extra+`}`)
	}
	agent := seedKey(t, db, "agent")
	if rec := invite(a.ID, agent, ""); rec.Code != http.StatusCreated {
		t.Fatalf("first room: %d %s", rec.Code, rec.Body.String())
	}
	if rec := invite(b.ID, agent, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"key_in_other_room"`) {
		t.Errorf("second room: %d %s, want 409 key_in_other_room", rec.Code, rec.Body.String())
	}
	if rec := invite(b.ID, agent, `,"allow_multi_room":true`); rec.Code != http.StatusCreated {
		t.Errorf("allow_multi_room: %d %s, want 201", rec.Code, rec.Body.String())
	}
	admin2 := seedKey(t, db, "admin2")
	if _, err := db.Exec(`UPDATE api_keys SET is_admin = 1 WHERE id = ?`, admin2); err != nil {
		t.Fatal(err)
	}
	if invite(a.ID, admin2, "").Code != http.StatusCreated || invite(b.ID, admin2, "").Code != http.StatusCreated {
		t.Error("admin keys must be exempt from the one-room rule")
	}
	// A room that is archived does not count.
	other := seedKey(t, db, "other")
	if rec := invite(a.ID, other, ""); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if rec := do(r, http.MethodPost, "/rooms/"+a.ID+"/archive", ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body.String())
	}
	if rec := invite(b.ID, other, ""); rec.Code != http.StatusCreated {
		t.Errorf("key whose only other room is archived: %d %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestRooms_SetRightsAndAudit(t *testing.T) {
	r, db, _ := setupAs(t)
	rv := createRoom(t, r, "r1")
	agent := seedKey(t, db, "agent")
	if rec := do(r, http.MethodPost, "/rooms/"+rv.ID+"/invite", `{"key_id":`+itoa(agent)+`,"rights":["read"]}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	member := "/rooms/" + rv.ID + "/members/" + itoa(agent)
	rec := do(r, http.MethodPatch, member, `{"rights":["handoff","read"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rights":["read","handoff"]`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var stored, detail string
	_ = db.QueryRow(`SELECT rights FROM room_members WHERE room_id = ? AND key_id = ?`, rv.ID, agent).Scan(&stored)
	_ = db.QueryRow(`SELECT detail FROM audit WHERE action = 'room.rights'`).Scan(&detail)
	if want := `{"new":["read","handoff"],"old":["read"],"room_id":"` + rv.ID + `"}`; stored != "read,handoff" || detail != want {
		t.Errorf("stored %q, audit %s; want read,handoff and %s", stored, detail, want)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/rooms/" + rv.ID + "/members/9999", `{"rights":["read"]}`, http.StatusNotFound},
		{member, `{"rights":["fly"]}`, http.StatusBadRequest},
		{member, `{}`, http.StatusBadRequest},
	} {
		if rec := do(r, http.MethodPatch, c.path, c.body); rec.Code != c.want {
			t.Errorf("PATCH %s %s: got %d, want %d", c.path, c.body, rec.Code, c.want)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestRooms_GetMembersAndListForAgents(t *testing.T) {
	r, db, _ := setupAs(t)
	mine := createRoom(t, r, "mine")
	if rec := do(r, http.MethodPost, "/rooms", `{"name":"other","visibility":"public"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	agent := seedKey(t, db, "agent")
	if rec := do(r, http.MethodPost, "/rooms/"+mine.ID+"/invite", `{"key_id":`+itoa(agent)+`,"rights":["read"]}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rAgent := newServer(t, db, agent)

	if rec := do(rAgent, http.MethodGet, "/rooms", ""); !strings.Contains(rec.Body.String(), mine.ID) || strings.Contains(rec.Body.String(), `"other"`) {
		t.Errorf("agent list must hold only its own room: %s", rec.Body.String())
	}
	if rec := do(r, http.MethodGet, "/rooms", ""); !strings.Contains(rec.Body.String(), `"other"`) {
		t.Errorf("admin list must hold every room: %s", rec.Body.String())
	}
	if rec := do(rAgent, http.MethodGet, "/rooms/"+mine.ID, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"my_rights":["read"]`) {
		t.Errorf("agent get: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(rAgent, http.MethodGet, "/rooms/"+mine.ID+"/members", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"label":"agent"`) || strings.Contains(rec.Body.String(), `"rights"`) {
		t.Errorf("agent members (no rights field): %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(r, http.MethodGet, "/rooms/"+mine.ID+"/members", ""); !strings.Contains(rec.Body.String(), `"rights":["read"]`) {
		t.Errorf("admin members must show rights: %s", rec.Body.String())
	}
	if _, err := db.Exec(`UPDATE room_members SET rights = '' WHERE key_id = ?`, agent); err != nil {
		t.Fatal(err)
	}
	if rec := do(rAgent, http.MethodGet, "/rooms/"+mine.ID, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"my_rights":[]`) {
		t.Errorf("member without rights still gets the room: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(rAgent, http.MethodGet, "/rooms/"+mine.ID+"/members", ""); rec.Code != http.StatusForbidden {
		t.Errorf("members without read: %d, want 403", rec.Code)
	}
}
