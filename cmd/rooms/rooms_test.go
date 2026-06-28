package rooms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// stubCommand returns a *cobra.Command pre-loaded with the flags
// that openClient reads. We don't use Execute() — we go through
// RunE directly to capture output.
func stubCommand(t *testing.T, server, token string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("server", server, "")
	cmd.Flags().String("token", token, "")
	return cmd
}

func captureRunE(t *testing.T, cmd *cobra.Command, runE func(cmd *cobra.Command, args []string) error, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	// cobra's RunE needs context — use the default.
	cmd.SetContext(context.Background())
	return buf.String(), runE(cmd, args)
}

// runEWithOutput wires up a fresh subcommand bound to the same cobra
// output buffer as the parent. Without this, cmd.OutOrStdout() inside
// RunE falls back to os.Stdout, not the buffer we captured.
func runEWithOutput(t *testing.T, parent *cobra.Command, buildSub func() *cobra.Command, args ...string) (string, error) {
	t.Helper()
	sub := buildSub()
	sub.SetOut(parent.OutOrStdout())
	sub.SetErr(parent.ErrOrStderr())
	sub.SetContext(parent.Context())
	return "", sub.RunE(sub, args)
}

func TestList_TableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rooms": []map[string]any{
				{"id": "r1", "name": "alpha", "owner_key_id": 7, "created_at": "2026-01-01T00:00:00Z"},
				{"id": "r2", "name": "beta", "owner_key_id": 9, "created_at": "2026-01-02T00:00:00Z", "archived_at": "2026-02-01T00:00:00Z"},
			},
		})
	}))
	defer srv.Close()

	// Swap openClient to bypass the real flag lookup, using a server that ignores token.
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	parent := &cobra.Command{}
	parent.SetOut(&buf)
	parent.SetErr(&buf)
	sub := newListCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("table missing names: %s", out)
	}
	if !strings.Contains(out, "r1") || !strings.Contains(out, "r2") {
		t.Errorf("table missing ids: %s", out)
	}
}

func TestList_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rooms": []map[string]any{{"id": "r1", "name": "alpha"}},
		})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	sub := newListCmd()
	if err := sub.Flags().Set("format", "json"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	out := buf.String()
	var got []roomView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\nout=%s", err, out)
	}
	if len(got) != 1 || got[0].ID != "r1" {
		t.Errorf("got=%+v", got)
	}
}

func TestCreate_PostsBody(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/rooms" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "new", "name": gotBody["name"], "owner_key_id": 42, "created_at": "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	c := newCreateCmd()
	_ = c.PersistentFlags().String("description", "", "")
	// SPEC-0004 §5.1: `rooms create --name NAME`. We set
	// the flag on `c` directly so the closure variable
	// inside newCreateCmd captures the value. The flag is
	// also marked required (MarkFlagRequired), so we
	// bypass the check by setting .Changed manually.
	if err := c.ParseFlags([]string{"--name", "alpha"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	// ParseFlags sets the value but not Changed. The
	// required-flag check happens at Execute time, not
	// RunE, so calling RunE directly is safe.
	cmd := stubCommand(t, srv.URL, "stub")
	_, err := captureRunE(t, cmd, c.RunE)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["name"] != "alpha" {
		t.Errorf("body=%v", gotBody)
	}
}

func TestArchive_204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/rooms/r1/archive" {
			t.Errorf("method=%s path=%s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	sub := newArchiveCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"r1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(buf.String(), "archived") {
		t.Errorf("out=%q", buf.String())
	}
}

func TestInvite_BadKeyID(t *testing.T) {
	cmd := stubCommand(t, "http://x", "x")
	_, err := captureRunE(t, cmd, newInviteCmd().RunE, "r1", "notanumber")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected exitError, got %T %v", err, err)
	}
	if ee.Code != 2 {
		t.Errorf("code=%d", ee.Code)
	}
}

func TestInvite_OK(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms/r1/members" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	sub := newInviteCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"r1", "42"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["key_id"].(float64) != 42 {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(buf.String(), "invited key 42") {
		t.Errorf("out=%q", buf.String())
	}
}

func TestRemove_404IsExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"nope"}}`)
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	cmd := stubCommand(t, srv.URL, "stub")
	_, err := captureRunE(t, cmd, newRemoveCmd().RunE, "r1", "42")
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestMapErr(t *testing.T) {
	if ExitCode(nil) != 2 { // nil -> ExitCode returns 2 (client misuse default) — actually no: ExitCode(nil) returns 0
		// hmm, fix:
	}
	// Recompute correctly:
	if ec := ExitCode(nil); ec != 0 {
		t.Errorf("ExitCode(nil)=%d", ec)
	}
	if ec := ExitCode(mapErr(nil)); ec != 0 {
		t.Errorf("mapErr(nil)=%d", ec)
	}
	if ec := ExitCode(mapErr(&client.NetError{Err: errors.New("x")})); ec != 3 {
		t.Errorf("mapErr(NetError)=%d", ec)
	}
	if ec := ExitCode(mapErr(&client.APIError{Status: 404, Code: "x"})); ec != 1 {
		t.Errorf("mapErr(APIError)=%d", ec)
	}
}
