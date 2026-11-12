package ui

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/messages"
)

// fakeSend stands in for messages.Service.SendAsOperator: it records
// the last call and answers 201.
type fakeSend struct {
	calls  int
	room   string
	body   string
	claims auth.Claims
}

func (f *fakeSend) send(w http.ResponseWriter, r *http.Request, roomID string) {
	b, _ := io.ReadAll(r.Body)
	f.calls++
	f.room, f.body = roomID, string(b)
	f.claims, _ = auth.FromContext(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "operator": true})
}

// newMessageUI is a logged-in UI whose SendAsOperator is a fake.
func newMessageUI(t *testing.T) (*Service, *fakeSend, *browser, *passkey, string) {
	t.Helper()
	s, _, base := newTestUI(t)
	f := &fakeSend{}
	s.SendAsOperator = f.send
	b, pk := loggedIn(t, s, base)
	return s, f, b, pk, base
}

// Finish posts exactly the payload stored at begin, as the operator key
// with the admin scope only, and answers what SendAsOperator answers.
func TestMessage_ConfirmedSendPostsTheBeginPayload(t *testing.T) {
	s, f, b, pk, _ := newMessageUI(t)
	opID, err := operatorKeyID(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	st, body := pk.confirm(b, map[string]any{
		"room_id": "r1", "content": "ship it", "message_type": "decision", "reply_to": 7,
		"operator": false, "content_type": "application/json", "metadata": map[string]any{"operator": true},
	})
	if st != http.StatusCreated || strings.TrimSpace(string(body)) != `{"id":1,"operator":true}` {
		t.Fatalf("confirm: %d %s, want 201 with the send response", st, body)
	}
	want := `{"content":"ship it","content_type":"text/plain","message_type":"decision","reply_to":7}`
	if f.calls != 1 || f.room != "r1" || f.body != want {
		t.Errorf("send got %d calls, room %q, body %s; want 1, r1, %s", f.calls, f.room, f.body, want)
	}
	if f.claims.KeyID != opID || !slices.Equal(f.claims.Scopes, []string{auth.ScopeAdmin}) {
		t.Errorf("claims %+v, want key %d with only the admin scope", f.claims, opID)
	}

	if st, body := pk.confirm(b, map[string]any{"room_id": "r2", "content": "hi"}); st != http.StatusCreated || f.body != `{"content":"hi","content_type":"text/plain"}` {
		t.Errorf("minimal message: %d %s, body sent %s", st, body, f.body)
	}

	s.SendAsOperator = nil
	if st, body := pk.confirm(b, map[string]any{"room_id": "r1", "content": "x"}); st != http.StatusServiceUnavailable || !strings.Contains(string(body), `"sending is not available"`) {
		t.Errorf("without SendAsOperator: %d %s, want 503", st, body)
	}
}

// With the real messages service the confirmed message is stored as the
// operator key's, marked operator.
func TestMessage_StoredByMessagesService(t *testing.T) {
	s, _, b, pk, _ := newMessageUI(t)
	s.SendAsOperator = messages.NewService(s.db, 0, nil).SendAsOperator
	opID, _ := operatorKeyID(t.Context(), s.db)
	if _, err := s.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('r1', 'r1', ?, '2026-11-12T10:00:00Z')`, opID); err != nil {
		t.Fatal(err)
	}
	st, body := pk.confirm(b, map[string]any{"room_id": "r1", "content": "approved", "message_type": "decision"})
	if st != http.StatusCreated || !strings.Contains(string(body), `"operator":true`) {
		t.Fatalf("confirm: %d %s, want 201 with operator true", st, body)
	}
	var author int64
	var operator int
	var content string
	if err := s.db.QueryRow(`SELECT author_key_id, operator, content FROM messages WHERE room_id = 'r1'`).Scan(&author, &operator, &content); err != nil || author != opID || operator != 1 || content != "approved" {
		t.Errorf("stored: author %d operator %d content %q (%v), want %d 1 approved", author, operator, content, err, opID)
	}
}

func TestMessage_BeginNeedsSessionOriginAndRoom(t *testing.T) {
	_, f, b, _, base := newMessageUI(t)
	msg := map[string]any{"room_id": "r1", "content": "x"}
	if st, body := newBrowser(t, base).do("POST", "/ui/auth/message/begin", msg); st != http.StatusUnauthorized {
		t.Errorf("begin without a session: %d %s, want 401", st, body)
	}
	resp := b.send("POST", "/ui/auth/message/begin", msg, map[string]string{"Origin": "http://attacker.example"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("begin from another origin: %d, want 403", resp.StatusCode)
	}
	if st, body := b.do("POST", "/ui/auth/message/begin", map[string]any{"content": "x"}); st != http.StatusBadRequest || !strings.Contains(string(body), `"room_id is required"`) {
		t.Errorf("begin without room_id: %d %s, want 400", st, body)
	}
	if f.calls != 0 {
		t.Errorf("send called %d times", f.calls)
	}
}

// An assertion without user verification posts nothing and is audited.
func TestMessage_WithoutUserVerificationRejected(t *testing.T) {
	s, f, b, pk, _ := newMessageUI(t)
	pk.auth.Options.UserNotVerified = true
	if st, body := pk.confirm(b, map[string]any{"room_id": "r1", "content": "x"}); st != http.StatusUnauthorized || !strings.Contains(string(body), `"passkey not accepted"`) {
		t.Errorf("finish without UV: %d %s, want 401 passkey not accepted", st, body)
	}
	if f.calls != 0 {
		t.Errorf("send called %d times", f.calls)
	}
	var result, detail string
	if err := s.db.QueryRow(`SELECT result, detail FROM audit WHERE action = 'message.send'`).Scan(&result, &detail); err != nil || result != "error:rejected" || !strings.Contains(detail, "reason") {
		t.Errorf("audit: %q %q (%v), want message.send error:rejected with a reason", result, detail, err)
	}
}

// Every refused finish posts nothing; the ceremony is used up by the
// first finish, refused or not.
func TestMessage_FinishRefusals(t *testing.T) {
	_, f, b, pk, base := newMessageUI(t)
	other := newBrowser(t, base)
	if st, body := pk.login(other); st != http.StatusOK {
		t.Fatalf("second session: %d %s", st, body)
	}
	msg := map[string]any{"room_id": "r1", "content": "x"}
	begin := func() (http.Cookie, func() (int, []byte)) {
		t.Helper()
		opts, st, body := pk.beginAssertion(b, "message", msg)
		if opts == nil {
			t.Fatalf("begin: %d %s", st, body)
		}
		return *b.cookies[ceremonyCookie], func() (int, []byte) { return pk.finishAssertion(b, "message", opts) }
	}
	refused := func(name string, st int, body []byte, want int, msg string) {
		t.Helper()
		if st != want || !strings.Contains(string(body), `"`+msg+`"`) {
			t.Errorf("%s: %d %s, want %d %q", name, st, body, want, msg)
		}
	}
	const noCeremony = "no message confirmation in progress, or it took longer than 5 minutes"

	// Another live session cannot finish it; the refusal uses it up.
	ck, finish := begin()
	own := b.session
	b.session = other.session
	st, body := finish()
	b.session = own
	refused("finish from another session", st, body, http.StatusForbidden, "this confirmation was started in another session")
	b.cookies[ceremonyCookie] = &ck
	st, body = finish()
	refused("retry after the refusal", st, body, http.StatusBadRequest, noCeremony)

	// A finished confirmation cannot be finished again.
	ck, finish = begin()
	if st, body := finish(); st != http.StatusCreated {
		t.Fatalf("finish: %d %s", st, body)
	}
	b.cookies[ceremonyCookie] = &ck
	st, body = finish()
	refused("second finish", st, body, http.StatusBadRequest, noCeremony)

	// A login ceremony does not confirm a message.
	opts, st, body := pk.beginAssertion(b, "login", nil)
	if opts == nil {
		t.Fatalf("login begin: %d %s", st, body)
	}
	st, body = pk.finishAssertion(b, "message", opts)
	refused("message finish with a login ceremony", st, body, http.StatusBadRequest, noCeremony)

	// The session that began it ended.
	_, finish = begin()
	if st, _ := b.do("POST", "/ui/auth/logout", nil); st != http.StatusNoContent {
		t.Fatalf("logout: %d", st)
	}
	st, body = finish()
	refused("finish after logout", st, body, http.StatusUnauthorized, "session expired: log in again")

	if f.calls != 1 {
		t.Errorf("send called %d times, want 1 (the one accepted finish)", f.calls)
	}
}
