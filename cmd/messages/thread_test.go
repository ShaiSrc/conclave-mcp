package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

const threadRoom = "01M72DWQRAZXY3X8HWGJ0QZGNY"

// threadAPI: we are key 1 (agent-a). #1 asks, agent-b answers (#2),
// #3 is another topic, we ask back (#4). After release, agent-b
// answers again (#5) and the event stream says so.
func threadAPI(t *testing.T) (url string, release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	released := func() bool {
		select {
		case <-release:
			return true
		default:
			return false
		}
	}
	msgs := `{"id":1,"author_key_id":1,"ts":"t1","content":"Frage?","message_type":"question"},
		{"id":2,"author_key_id":2,"ts":"t2","content":"Antwort","message_type":"answer","metadata":{"reply_to":1}},
		{"id":3,"author_key_id":2,"ts":"t3","content":"anderes Thema"},
		{"id":4,"author_key_id":1,"ts":"t4","content":"Rückfrage","metadata":{"reply_to":2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":1,"label":"agent-a"},{"id":2,"label":"agent-b"}]}`)
		case "/v1/rooms/" + threadRoom + "/messages":
			if released() {
				fmt.Fprintf(w, `{"messages":[%s,
					{"id":5,"author_key_id":2,"ts":"t5","content":"zweite Antwort","metadata":{"reply_to":4}}]}`, msgs)
				return
			}
			fmt.Fprintf(w, `{"messages":[%s]}`, msgs)
		case "/v1/rooms/" + threadRoom + "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
				fmt.Fprint(w, "event: message.created\ndata: {\"id\":5,\"author_key_id\":2,\"ts\":\"t5\"}\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, release
}

func runThread(t *testing.T, url string, args []string, flags map[string]string) (string, error, time.Duration) {
	t.Helper()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: url, Token: "stub"})
	}
	t.Cleanup(func() { openClient = oldOC })
	var out bytes.Buffer
	cmd := newThreadCmd()
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	start := time.Now()
	err := cmd.RunE(cmd, args)
	return out.String(), err, time.Since(start)
}

