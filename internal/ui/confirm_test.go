package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/keys"
	"github.com/hermes/conclave-mcp/internal/messages"
	"github.com/hermes/conclave-mcp/internal/rooms"
)

// withServices mounts the real keys, rooms and messages endpoints under
// /v1 behind Auth; bearer keys are refused.
func withServices(s *Service, r chi.Router) {
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.Auth(func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeErr(w, r, http.StatusUnauthorized, "unauthenticated", "no bearer keys in this test")
			})
		}))
		keys.NewService(s.db).Routes(r)
		rooms.NewService(s.db).Routes(r)
		messages.NewService(s.db, 0, nil).Routes(r)
	})
}

// wantErr fails unless the response has the status and contains msg.
func wantErr(t *testing.T, name string, st int, body []byte, want int, msg string) {
	t.Helper()
	if st != want || !strings.Contains(string(body), msg) {
		t.Errorf("%s: %d %s, want %d with %q", name, st, body, want, msg)
	}
}

// A session reads; every other method needs a confirmation.
func TestConfirm_SessionOnlyReads(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	echoCalls = 0
	if st, body := b.do("GET", "/v1/echo/x", nil); st != http.StatusOK {
		t.Errorf("GET: %d %s, want 200", st, body)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		st, body := b.do(m, "/v1/echo/x", map[string]any{"name": "x"})
		wantErr(t, m+" with the session alone", st, body, http.StatusForbidden, needsConfirm)
	}
	if echoCalls != 1 {
		t.Errorf("echo answered %d requests, want 1 (the GET)", echoCalls)
	}
}

// Finish runs exactly the request stored at begin, as the operator key
// with the session's scopes.
func TestConfirm_RunsTheBeginRequest(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, pk := loggedIn(t, s, base)
	opID, err := operatorKeyID(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Method, URI, Body string
		Key               int64
		Scopes            []string
	}
	st, body := pk.confirmChange(b, "PATCH", "/v1/echo/members/3?x=1", map[string]any{"rights": []string{"read"}})
	if st != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("confirmed PATCH: %d %s", st, body)
	}
	if got.Method != "PATCH" || got.URI != "/v1/echo/members/3?x=1" || got.Body != `{"rights":["read"]}` ||
		got.Key != opID || !slices.Equal(got.Scopes, []string{auth.ScopeAdmin, auth.ScopeUI}) {
		t.Errorf("the handler got %+v, want PATCH /v1/echo/members/3?x=1 {\"rights\":[\"read\"]} as key %d with admin and ui", got, opID)
	}
	if st, body := pk.confirmChange(b, "DELETE", "/v1/echo/keys/9", nil); st != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Method != "DELETE" || got.Body != "" {
		t.Errorf("confirmed DELETE without a body: %d %s", st, body)
	}
}

