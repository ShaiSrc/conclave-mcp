package audit

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/audit") {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{"id": 1, "ts": "2026-01-01T00:00:00Z", "actor": "abc12345", "action": "message.send", "target_type": "room", "target_id": "r1", "request_id": "req-1"},
				{"id": 2, "ts": "2026-01-02T00:00:00Z", "actor": "def67890", "action": "key.create"},
			},
		})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var buf bytes.Buffer
	sub := newListCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(buf.String(), "abc12345") || !strings.Contains(buf.String(), "message.send") {
		t.Errorf("table missing data: %s", buf.String())
	}
}

func TestList_Filters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.RawQuery
		if !strings.Contains(q, "actor=abc") {
			t.Errorf("actor missing: %s", q)
		}
		if !strings.Contains(q, "action=key.create") {
			t.Errorf("action missing: %s", q)
		}
		if !strings.Contains(q, "since=") || !strings.Contains(q, "limit=50") {
			t.Errorf("since/limit missing: %s", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{}})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var buf bytes.Buffer
	sub := newListCmd()
	_ = sub.Flags().Set("actor", "abc")
	_ = sub.Flags().Set("action", "key.create")
	_ = sub.Flags().Set("since", "2026-01-01T00:00:00Z")
	_ = sub.Flags().Set("limit", "50")
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestList_BadSinceIsExit2(t *testing.T) {
	sub := newListCmd()
	if err := sub.Flags().Set("since", "yesterday"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&bytes.Buffer{})
	sub.SetErr(&bytes.Buffer{})
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); ExitCode(err) != 2 {
		t.Fatalf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestList_403IsExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"audit:read scope required"}}`))
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var buf bytes.Buffer
	sub := newListCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); ExitCode(err) != 1 {
		t.Fatalf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{{"id": 1, "action": "x"}},
		})
	}))
	defer srv.Close()
	stubOC(t, srv.URL)

	var buf bytes.Buffer
	sub := newListCmd()
	_ = sub.Flags().Set("format", "json")
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got []entry
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v", err)
	}
}
