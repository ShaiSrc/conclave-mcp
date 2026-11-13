package federation

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// runCmd runs args against a server answering status and body, and
// returns stdout and the request line and body the server saw.
func runCmd(t *testing.T, status int, body string, args ...string) (out, req string, err error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		req = r.Method + " " + r.URL.RequestURI() + " " + b.String()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	old := openClient
	openClient = func(context.Context, *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = old }()
	var stdout bytes.Buffer
	cmd := NewCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	err = cmd.ExecuteContext(context.Background())
	return stdout.String(), req, err
}

func TestStatus(t *testing.T) {
	out, req, err := runCmd(t, 200, `{"domain":"a.example","mode":"open","fingerprint":"SHA256:f","key_id":"ed25519:k","peers":{"allowed":2,"blocked":1,"seen":0}}`, "status")
	if err != nil || !strings.HasPrefix(req, "GET /v1/federation ") ||
		!strings.Contains(out, "a.example") || !strings.Contains(out, "2 allowed, 1 blocked, 0 seen") {
		t.Errorf("err=%v req=%q out=%q", err, req, out)
	}
}

func TestPeersList(t *testing.T) {
	body := `{"peers":[{"domain":"b.example","status":"allowed","fingerprint":"SHA256:x","note":"n","added_at":"t","last_seen_at":null}]}`
	out, _, err := runCmd(t, 200, body, "peers", "list")
	if err != nil || !strings.Contains(out, "DOMAIN") || !strings.Contains(out, "b.example") {
		t.Errorf("table: err=%v out=%q", err, out)
	}
	out, _, err = runCmd(t, 200, body, "peers", "list", "--format", "json")
	if err != nil || !strings.Contains(out, `"domain": "b.example"`) {
		t.Errorf("json: err=%v out=%q", err, out)
	}
}

func TestPeersAdd_TwoSteps(t *testing.T) {
	out, req, err := runCmd(t, 200, `{"domain":"b.example","fingerprint":"SHA256:abc","key_id":"k"}`, "peers", "add", "b.example")
	want := "fingerprint of b.example: SHA256:abc\ncompare it with the peer's operator, then run: conclave-mcp federation peers add b.example --fingerprint SHA256:abc\n"
	if err != nil || out != want || !strings.HasPrefix(req, "GET /v1/federation/lookup?domain=b.example ") {
		t.Errorf("lookup: err=%v req=%q out=%q", err, req, out)
	}
	out, req, err = runCmd(t, 201, `{}`, "peers", "add", "b.example", "--fingerprint", "SHA256:abc", "--note", "hi")
	if err != nil || !strings.HasPrefix(req, "POST /v1/federation/peers ") ||
		!strings.Contains(req, `"fingerprint":"SHA256:abc"`) || !strings.Contains(req, `"note":"hi"`) || !strings.Contains(out, "added") {
		t.Errorf("add: err=%v req=%q out=%q", err, req, out)
	}
}

func TestPeersBlockRemovePing(t *testing.T) {
	if _, req, err := runCmd(t, 200, `{}`, "peers", "block", "b.example"); err != nil || !strings.HasPrefix(req, "POST /v1/federation/peers/b.example/block ") {
		t.Errorf("block: err=%v req=%q", err, req)
	}
	if _, req, err := runCmd(t, 204, ``, "peers", "remove", "b.example"); err != nil || !strings.HasPrefix(req, "DELETE /v1/federation/peers/b.example ") {
		t.Errorf("remove: err=%v req=%q", err, req)
	}
	out, req, err := runCmd(t, 200, `{"domain":"b.example","rtt_ms":7}`, "peers", "ping", "b.example")
	if err != nil || !strings.HasPrefix(req, "POST /v1/federation/peers/b.example/ping ") || !strings.Contains(out, "7 ms") {
		t.Errorf("ping: err=%v req=%q out=%q", err, req, out)
	}
}

func TestAPIErrorExitCode(t *testing.T) {
	_, _, err := runCmd(t, 404, `{"error":{"code":"federation_disabled","message":"off"}}`, "status")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "federation_disabled") {
		t.Errorf("err=%v code=%d", err, ExitCode(err))
	}
}
