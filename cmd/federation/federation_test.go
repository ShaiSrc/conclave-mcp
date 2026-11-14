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
	out, req, err := runCmd(t, 200, `{"domain":"a.example","mode":"open","fingerprint":"SHA256:f","key_id":"ed25519:k","peers":{"allowed":2,"blocked":1,"seen":0},
		"keys":[{"key_id":"ed25519:k","fingerprint":"SHA256:f","status":"current"},
		{"key_id":"ed25519:o","fingerprint":"SHA256:o","status":"previous","until":"2026-12-14T10:00:00Z"}]}`, "status")
	if err != nil || !strings.HasPrefix(req, "GET /v1/federation ") ||
		!strings.Contains(out, "a.example") || !strings.Contains(out, "2 allowed, 1 blocked, 0 seen") ||
		!strings.Contains(out, "ed25519:k  SHA256:f  current\n") ||
		!strings.Contains(out, "ed25519:o  SHA256:o  previous until 2026-12-14T10:00:00Z\n") {
		t.Errorf("err=%v req=%q out=%q", err, req, out)
	}
}

func TestRotateKey(t *testing.T) {
	out, req, err := runCmd(t, 200, `{"key_id":"ed25519:n","fingerprint":"SHA256:n"}`, "rotate-key")
	if err != nil || !strings.HasPrefix(req, "POST /v1/federation/keys/rotate ") ||
		!strings.Contains(out, "new key id:  ed25519:n\nfingerprint: SHA256:n\n") || !strings.Contains(out, "30 days") {
		t.Errorf("err=%v req=%q out=%q", err, req, out)
	}
	_, _, err = runCmd(t, 409, `{"error":{"code":"rotation_too_soon","message":"m"}}`, "rotate-key")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "rotation_too_soon") {
		t.Errorf("409: err=%v", err)
	}
}

func TestPeersList(t *testing.T) {
	body := `{"peers":[{"domain":"b.example","status":"allowed","fingerprint":"SHA256:x","note":"n","added_at":"t","last_seen_at":null,
		"last_out_at":"2026-11-14T10:00:00Z","health":"failing"}]}`
	out, _, err := runCmd(t, 200, body, "peers", "list")
	if err != nil || !strings.Contains(out, "DOMAIN") || !strings.Contains(out, "HEALTH") || !strings.Contains(out, "LAST_OUT") ||
		!strings.Contains(out, "b.example") || !strings.Contains(out, "failing") || !strings.Contains(out, "2026-11-14T10:00:00Z") {
		t.Errorf("table: err=%v out=%q", err, out)
	}
	out, _, err = runCmd(t, 200, body, "peers", "list", "--format", "json")
	if err != nil || !strings.Contains(out, `"domain": "b.example"`) || !strings.Contains(out, `"health": "failing"`) {
		t.Errorf("json: err=%v out=%q", err, out)
	}
}

const peerJSON = `{"domain":"b.example","status":"allowed","fingerprint":"SHA256:x","key_id":"ed25519:k","note":"n","added_at":"2026-11-01T00:00:00Z",
	"last_seen_at":"2026-11-14T09:00:00Z","last_out_at":null,"health":"unreachable",
	"delivery":{"rooms":2,"pending":5,"failing_since":"2026-11-02T00:00:00Z","paused":1,"last_error":"boom"},
	"limits":{"requests_per_minute":{"value":120,"override":false},"dm_invites_per_hour":{"value":0,"override":true},
	"follow_requests_per_hour":{"value":100,"override":false}}}`

// wantPeer checks the output of a peer view.
func wantPeer(t *testing.T, what, out string) {
	t.Helper()
	for _, want := range []string{"domain:         b.example\n", "health:         unreachable\n", "last seen:      2026-11-14T09:00:00Z\n",
		"last out:       never\n", "delivery:       2 rooms, 5 events pending, 1 paused\n", "failing since:  2026-11-02T00:00:00Z\n",
		"last error:     boom\n", "  requests_per_minute       120\n", "  dm_invites_per_hour       0 (override)\n",
		"  follow_requests_per_hour  100\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s: no %q in %q", what, want, out)
		}
	}
}

func TestPeersShowRetry(t *testing.T) {
	out, req, err := runCmd(t, 200, peerJSON, "peers", "show", "b.example")
	if err != nil || !strings.HasPrefix(req, "GET /v1/federation/peers/b.example ") {
		t.Errorf("show: err=%v req=%q", err, req)
	}
	wantPeer(t, "show", out)
	out, req, err = runCmd(t, 200, peerJSON, "peers", "retry", "b.example")
	if err != nil || !strings.HasPrefix(req, "POST /v1/federation/peers/b.example/retry ") || !strings.HasPrefix(out, "delivery to b.example is due now\n") {
		t.Errorf("retry: err=%v req=%q out=%q", err, req, out)
	}
	wantPeer(t, "retry", out)
}

func TestPeersLimits(t *testing.T) {
	out, req, err := runCmd(t, 200, peerJSON, "peers", "limits", "b.example", "--requests", "5", "--follow-requests", "0")
	if err != nil || req != `PATCH /v1/federation/peers/b.example {"limits":{"follow_requests_per_hour":0,"requests_per_minute":5}}` {
		t.Errorf("limits: err=%v req=%q", err, req)
	}
	wantPeer(t, "limits", out)
	_, req, err = runCmd(t, 200, peerJSON, "peers", "limits", "b.example", "--dm-invites", "7")
	if err != nil || !strings.Contains(req, `{"limits":{"dm_invites_per_hour":7}}`) {
		t.Errorf("dm: err=%v req=%q", err, req)
	}
	_, req, err = runCmd(t, 200, peerJSON, "peers", "limits", "b.example", "--reset")
	if err != nil || !strings.Contains(req, `{"limits":{"dm_invites_per_hour":null,"follow_requests_per_hour":null,"requests_per_minute":null}}`) {
		t.Errorf("reset: err=%v req=%q", err, req)
	}
	for _, args := range [][]string{{"b.example"}, {"b.example", "--reset", "--requests", "1"}} {
		if _, req, err := runCmd(t, 200, peerJSON, append([]string{"peers", "limits"}, args...)...); ExitCode(err) != 2 || req != "" {
			t.Errorf("%v: err=%v req=%q", args, err, req)
		}
	}
	_, _, err = runCmd(t, 400, `{"error":{"code":"invalid_limit","message":"m"}}`, "peers", "limits", "b.example", "--requests", "-1")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "invalid_limit") {
		t.Errorf("400: err=%v", err)
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