func TestConfirm_BeginRefusals(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, _ := loggedIn(t, s, base)
	echoCalls = 0
	change := func(method, path string) map[string]any { return map[string]any{"method": method, "path": path} }
	st, body := newBrowser(t, base).do("POST", "/ui/auth/confirm/begin", change("POST", "/v1/rooms"))
	wantErr(t, "begin without a session", st, body, http.StatusUnauthorized, "log in first")
	resp := b.send("POST", "/ui/auth/confirm/begin", change("POST", "/v1/rooms"), map[string]string{"Origin": "http://attacker.example"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("begin from another origin: %d, want 403", resp.StatusCode)
	}
	st, body = b.do("POST", "/ui/auth/confirm/begin", "{")
	wantErr(t, "malformed JSON", st, body, http.StatusBadRequest, "malformed JSON")
	for _, m := range []string{"GET", "HEAD", "OPTIONS", "", "post"} {
		st, body := b.do("POST", "/ui/auth/confirm/begin", change(m, "/v1/rooms"))
		wantErr(t, "method "+m, st, body, http.StatusBadRequest, "method must be POST, PUT, PATCH or DELETE")
	}
	for _, p := range []string{"", "/v1", "v1/rooms", "/ui/auth/logout", "/ui/auth/login/finish", "/ui/auth/confirm/finish",
		"/ui/auth/message/finish", "/v1/../ui/agents", "/v1/rooms/../../ui/x", "/v1//rooms", "//evil.example/v1/rooms",
		"http://localhost:7432/v1/rooms", "/v1/rooms#x", "/ui/agents/x", "/ui/auth/credentials/"} {
		st, body := b.do("POST", "/ui/auth/confirm/begin", change("POST", p))
		wantErr(t, "path "+p, st, body, http.StatusBadRequest, "path is not a change the web UI can confirm")
	}
	for _, p := range []string{"/v1/rooms", "/v1/rooms/r1/members/3", "/ui/agents", "/ui/auth/register/begin", "/ui/auth/credentials/abc"} {
		if st, body := b.do("POST", "/ui/auth/confirm/begin", change("DELETE", p)); st != http.StatusOK {
			t.Errorf("begin %s: %d %s, want 200", p, st, body)
		}
	}
	if echoCalls != 0 {
		t.Errorf("echo answered %d requests", echoCalls)
	}
}

// Every refused finish runs nothing; the first finish, refused or not,
// uses the ceremony up.
func TestConfirm_FinishRefusals(t *testing.T) {
	s, _, base := newTestUI(t, withV1)
	b, pk := loggedIn(t, s, base)
	other := newBrowser(t, base)
	if st, body := pk.login(other); st != http.StatusOK {
		t.Fatalf("second session: %d %s", st, body)
	}
	echoCalls = 0
	change := map[string]any{"method": "POST", "path": "/v1/echo/x", "body": map[string]string{"name": "x"}}
	begin := func() (http.Cookie, func() (int, []byte)) {
		t.Helper()
		opts, st, body := pk.beginAssertion(b, "confirm", change)
		if opts == nil {
			t.Fatalf("begin: %d %s", st, body)
		}
		return *b.cookies[ceremonyCookie], func() (int, []byte) { return pk.finishAssertion(b, "confirm", opts) }
	}
	audited := func(result string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'ui.confirm' AND result = ?`, result).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const noCeremony = "no confirmation in progress, or it took longer than 5 minutes"

	// No user verification.
	pk.auth.Options.UserNotVerified = true
	_, finish := begin()
	st, body := finish()
	pk.auth.Options.UserNotVerified = false
	wantErr(t, "finish without user verification", st, body, http.StatusUnauthorized, "passkey not accepted")
	if audited("error:rejected") != 1 {
		t.Error("the rejected assertion is not audited as ui.confirm error:rejected")
	}

	// Another live session cannot finish it; the refusal uses it up.
	ck, finish := begin()
	own := b.session
	b.session = other.session
	st, body = finish()
	b.session = own
	wantErr(t, "finish from another session", st, body, http.StatusForbidden, "this confirmation was started in another session")
	if audited("error:other_session") != 1 {
		t.Error("the cross-session finish is not audited as ui.confirm error:other_session")
	}
	b.cookies[ceremonyCookie] = &ck
	st, body = finish()
	wantErr(t, "retry after the refusal", st, body, http.StatusBadRequest, noCeremony)

	// A finished confirmation cannot be finished again.
	ck, finish = begin()
	if st, body := finish(); st != http.StatusOK {
		t.Fatalf("finish: %d %s", st, body)
	}
	b.cookies[ceremonyCookie] = &ck
	st, body = finish()
	wantErr(t, "second finish", st, body, http.StatusBadRequest, noCeremony)

	// Ceremonies of other kinds do not confirm a change, and a confirm
	// ceremony neither logs in nor sends a message.
	for _, c := range []struct {
		begin, finish string
		body          any
		msg           string
	}{
		{"login", "confirm", nil, noCeremony},
		{"message", "confirm", map[string]any{"room_id": "r1", "content": "x"}, noCeremony},
		{"confirm", "login", change, "no login in progress, or it took longer than 5 minutes"},
		{"confirm", "message", change, "no message confirmation in progress, or it took longer than 5 minutes"},
	} {
		opts, st, body := pk.beginAssertion(b, c.begin, c.body)
		if opts == nil {
			t.Fatalf("%s begin: %d %s", c.begin, st, body)
		}
		st, body = pk.finishAssertion(b, c.finish, opts)
		wantErr(t, c.finish+" finish with a "+c.begin+" ceremony", st, body, http.StatusBadRequest, c.msg)
	}

	// The session that began it ended.
	_, finish = begin()
	if st, _ := b.do("POST", "/ui/auth/logout", nil); st != http.StatusNoContent {
		t.Fatalf("logout: %d", st)
	}
	st, body = finish()
	wantErr(t, "finish after logout", st, body, http.StatusUnauthorized, "session expired: log in again")

	if echoCalls != 1 {
		t.Errorf("echo answered %d requests, want 1 (the one accepted finish)", echoCalls)
	}
}

// A confirmation runs real /v1 changes, but not a message: operator
// messages keep their own ceremony and mark.
func TestConfirm_RealServicesAndNoMessages(t *testing.T) {
	s, _, base := newTestUI(t, withServices)
	b, pk := loggedIn(t, s, base)
	st, body := b.do("POST", "/v1/rooms", map[string]string{"name": "r"})
	wantErr(t, "create a room with the session alone", st, body, http.StatusForbidden, needsConfirm)
	var room struct{ ID string }
	if st, body := pk.confirmChange(b, "POST", "/v1/rooms", map[string]string{"name": "r"}); st != http.StatusCreated || json.Unmarshal(body, &room) != nil || room.ID == "" {
		t.Fatalf("create a room, confirmed: %d %s", st, body)
	}
	st, body = pk.confirmChange(b, "POST", "/v1/rooms/"+room.ID+"/messages", map[string]string{"content": "unmarked"})
	wantErr(t, "a message through a change confirmation", st, body, http.StatusForbidden, "operator messages need a passkey confirmation")
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d messages stored (%v), want 0", n, err)
	}
}

// Creating an agent is one confirmation: the key and its membership, or
// nothing usable.
func TestConfirm_CreateAgent(t *testing.T) {
	s, _, base := newTestUI(t, withServices)
	b, pk := loggedIn(t, s, base)
	var room struct{ ID string }
	if st, body := pk.confirmChange(b, "POST", "/v1/rooms", map[string]string{"name": "r"}); st != http.StatusCreated || json.Unmarshal(body, &room) != nil {
		t.Fatalf("create a room: %d %s", st, body)
	}
	agent := func(label, roomID string, rights ...string) map[string]any {
		return map[string]any{"label": label, "trust_origin": "internal", "room_id": roomID, "rights": rights}
	}

	st, body := b.do("POST", "/ui/agents", agent("agent-a", room.ID, "read"))
	wantErr(t, "agent with the session alone", st, body, http.StatusForbidden, needsConfirm)

	var created struct {
		ID          int64
		Key         string
		Label       string
		TrustOrigin string `json:"trust_origin"`
	}
	st, body = pk.confirmChange(b, "POST", "/ui/agents", agent("agent-a", room.ID, "read", "answer"))
	if st != http.StatusCreated || json.Unmarshal(body, &created) != nil || !strings.HasPrefix(created.Key, "tk_") ||
		created.Label != "agent-a" || created.TrustOrigin != "internal" {
		t.Fatalf("agent, confirmed: %d %s", st, body)
	}
	var members struct {
		Members []struct {
			KeyID  int64 `json:"key_id"`
			Rights []string
		}
	}
	if st, body := b.do("GET", "/v1/rooms/"+room.ID+"/members", nil); st != http.StatusOK || json.Unmarshal(body, &members) != nil {
		t.Fatalf("members: %d %s", st, body)
	}
	found := false
	for _, m := range members.Members {
		if m.KeyID == created.ID {
			found = true
			if !slices.Equal(m.Rights, []string{"read", "answer"}) {
				t.Errorf("agent rights %v, want [read answer]", m.Rights)
			}
		}
	}
	if !found {
		t.Errorf("agent %d is not a member: %+v", created.ID, members)
	}

	// The invite fails: the key is revoked and the error says so.
	st, body = pk.confirmChange(b, "POST", "/ui/agents", agent("agent-b", "no-such-room", "read"))
	wantErr(t, "agent for a missing room", st, body, http.StatusNotFound, "The key was revoked; its label stays taken, so a retry needs another label.")
	var revoked bool
	if err := s.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM api_keys WHERE label = 'agent-b'`).Scan(&revoked); err != nil || !revoked {
		t.Errorf("agent-b key revoked: %v (%v), want true", revoked, err)
	}

	// Checked before anything is created.
	st, body = pk.confirmChange(b, "POST", "/ui/agents", agent("agent-c", room.ID))
	wantErr(t, "agent without rights", st, body, http.StatusBadRequest, "pick at least one right for the agent")
	st, body = pk.confirmChange(b, "POST", "/ui/agents", agent("agent-c", "", "read"))
	wantErr(t, "agent without a room", st, body, http.StatusBadRequest, "room_id is required")
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE label = 'agent-c'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("agent-c keys: %d (%v), want 0", n, err)
	}
}
