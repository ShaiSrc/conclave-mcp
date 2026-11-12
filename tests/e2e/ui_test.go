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
// session cookie, the cookie reaches an admin endpoint as the operator
// key, and forged cross-site requests are refused.
func TestE2E_WebUI(t *testing.T) {
	env, admin := bootstrapKeyFor(t)
	port := strconv.Itoa(portFromAddr(env.addr))
	host, origin := "localhost:"+port, "http://localhost:"+port
	cookies := map[string]string{}
	// do sends a request as the UI page would: the localhost Host, the
	// UI's Origin on writes, the X-Conclave-UI header, the cookies so far.
	do := func(method, path, body string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, env.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conclave-UI", "1")
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
		if resp, body := do("POST", "/ui/auth/register/finish", virtualwebauthn.CreateAttestationResponse(rp, pk, cred, *opts), nil); resp.StatusCode != 201 || cookies["conclave_ui"] == "" {
			t.Fatalf("register finish: %d %s", resp.StatusCode, body)
		}
		return pk, cred
	}
	// login runs a discoverable passkey login; it sets the cookie.
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
		if resp, body := do("POST", "/ui/auth/login/finish", virtualwebauthn.CreateAssertionResponse(rp, pk, cred, *opts), nil); resp.StatusCode != 204 || cookies["conclave_ui"] == "" {
			t.Fatalf("login finish: %d %s", resp.StatusCode, body)
		}
	}
	// operatorID asks /v1/whoami with the cookie who the UI acts as.
	operatorID := func() int64 {
		t.Helper()
		resp, body := do("GET", "/v1/whoami", "", nil)
		var me struct {
			ID    int64  `json:"id"`
			Label string `json:"label"`
			Admin bool   `json:"admin"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &me) != nil || me.Label != "operator" || !me.Admin || me.ID == 0 {
			t.Fatalf("whoami with the cookie: %d %s", resp.StatusCode, body)
		}
		return me.ID
	}

	pk, cred := setupAndRegister()
	firstOperator := operatorID()

	// The cookie reaches admin endpoints as the operator key.
	if resp, body := do("GET", "/v1/keys", "", nil); resp.StatusCode != 200 {
		t.Errorf("keys with the cookie: %d %s", resp.StatusCode, body)
	}
	if resp, body := do("POST", "/v1/rooms", `{"name":"e2e-ui-room"}`, nil); resp.StatusCode != 201 {
		t.Errorf("create a room with the cookie: %d %s", resp.StatusCode, body)
	}

	// Forged cross-site requests carry the cookie but fail the checks.
	for _, c := range []struct {
		name, host string
		hdr        map[string]string
		want       int
	}{
		{"another origin", "localhost:" + port, map[string]string{"Origin": "http://attacker.example"}, 403},
		{"no X-Conclave-UI header", "localhost:" + port, map[string]string{"X-Conclave-UI": ""}, 401},
		{"rebound Host", "attacker.example:" + port, map[string]string{"Origin": "http://attacker.example:" + port}, 403},
	} {
		host = c.host
		if resp, body := do("POST", "/v1/rooms", `{"name":"forged"}`, c.hdr); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, resp.StatusCode, body, c.want)
		}
	}
	host = "localhost:" + port
	if r := env.call("GET", "/v1/rooms", admin.Plaintext, nil); r.Status != 200 || strings.Contains(string(r.Body), "forged") {
		t.Errorf("a forged request created a room: %d %s", r.Status, r.Body)
	}

	// Logout ends the session on the server.
	session := cookies["conclave_ui"]
	if resp, _ := do("POST", "/ui/auth/logout", "", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	cookies["conclave_ui"] = session
	if resp, _ := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("whoami after logout: %d, want 401", resp.StatusCode)
	}

	// A passkey login works again after logout.
	login(pk, cred)
	if id := operatorID(); id != firstOperator {
		t.Errorf("operator id after login %d, want %d", id, firstOperator)
	}

	// admin ui-reset (server stopped), restart: the old session is dead.
	env.cancel()
	_ = env.cmd.Wait()
	reset := exec.Command(env.BinaryPath(), "admin", "ui-reset", "--db", filepath.Join(env.DataDir(), "data.db"))
	reset.Env = []string{"PATH=" + os.Getenv("PATH")}
	if out, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("admin ui-reset: %v\n%s", err, out)
	}
	restartServer(t, env, port)
	if resp, _ := do("GET", "/v1/whoami", "", nil); resp.StatusCode != 401 {
		t.Errorf("old session after reset: %d, want 401", resp.StatusCode)
	}
	delete(cookies, "conclave_ui")

	// Set up again: a new passkey, a new operator key.
	pk2, cred2 := setupAndRegister()
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
