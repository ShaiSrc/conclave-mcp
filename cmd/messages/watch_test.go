package messages

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// syncBuf is a bytes.Buffer safe to read while watch writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// watchAPI: two old messages; the event stream then delivers a
// question from agent-b (#3) and a status update from us (#4).
func watchAPI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":1,"label":"agent-a"},{"id":2,"label":"agent-b"}]}`)
		case "/v1/rooms/" + threadRoom + "/messages":
			if r.URL.Query().Get("since") != "" {
				fmt.Fprint(w, `{"messages":[]}`)
				return
			}
			fmt.Fprint(w, `{"messages":[
				{"id":1,"author_key_id":2,"ts":"2026-11-11T10:00:00Z","message_type":"question","content":"old one"},
				{"id":2,"author_key_id":1,"ts":"2026-11-11T10:01:00Z","content":"old two"}]}`)
		case "/v1/rooms/" + threadRoom + "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ok\n\n")
			// #2 again: already shown, must not repeat.
			fmt.Fprint(w, "event: message.created\ndata: {\"id\":2,\"author_key_id\":1,\"ts\":\"2026-11-11T10:01:00Z\",\"content\":\"old two\"}\n\n")
			fmt.Fprint(w, "event: message.created\ndata: {\"id\":3,\"author_key_id\":2,\"ts\":\"2026-11-11T10:02:00Z\",\"message_type\":\"question\",\"content\":\"Port\\nwhich one?\",\"metadata\":{\"reply_to\":2}}\n\n")
			fmt.Fprint(w, "event: message.created\ndata: {\"id\":4,\"author_key_id\":1,\"ts\":\"2026-11-11T10:03:00Z\",\"message_type\":\"alert\",\"content\":\"mine\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWatch_BacklogThenLiveWithNotifications(t *testing.T) {
	url := watchAPI(t)
	oldOC, oldNotify, oldCan := openClient, notify, canNotify
	canNotify = func() error { return nil }
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: url, Token: "stub"})
	}
	var mu sync.Mutex
	var notes []string
	notify = func(title, body string) error {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, title+" | "+body)
		return nil
	}
	t.Cleanup(func() { openClient, notify, canNotify = oldOC, oldNotify, oldCan })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	cmd := NewWatchCmd()
	_ = cmd.Flags().Set("last", "1")
	_ = cmd.Flags().Set("notify", "true")
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- cmd.RunE(cmd, []string{threadRoom}) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "#4") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "old one") {
		t.Errorf("--last 1 should show only the newest old message:\n%s", got)
	}
	if strings.Count(got, "old two") != 1 {
		t.Errorf("#2 should appear once:\n%s", got)
	}
	for _, want := range []string{"#3 agent-b <question> re #2: Port\n    which one?", "#4 agent-a (you) <alert>: mine"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "agent-b") || !strings.Contains(notes[0], "question") || !strings.Contains(notes[0], "Port which one?") {
		t.Errorf("want one notification for #3 only, got %q", notes)
	}
}
