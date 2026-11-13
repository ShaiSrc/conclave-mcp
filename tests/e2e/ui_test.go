package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
)

// TestE2E_WebUI: the real binary serves /ui, ui setup prints a working
// setup URL, a passkey registered with a software authenticator gets a
// session token, the token in X-Conclave-UI reads admin endpoints as
// the operator key and changes them only through a passkey
// confirmation, and forged requests (no header, the token only as a
// cookie, another Origin or Host) are refused.
func TestE2E_WebUI(t *testing.T) {
	env, admin := bootstrapKeyFor(t)
	port := strconv.Itoa(portFromAddr(env.addr))
	host, origin := "localhost:"+port, "http://localhost:"+port
	cookies := map[string]string{} // the ceremony cookie
	session := ""                  // the token from the last finish, held like the page holds it
	// do sends a request as the UI page would: the localhost Host, the
	// UI's Origin on writes, the session token in X-Conclave-UI, the
	// cookies so far.
	do := func(method, path, body string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, env.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("X-Conclave-UI", session)
		}
		if method != "GET" {
			req.Header.Set("Origin", origin)
		}
		for k, v := range hdr {
			if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
		for k, v := range cookies {
			req.AddCookie(&http.Cookie{Name: k, Value: v})
		}
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(cookies, c.Name)
			} else {
				cookies[c.Name] = c.Value
			}
		}
		return resp, string(b)
	}
	// keepSession takes the session token from a finish response.
	keepSession := func(body string) bool {
		var r struct {
			Session string `json:"session"`
		}
		if json.Unmarshal([]byte(body), &r) != nil || r.Session == "" {
			return false
		}
		session = r.Session
		return true
	}

	// The pages, with the CSP.
	resp, body := do("GET", "/ui/", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `src="app.js"`) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("/ui/: %d %q\n%s", resp.StatusCode, resp.Header.Get("Content-Security-Policy"), body)
	}
	if resp, _ := do("GET", "/ui/app.js", "", nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("/ui/app.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "Conclave", Origin: origin}
	// setupAndRegister runs ui setup with the admin key, then registers a
	// passkey with the printed token: the response logs us in.
	setupAndRegister := func() (virtualwebauthn.Authenticator, virtualwebauthn.Credential) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, env.BinaryPath(), "ui", "setup")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CONCLAVE_API_KEY=" + admin.Plaintext, "CONCLAVE_SERVER=" + env.URL()}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ui setup: %v\n%s", err, stderr.String())
		}
		prefix := origin + "/ui/#setup="
		setupURL := strings.TrimSpace(stdout.String())
		if !strings.HasPrefix(setupURL, prefix) {
			t.Fatalf("ui setup printed %q, want %s<token>", setupURL, prefix)
		}
		token := strings.TrimPrefix(setupURL, prefix)

		pk := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true})
		cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
		resp, body := do("POST", "/ui/auth/register/begin", `{"setup_token":"`+token+`","name":"e2e"}`, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("register begin: %d %s", resp.StatusCode, body)
		}
		opts, err := virtualwebauthn.ParseAttestationOptions(body)
		if err != nil {
			t.Fatal(err)
		}
		pk.Options.UserHandle = []byte(opts.UserID)
		if resp, body := do("POST", "/ui/auth/register/finish", virtualwebauthn.CreateAttestationResponse(rp, pk, cred, *opts), nil); resp.StatusCode != 201 || !keepSession(body) {
			t.Fatalf("register finish: %d %s, want 201 with a session token", resp.StatusCode, body)
		}
		if _, ok := cookies["conclave_ui"]; ok {
			t.Error("register finish set a conclave_ui cookie")
		}
		return pk, cred
	}
	// login runs a discoverable passkey login; it sets the session token.
	login := func(pk virtualwebauthn.Authenticator, cred virtualwebauthn.Credential) {
		t.Helper()
		resp, body := do("POST", "/ui/auth/login/begin", "", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("login begin: %d %s", resp.StatusCode, body)
		}
		opts, err := virtualwebauthn.ParseAssertionOptions(body)
		if err != nil {
			t.Fatal(err)
		}
		if resp, body := do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(rp, pk, cred, *opts), nil); resp.StatusCode != 200 || !keepSession(body) {
			t.Fatalf("login finish: %d %s, want 200 with a session token", resp.StatusCode, body)
		}
	}
	// operatorID asks /v1/whoami with the session token who the UI acts as.
	operatorID := func() int64 {
		t.Helper()
		resp, body := do("GET", "/v1/whoami", "", nil)
		var me struct {
			ID    int64  `json:"id"`
			Label string `json:"label"`
			Admin bool   `json:"admin"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &me) != nil || me.Label != "operator" || !me.Admin || me.ID == 0 {
			t.Fatalf("whoami with the session token: %d %s", resp.StatusCode, body)
		}
		return me.ID
	}

	pk, cred := setupAndRegister()
	firstOperator := operatorID()

	// The session token reaches admin endpoints as the operator key.
	if resp, body := do("GET", "/v1/keys", "", nil); resp.StatusCode != 200 {
		t.Errorf("keys with the session token: %d %s", resp.StatusCode, body)
	}
	// A session alone only reads; a change needs a passkey confirmation
	// of exactly that request.
	const needsConfirm = "changes made with a UI session need a passkey confirmation: make them in the web UI"
	if resp, body := do("POST", "/v1/rooms", `{"name":"e2e-ui-room"}`, nil); resp.StatusCode != 403 || !strings.Contains(body, needsConfirm) {
		t.Errorf("create a room with the session token alone: %d %s, want 403", resp.StatusCode, body)
	}
	resp, body = do("POST", "/ui/auth/confirm/begin", `{"method":"POST","path":"/v1/rooms","body":{"name":"e2e-ui-room"}}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("confirm begin: %d %s", resp.StatusCode, body)
	}
	change, err := virtualwebauthn.ParseAssertionOptions(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := do("POST", "/ui/auth/confirm/finish", virtualwebauthn.CreateAssertionResponse(rp, pk, cred, *change), nil); resp.StatusCode != 201 || !strings.Contains(body, `"name":"e2e-ui-room"`) {
		t.Errorf("create a room, confirmed: %d %s, want 201", resp.StatusCode, body)
	}

	// Forged requests fail the checks; each is refused for its own
	// reason, not by another check. A process on another localhost port
	// may hold a cookie the browser sent it, never the token.
	for _, c := range []struct {
		name, method, host string
		hdr                map[string]string
		want               int
		msg                string
	}{
		{"another origin", "POST", "localhost:" + port, map[string]string{"Origin": "http://attacker.example"}, 403, "origin not allowed"},
		{"no X-Conclave-UI header", "POST", "localhost:" + port, map[string]string{"X-Conclave-UI": ""}, 401, "Authorization"},
		{"the token only as a cookie", "POST", "localhost:" + port, map[string]string{"X-Conclave-UI": "", "Cookie": "conclave_ui=" + session}, 401, "Authorization"},
		{"the old X-Conclave-UI: 1", "POST", "localhost:" + port, map[string]string{"X-Conclave-UI": "1"}, 401, "X-Conclave-UI"},
		{"rebound Host (POST, right Origin)", "POST", "attacker.example:" + port, nil, 403, "host not allowed"},
		{"rebound Host (GET)", "GET", "attacker.example:" + port, nil, 403, "host not allowed"},
	} {
		host = c.host
		path, body := "/v1/rooms", `{"name":"forged"}`
		if c.method == "GET" {
			path, body = "/v1/whoami", ""
		}
		if resp, b := do(c.method, path, body, c.hdr); resp.StatusCode != c.want || !strings.Contains(b, c.msg) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, resp.StatusCode, b, c.want, c.msg)
		}
	}
	host = "localhost:" + port
	if r := env.call("GET", "/v1/rooms", admin.Plaintext, nil); r.Status != 200 || strings.Contains(string(r.Body), "forged") {
		t.Errorf("a forged request created a room: %d %s", r.Status, r.Body)
	}

	// Operator messages: a session alone cannot post; a passkey
	// confirmation can, and agents see the mark.
	roomResp := env.call("POST", "/v1/rooms", admin.Plaintext, map[string]any{"name": "e2e-operator-msg"})
	var opRoom struct {
		ID string `json:"id"`
	}
	if roomResp.Status != 201 || json.Unmarshal(roomResp.Body, &opRoom) != nil {
		t.Fatalf("create room: %d %s", roomResp.Status, roomResp.Body)
	}
	agent := env.call("POST", "/v1/keys", admin.Plaintext, map[string]any{"label": "e2e-agent", "trust_origin": "internal"})
	var agentKey struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if agent.Status != 201 || json.Unmarshal(agent.Body, &agentKey) != nil {
		t.Fatalf("create agent key: %d %s", agent.Status, agent.Body)
	}
	if r := env.call("POST", "/v1/rooms/"+opRoom.ID+"/invite", admin.Plaintext, map[string]any{"key_id": agentKey.ID, "rights": []string{"all"}}); r.Status != 201 {
		t.Fatalf("invite agent: %d %s", r.Status, r.Body)
	}
	q := env.call("POST", "/v1/rooms/"+opRoom.ID+"/messages", agentKey.Key, map[string]any{"content": "may I deploy?", "message_type": "question"})
	var question struct {
		ID int64 `json:"id"`
	}
	if q.Status != 201 || json.Unmarshal(q.Body, &question) != nil {
		t.Fatalf("agent question: %d %s", q.Status, q.Body)
	}
	msgsPath := "/v1/rooms/" + opRoom.ID + "/messages"
	if resp, b := do("POST", msgsPath, `{"content":"sneaky"}`, nil); resp.StatusCode != 403 || !strings.Contains(b, needsConfirm) {
		t.Errorf("session POST of a message: %d %s, want 403 with the passkey message", resp.StatusCode, b)
	}
	begin := `{"room_id":"` + opRoom.ID + `","content":"yes, deploy","message_type":"answer","reply_to":` + strconv.FormatInt(question.ID, 10) + `}`
	resp, body = do("POST", "/ui/auth/message/begin", begin, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("message begin: %d %s", resp.StatusCode, body)
	}
	confirm, err := virtualwebauthn.ParseAssertionOptions(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := do("POST", "/ui/auth/message/finish", virtualwebauthn.CreateAssertionResponse(rp, pk, cred, *confirm), nil); resp.StatusCode != 201 || !strings.Contains(body, `"operator":true`) {
		t.Fatalf("message finish: %d %s, want 201 with operator true", resp.StatusCode, body)
	}
	var listed struct {
		Messages []struct {
			Content     string `json:"content"`
			Operator    bool   `json:"operator"`
			AuthorKeyID int64  `json:"author_key_id"`
			Metadata    struct {
				ReplyTo int64 `json:"reply_to"`
			} `json:"metadata"`
		} `json:"messages"`
	}
	if r := env.call("GET", msgsPath, agentKey.Key, nil); r.Status != 200 || json.Unmarshal(r.Body, &listed) != nil {
		t.Fatalf("agent list: %d %s", r.Status, r.Body)
	}
	found := false
	for _, m := range listed.Messages {
		if m.Content == "yes, deploy" {
			found = true
			if !m.Operator || m.AuthorKeyID != firstOperator || m.Metadata.ReplyTo != question.ID {
				t.Errorf("operator message: operator=%v author=%d reply_to=%d, want true, %d, %d", m.Operator, m.AuthorKeyID, m.Metadata.ReplyTo, firstOperator, question.ID)
			}
		}
	}
	if !found {
		t.Errorf("agent does not see the operator message: %+v", listed.Messages)
	}
	if resp, b := do("GET", msgsPath, "", nil); resp.StatusCode != 200 || !strings.Contains(b, "yes, deploy") {
		t.Errorf("session GET of messages: %d %s", resp.StatusCode, b)
	}
	inbox := exec.Command(env.BinaryPath(), "messages", "inbox", opRoom.ID, "--state", filepath.Join(t.TempDir(), "cursor"))
	inbox.Env = []string{"PATH=" + os.Getenv("PATH"), "CONCLAVE_API_KEY=" + agentKey.Key, "CONCLAVE_SERVER=" + env.URL()}
	if out, err := inbox.CombinedOutput(); err != nil || !strings.Contains(string(out), "- OPERATOR (confirmed with passkey) [") {
		t.Errorf("inbox: %v\n%s, want a line starting with the operator mark", err, out)
	}

	// Logout ends the session on the server: the same token is dead.
	if resp, _ := do("POST", "/ui/auth/logout", "", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	if resp, _ := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("whoami after logout: %d, want 401", resp.StatusCode)
	}

	// A passkey login works again after logout.
	login(pk, cred)
	if id := operatorID(); id != firstOperator {
		t.Errorf("operator id after login %d, want %d", id, firstOperator)
	}

	// admin ui-reset (server stopped), restart: the old session is dead.
	env.stop()
	reset := exec.Command(env.BinaryPath(), "admin", "ui-reset", "--db", filepath.Join(env.DataDir(), "data.db"))
	reset.Env = []string{"PATH=" + os.Getenv("PATH")}
	if out, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("admin ui-reset: %v\n%s", err, out)
	}
	restartServer(t, env, port)
	if resp, _ := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("old session after reset: %d, want 401", resp.StatusCode)
	}
	oldSession := session
	session = ""

	// Set up again: a new passkey, a new operator key.
	pk2, cred2 := setupAndRegister()
	// A new operator exists now, so a 401 for the old token can only mean
	// the reset deleted its session row.
	newSession := session
	session = oldSession
	if resp, body := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("old session after reset and re-setup: %d %s, want 401", resp.StatusCode, body)
	}
	session = newSession
	// The reset revoked the old operator key and freed its label.
	var keys struct {
		Keys []struct {
			ID        int64  `json:"id"`
			Label     string `json:"label"`
			RevokedAt string `json:"revoked_at"`
		} `json:"keys"`
	}
	if r := env.call("GET", "/v1/keys", admin.Plaintext, nil); r.Status != 200 || json.Unmarshal(r.Body, &keys) != nil {
		t.Fatalf("keys: %d %s", r.Status, r.Body)
	}
	foundOld := false
	for _, k := range keys.Keys {
		if k.ID == firstOperator {
			foundOld = true
			if k.RevokedAt == "" || !strings.HasPrefix(k.Label, "operator-revoked-"+strconv.FormatInt(firstOperator, 10)+"-") {
				t.Errorf("old operator key after reset: %+v, want revoked and renamed", k)
			}
		}
	}
	if !foundOld {
		t.Errorf("old operator key %d missing from /v1/keys", firstOperator)
	}
	if id := operatorID(); id == firstOperator {
		t.Errorf("operator id after re-setup is still %d, want a new key", id)
	}
	if resp, _ := do("POST", "/ui/auth/logout", "", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	login(pk2, cred2)
	operatorID()
}

// restartServer starts the binary again on the same port and database as
// env and waits until it is healthy; it stops with the test.
func restartServer(t *testing.T, env *subprocessEnv, port string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, env.BinaryPath(), "serve", "--host", "127.0.0.1", "--port="+port, "--db", filepath.Join(env.DataDir(), "data.db"))
	cmd.Dir = env.DataDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if resp, err := http.Get(env.URL() + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
	}
	t.Fatalf("restarted server not healthy: %s", out.String())
}
