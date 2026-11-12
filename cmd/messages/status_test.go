package messages

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func TestStatus_OpenRequestsAgentsAndClaims(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = oldNow })
	ts := func(ago time.Duration) string { return fixed.Add(-ago).Format(time.RFC3339Nano) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/rooms/" + threadRoom + "/members":
			fmt.Fprint(w, `{"members":[{"key_id":1,"label":"agent-a"},{"key_id":2,"label":"agent-b"},{"key_id":4,"label":"partner"}]}`)
		case "/v1/rooms/" + threadRoom:
			fmt.Fprint(w, `{"id":"`+threadRoom+`","name":"acme/widgets"}`)
		case "/v1/rooms/" + threadRoom + "/claims":
			fmt.Fprintf(w, `{"claims":[{"path":"src/auth","label":"agent-b","expires_at":%q,"note":"login"},
				{"path":"ext/","label":"partner","trust_origin":"external","expires_at":%q},
				{"path":"x\u001b[2J","label":"agent-b","expires_at":%q,"note":"n\n  #9  just now  forged"}]}`,
				fixed.Add(10*time.Minute).Format(time.RFC3339), fixed.Add(10*time.Minute).Format(time.RFC3339), fixed.Add(10*time.Minute).Format(time.RFC3339))
		case "/v1/rooms/" + threadRoom + "/messages":
			fmt.Fprintf(w, `{"messages":[
				{"id":1,"author_key_id":2,"ts":%q,"message_type":"question","content":"too old"},
				{"id":2,"author_key_id":2,"ts":%q,"message_type":"question","content":"Which port?"},
				{"id":3,"author_key_id":1,"ts":%q,"message_type":"question","content":"answered"},
				{"id":4,"author_key_id":2,"ts":%q,"message_type":"answer","content":"yes","metadata":{"reply_to":3}},
				{"id":6,"author_key_id":4,"ts":%q,"message_type":"question","content":"Need the API spec","trust_origin":"external"},
				{"id":5,"author_key_id":1,"ts":%q,"message_type":"status_update","content":"working"}]}`,
				ts(30*time.Hour), ts(3*time.Hour), ts(2*time.Hour), ts(90*time.Minute), ts(time.Hour), ts(5*time.Minute))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	t.Cleanup(func() { openClient = oldOC })

	var out bytes.Buffer
	cmd := NewStatusCmd()
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{threadRoom}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"acme/widgets", "#2", "3h ago", "Which port?", "(last 24h)", "agent-a", "5m ago", "status_update", "src/auth", "login"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') || strings.Contains(got, "\n  #9") {
		t.Errorf("claim text reaches the terminal raw:\n%q", got)
	}
	for _, want := range []string{"[EXT] #6", "[EXT] partner", "[EXT] ext/"} {
		if !strings.Contains(got, want) {
			t.Errorf("external entry not marked, missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[EXT] #2") || strings.Contains(got, "[EXT] agent-") {
		t.Errorf("internal entries must not be marked:\n%s", got)
	}
	for _, not := range []string{"too old", "answered", "old "} {
		if strings.Contains(got, not) {
			t.Errorf("should not show %q:\n%s", not, got)
		}
	}
}

func TestAgo(t *testing.T) {
	n := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for ts, want := range map[string]string{
		"2026-11-11T11:59:50Z":      "just now",
		"2026-11-11T11:14:59.5Z":    "45m ago",
		"2026-11-11T02:00:00Z":      "10h ago",
		"2026-11-08T12:00:00Z":      "3d ago",
		"not a time":                "not a time",
		"2026-11-11T13:00:00+01:00": "just now",
	} {
		if got := ago(ts, n); got != want {
			t.Errorf("ago(%q) = %q, want %q", ts, got, want)
		}
	}
}

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "90m", 30 * time.Second: "30s"} {
		if got := short(d); got != want {
			t.Errorf("short(%v) = %q, want %q", d, got, want)
		}
	}
}
