package messages

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Write appends p under the lock.
func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

// String returns what was written so far.
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// watchAPI: two old messages. Once watch is subscribed, #3 (agent-b's
// question) and #4 (our alert) are stored, but the stream carries
// only #4's event, as when the server drops events for a slow reader.
func watchAPI(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	stored := []string{
		`{"id":1,"author_key_id":2,"ts":"2026-11-11T10:00:00Z","message_type":"question","content":"old one"}`,
		`{"id":2,"author_key_id":1,"ts":"2026-11-11T10:01:00Z","content":"old two"}`,
	}
	tsOf := func(m string) string { return m[strings.Index(m, `"ts":"`)+6 : strings.Index(m, `"ts":"`)+26] }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":1,"label":"agent-a"},{"id":2,"label":"agent-b"}]}`)
		case "/v1/rooms/" + threadRoom + "/messages":
			since := r.URL.Query().Get("since")
			mu.Lock()
			var out []string
			for _, m := range stored {
				if tsOf(m) > since {
					out = append(out, m)
				}
			}
			mu.Unlock()
			fmt.Fprintf(w, `{"messages":[%s]}`, strings.Join(out, ","))
		case "/v1/rooms/" + threadRoom + "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(200 * time.Millisecond) // after watch's catch-up read
			mu.Lock()
			if len(stored) == 2 {
				stored = append(stored,
					`{"id":3,"author_key_id":2,"ts":"2026-11-11T10:02:00Z","message_type":"question","content":"Port\nwhich one?","metadata":{"reply_to":2}}`,
					`{"id":4,"author_key_id":1,"ts":"2026-11-11T10:03:00Z","message_type":"alert","content":"mine"}`)
			}
			mu.Unlock()
			fmt.Fprint(w, "event: message.created\ndata: {\"id\":4,\"author_key_id\":1,\"ts\":\"2026-11-11T10:03:00Z\"}\n\n")
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
	notify = func(_ context.Context, title, body string) error {
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

func TestNotify_IsBounded(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"osascript", "notify-send"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := notifyTimeout
	notifyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { notifyTimeout = old })
	start := time.Now()
	if err := notify(context.Background(), "t", "b"); err == nil {
		t.Error("want an error from a notifier that hangs")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("notify blocked for %v", took)
	}
}