func TestThread_PrintsTheWholeConversation(t *testing.T) {
	url, _ := threadAPI(t)
	out, err, _ := runThread(t, url, []string{threadRoom, "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#1 agent-a (you) <question>: Frage?", "#2 agent-b <answer> re #1: Antwort", "#4 agent-a (you) re #2: Rückfrage"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "anderes Thema") {
		t.Errorf("another thread leaked in:\n%s", out)
	}
}

func TestThread_RoomFromEnv(t *testing.T) {
	url, _ := threadAPI(t)
	t.Setenv("CONCLAVE_ROOM", threadRoom)
	out, err, _ := runThread(t, url, []string{"3"}, nil)
	if err != nil || !strings.Contains(out, "anderes Thema") {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestThread_JSONCarriesAuthorLabels(t *testing.T) {
	url, _ := threadAPI(t)
	out, err, _ := runThread(t, url, []string{threadRoom, "1"}, map[string]string{"format": "json"})
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		ID       int64  `json:"id"`
		Author   string `json:"author"`
		AuthorID int64  `json:"author_key_id"`
		Type     string `json:"message_type"`
		Meta     struct {
			ReplyTo int64 `json:"reply_to"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if len(got) != 3 || got[1].Author != "agent-b" || got[1].AuthorID != 2 || got[1].Type != "answer" || got[1].Meta.ReplyTo != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestThread_WaitReturnsOnReply(t *testing.T) {
	url, release := threadAPI(t)
	time.AfterFunc(300*time.Millisecond, func() { close(release) })
	out, err, took := runThread(t, url, []string{threadRoom, "1"}, map[string]string{"wait": "10s"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#5 agent-b re #4: zweite Antwort") || !strings.Contains(out, "Frage?") {
		t.Errorf("want the thread with the new reply, got:\n%s", out)
	}
	if took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("took %v: want it to wait for the reply, then return at once", took)
	}
}

func TestThread_WaitReturnsAtOnceWhenTheLastWordIsNotOurs(t *testing.T) {
	url, _ := threadAPI(t)
	out, err, took := runThread(t, url, []string{threadRoom, "3"}, map[string]string{"wait": "10s"})
	if err != nil || !strings.Contains(out, "anderes Thema") {
		t.Fatalf("got %q, %v", out, err)
	}
	if took > 2*time.Second {
		t.Errorf("took %v, want no wait", took)
	}
}

func TestThread_WaitTimesOutSilently(t *testing.T) {
	url, _ := threadAPI(t)
	out, err, took := runThread(t, url, []string{threadRoom, "1"}, map[string]string{"wait": "400ms"})
	if err != nil || out != "" {
		t.Fatalf("timeout: want no output and no error, got %q, %v", out, err)
	}
	if took < 350*time.Millisecond || took > 3*time.Second {
		t.Errorf("took %v, want about 400ms", took)
	}
}

func TestThread_UnknownMessage(t *testing.T) {
	url, _ := threadAPI(t)
	_, err, _ := runThread(t, url, []string{threadRoom, "99"}, nil)
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "#99") {
		t.Errorf("want exit 1 naming #99, got %v (exit %d)", err, ExitCode(err))
	}
	if _, err, _ := runThread(t, url, []string{threadRoom, "x"}, nil); err == nil {
		t.Error("want an error for a non-numeric id")
	}
}

func TestList_TableShowsAuthorTypeReplyAndExcerpt(t *testing.T) {
	long := strings.Repeat("x", 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/keys":
			fmt.Fprint(w, `{"keys":[{"id":2,"label":"agent-b"}]}`)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{
				{"id": 7, "author_key_id": 2, "ts": "t7", "trust_origin": "internal", "message_type": "answer",
					"metadata": map[string]any{"reply_to": 3}, "content": "Zeile eins\n\tZeile zwei \x1b[31mrot"},
				{"id": 8, "author_key_id": 9, "ts": "t8", "trust_origin": "external", "content": long},
			}})
		}
	}))
	defer srv.Close()
	out, err, _ := runList(t, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "FROM") || !strings.Contains(lines[0], "CONTENT") {
		t.Fatalf("want header + 2 rows:\n%s", out)
	}
	for _, want := range []string{"agent-b", "answer", "#3", "Zeile eins Zeile zwei [31mrot"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row 1 missing %q: %q", want, lines[1])
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("control characters reach the terminal: %q", out)
	}
	if !strings.Contains(lines[2], "key 9") || !strings.Contains(lines[2], "external") || !strings.Contains(lines[2], "…") || strings.Contains(lines[2], long) {
		t.Errorf("row 2: want key 9, external and a cut excerpt: %q", lines[2])
	}
}

func TestList_JSONKeepsAllFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/keys" {
			fmt.Fprint(w, `{"keys":[{"id":2,"label":"agent-b"}]}`)
			return
		}
		fmt.Fprint(w, `{"messages":[{"id":7,"author_key_id":2,"ts":"t7","content":"hi","message_type":"answer","metadata":{"reply_to":3}}]}`)
	}))
	defer srv.Close()
	out, err, _ := runList(t, srv.URL, map[string]string{"format": "json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"author_key_id": 2`, `"author": "agent-b"`, `"message_type": "answer"`, `"reply_to": 3`, `"content": "hi"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json missing %s:\n%s", want, out)
		}
	}
}

func runList(t *testing.T, url string, flags map[string]string) (string, error, time.Duration) {
	t.Helper()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: url, Token: "stub"})
	}
	t.Cleanup(func() { openClient = oldOC })
	var out bytes.Buffer
	cmd := newListCmd()
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	start := time.Now()
	err := cmd.RunE(cmd, []string{threadRoom})
	return out.String(), err, time.Since(start)
}

func TestThread_WaitTimesOutSilentlyWhileTheListHangs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/rooms/" + threadRoom + "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default: // the message list never answers
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	out, err, took := runThread(t, srv.URL, []string{threadRoom, "1"}, map[string]string{"wait": "400ms"})
	if err != nil || out != "" {
		t.Fatalf("want a silent timeout, got %q, %v", out, err)
	}
	if took > 3*time.Second {
		t.Errorf("took %v, want about 400ms", took)
	}
}
