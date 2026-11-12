package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// runSetup runs ui setup against a stub server and returns stdout,
// stderr, the stub's port and the error.
func runSetup(t *testing.T, h http.HandlerFunc) (string, string, string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := openClient
	openClient = func(*cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "admin"})
	}
	t.Cleanup(func() { openClient = old })
	var out, errOut bytes.Buffer
	cmd := newSetupCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	u, _ := url.Parse(srv.URL)
	return out.String(), errOut.String(), u.Port(), err
}

func TestSetup_PrintsTheLocalhostURL(t *testing.T) {
	out, errOut, port, err := runSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/ui/setup-tokens" || r.Header.Get("Authorization") != "Bearer admin" {
			t.Errorf("request %s %s (%q)", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"abc_DEF-123","expires_at":"2026-11-12T10:10:00.000000000Z"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://localhost:" + port + "/ui/#setup=abc_DEF-123\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
	if !strings.Contains(errOut, "works once") {
		t.Errorf("stderr: %q", errOut)
	}
}

func TestSetup_ServerRefusal(t *testing.T) {
	_, _, _, err := runSetup(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"a passkey is registered"}}`))
	})
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "a passkey is registered") {
		t.Errorf("err %v (exit %d), want the server's message and exit 1", err, ExitCode(err))
	}
}
