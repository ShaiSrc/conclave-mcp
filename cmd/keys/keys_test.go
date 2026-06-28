package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func stubOC(t *testing.T, srvURL string) {
	t.Helper()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srvURL, Token: "stub"})
	}
	t.Cleanup(func() { openClient = oldOC })
}

func TestCreate_PlaintextToStdout_StatusToStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "key": "tk_secret_xyz", "label": "ci-deploy",
		})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout, stderr bytes.Buffer
	sub := newCreateCmd()
	if err := sub.Flags().Set("label", "ci-deploy"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	// The plaintext key MUST end up on STDOUT.
	if got := strings.TrimSpace(stdout.String()); got != "tk_secret_xyz" {
		t.Errorf("stdout=%q, want 'tk_secret_xyz'", got)
	}
	// The status message MUST be on STDERR, not on STDOUT.
	if strings.Contains(stdout.String(), "created") || strings.Contains(stdout.String(), "store the") {
		t.Errorf("status leaked to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "created") {
		t.Errorf("status missing from stderr: %q", stderr.String())
	}
}

func TestCreate_JSONFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "key": "tk_abc"})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout, stderr bytes.Buffer
	sub := newCreateCmd()
	if err := sub.Flags().Set("format", "json"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got createResponse
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v\nout=%s", err, stdout.String())
	}
	if got.Key != "tk_abc" || got.ID != 42 {
		t.Errorf("got=%+v", got)
	}
}

func TestList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"id": 1, "prefix": "abc12345", "label": "one", "created_at": "2026-01-01T00:00:00Z"},
				{"id": 2, "prefix": "def67890", "label": "two", "created_at": "2026-01-02T00:00:00Z", "revoked_at": "2026-02-01T00:00:00Z"},
			},
		})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout bytes.Buffer
	sub := newListCmd()
	sub.SetOut(&stdout)
	sub.SetErr(&stdout)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(stdout.String(), "abc12345") || !strings.Contains(stdout.String(), "def67890") {
		t.Errorf("table missing prefixes: %s", stdout.String())
	}
	// CRUCIAL: list MUST NOT contain a "key" or "hash" column.
	if strings.Contains(stdout.String(), "tk_") || strings.Contains(stdout.String(), "hash") {
		t.Errorf("plaintext/hash leaked: %s", stdout.String())
	}
}

func TestList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"id": 1, "prefix": "abc"}},
		})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout bytes.Buffer
	sub := newListCmd()
	if err := sub.Flags().Set("format", "json"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&stdout)
	sub.SetErr(&stdout)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got []keyView
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if len(got) != 1 || got[0].Prefix != "abc" {
		t.Errorf("got=%+v", got)
	}
}

func TestRevoke_WithYes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/v1/keys/42" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout, stderr bytes.Buffer
	sub := newRevokeCmd()
	if err := sub.Flags().Set("yes", "true"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"42"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Errorf("stdout=%q", stdout.String())
	}
	// No prompt should be printed when --yes is set.
	if strings.Contains(stderr.String(), "?") {
		t.Errorf("prompt leaked: %q", stderr.String())
	}
}

func TestRevoke_AbortOnN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be called when user aborts")
	}))
	defer srv.Close()
	// (no stubOC — we expect no HTTP call)

	ResetStdin(strings.NewReader("n\n"))
	defer ResetStdin(nil)

	var stdout, stderr bytes.Buffer
	sub := newRevokeCmd()
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	err := sub.RunE(sub, []string{"42"})
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
	if !strings.Contains(stderr.String(), "aborted") {
		t.Errorf("missing 'aborted' in stderr: %q", stderr.String())
	}
}

func TestRevoke_ProceedOnY(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method=%s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	ResetStdin(strings.NewReader("y\n"))
	defer ResetStdin(nil)

	var stdout, stderr bytes.Buffer
	sub := newRevokeCmd()
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"42"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Errorf("stdout=%q", stdout.String())
	}
}

func TestRevoke_404IsExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"nope"}}`))
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var stdout, stderr bytes.Buffer
	sub := newRevokeCmd()
	if err := sub.Flags().Set("yes", "true"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"999"}); ExitCode(err) != 1 {
		t.Fatalf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

// TestCreate_TrustOriginExternalForwarded asserts that
// --trust-origin=external is forwarded in the POST body to
// the server (SPEC-0001 Q11, ADR-0008 revision).
func TestCreate_TrustOriginExternalForwarded(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"key":"tk_x","label":"ext","trust_origin":"external"}`))
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	sub := newCreateCmd()
	_ = sub.Flags().Set("label", "ext")
	_ = sub.Flags().Set("trust-origin", "external")
	_ = sub.Flags().Set("format", "json")
	var stdout, stderr bytes.Buffer
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(capturedBody, `"trust_origin":"external"`) {
		t.Errorf("body did not include trust_origin=external: %s", capturedBody)
	}
	// Plaintext still on stdout.
	if !strings.Contains(stdout.String(), "tk_x") {
		t.Errorf("stdout missing key: %s", stdout.String())
	}
}

// TestCreate_TrustOriginInvalidRejected asserts that
// --trust-origin=garbage fails fast with a friendly error
// (no network round-trip).
func TestCreate_TrustOriginInvalidRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called when --trust-origin is invalid")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	sub := newCreateCmd()
	_ = sub.Flags().Set("trust-origin", "garbage")
	var stdout, stderr bytes.Buffer
	sub.SetOut(&stdout)
	sub.SetErr(&stderr)
	sub.SetContext(context.Background())
	err := sub.RunE(sub, nil)
	if err == nil {
		t.Fatalf("expected error for invalid trust_origin")
	}
	if !strings.Contains(err.Error(), "trust-origin") {
		t.Errorf("err=%v does not mention trust-origin", err)
	}
}
