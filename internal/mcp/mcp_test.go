package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServer_Initialize(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var out, errOut bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out, &errOut); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected error log: %s", errOut.String())
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("response not JSON: %v\n%s", err, out.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: %q", resp.JSONRPC)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Error("result is nil")
	}
}

func TestServer_ToolsList(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	_ = s.ServeStdio(context.Background(), in, &out, &bytes.Buffer{})
	var resp jsonRPCResponse
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp)
	if resp.Error != nil {
		t.Fatalf("err: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not map: %T", resp.Result)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools not array: %T", result["tools"])
	}
	// 13 tools: 11 original + rooms.invite, rooms.remove
	// (ADR-0007 §Open questions, resolved 2026-06-28). If a
	// future commit adds or removes a tool, the message
	// below makes the drift obvious.
	if len(tools) != 13 {
		t.Errorf("tool count: got %d, want 13 (rooms.list, rooms.create, rooms.delete, rooms.invite, rooms.remove, messages.send, messages.list, messages.subscribe, messages.delete, keys.list, keys.create, keys.revoke, audit.list)", len(tools))
	}
	for i, raw := range tools {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("tool %d not map: %T", i, raw)
			continue
		}
		if m["name"] == nil || m["description"] == nil {
			t.Errorf("tool %d missing fields: %v", i, m)
		}
		// Claude Code rejects the whole tool list if any tool
		// lacks an object inputSchema.
		if schema, ok := m["inputSchema"].(map[string]any); !ok || schema["type"] != "object" {
			t.Errorf("tool %v: inputSchema missing or not an object schema: %v", m["name"], m["inputSchema"])
		}
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"frobnicate"}` + "\n")
	var out bytes.Buffer
	_ = s.ServeStdio(context.Background(), in, &out, &bytes.Buffer{})
	var resp jsonRPCResponse
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("code: got %d, want -32601", resp.Error.Code)
	}
}

func TestServer_NotificationGetsNoResponse(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out, errOut bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out, &errOut); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notification should not produce output, got: %s", out.String())
	}
}

func TestServer_BadJSON(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	in := strings.NewReader(`{not json` + "\n")
	var out, errOut bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out, &errOut); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if !strings.Contains(errOut.String(), "bad request") {
		t.Errorf("expected error log, got: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("bad request should not produce output, got: %s", out.String())
	}
}

func TestServer_ContextCancelStopsLoop(t *testing.T) {
	s := NewServer("http://127.0.0.1:1", "")
	// Use an unbuffered channel + goroutine to feed input that
	// never arrives, so ServeStdio blocks in scanner.Scan. We
	// then cancel and assert it returns within 2s.
	in := make(chan []byte)
	cin := &chanReader{ch: in}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeStdio(ctx, cin, &bytes.Buffer{}, &bytes.Buffer{}) }()

	time.Sleep(20 * time.Millisecond) // let ServeStdio reach the scan
	cancel()
	close(in) // unblock the scan

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeStdio returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio did not return within 2s after cancel")
	}
}

type chanReader struct{ ch <-chan []byte }

func (c *chanReader) Read(p []byte) (int, error) {
	chunk, ok := <-c.ch
	if !ok {
		// Returning io.EOF here makes bufio.Scanner's Err()
		// return io.EOF, which ServeStdio treats as clean shutdown.
		return 0, io.EOF
	}
	return copy(p, chunk), nil
}

// rpc sends one JSON-RPC line through ServeStdio and returns the
// decoded result.
func rpc(t *testing.T, s *Server, line string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	_ = s.ServeStdio(context.Background(), strings.NewReader(line+"\n"), &out, io.Discard)
	var resp struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("bad response %q: %v", out.String(), err)
	}
	if resp.Error != nil || resp.Result == nil {
		t.Fatalf("want result, got error %v", resp.Error)
	}
	return resp.Result
}

// toolText calls a tool and returns its text content and isError.
func toolText(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	res := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("want one content block, got %v", res)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content type %v, want text", block["type"])
	}
	isErr, _ := res["isError"].(bool)
	return block["text"].(string), isErr
}

func TestServer_PingAndToolResultShape(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/rooms/R/invite":
			w.WriteHeader(http.StatusCreated) // no body, like the real server
		default:
			http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
		}
	}))
	defer api.Close()
	s := NewServer(api.URL, "tk_test")

	rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	if text, isErr := toolText(t, s, "rooms.invite", `{"room_id":"R","key_id":3,"role":"writer"}`); isErr || text != `{"ok":true}` {
		t.Errorf("invite with empty 201 body: got %q isError=%v, want {\"ok\":true}", text, isErr)
	}
	if text, isErr := toolText(t, s, "rooms.delete", `{"room_id":"nope"}`); !isErr || !strings.Contains(text, "404") {
		t.Errorf("failing call: got %q isError=%v, want isError with http 404", text, isErr)
	}
}

// fakeRoom serves whoami (id 1), a backlog, and an SSE stream
// that sends the given live messages after the ": ok" preamble.
func fakeRoom(backlog []string, live ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/rooms/R/messages":
			fmt.Fprintf(w, `{"messages":[%s]}`, strings.Join(backlog, ","))
		case "/v1/rooms/R/events":
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			for _, m := range live {
				time.Sleep(50 * time.Millisecond)
				fmt.Fprintf(w, "event: ping\ndata: {\"ts\":\"x\"}\n\nevent: message.created\ndata: %s\n\n", m)
				w.(http.Flusher).Flush()
			}
			<-r.Context().Done() // hold open like the real server
		}
	}))
}

func msg(id, author int) string {
	return fmt.Sprintf(`{"id":%d,"author_key_id":%d,"ts":"t%d","content":"m%d"}`, id, author, id, id)
}

func subscribeResult(t *testing.T, s *Server, args string) (events []map[string]any, next string, elapsed time.Duration) {
	t.Helper()
	start := time.Now()
	text, isErr := toolText(t, s, "messages.subscribe", args)
	if isErr {
		t.Fatalf("subscribe error: %s", text)
	}
	var env struct {
		Events    []map[string]any `json:"events"`
		NextSince string           `json:"next_since"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("envelope %q: %v", text, err)
	}
	return env.Events, env.NextSince, time.Since(start)
}

func TestSubscribe_BacklogReturnsAtOnceAndSkipsOwn(t *testing.T) {
	api := fakeRoom([]string{msg(5, 2), msg(6, 1)})
	defer api.Close()
	events, next, elapsed := subscribeResult(t, NewServer(api.URL, "tk_test"), `{"room_id":"R","since":"t4","wait":30}`)
	if len(events) != 1 || events[0]["id"] != float64(5) {
		t.Errorf("events: got %v, want only message 5 (6 is our own)", events)
	}
	if next != "t6" {
		t.Errorf("next_since: got %q, want t6 (own messages still advance the cursor)", next)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; backlog must not wait for the stream", elapsed)
	}
}

func TestSubscribe_ReturnsOnFirstLiveMessageFromOthers(t *testing.T) {
	api := fakeRoom(nil, msg(7, 1), msg(8, 2), msg(9, 2))
	defer api.Close()
	events, next, elapsed := subscribeResult(t, NewServer(api.URL, "tk_test"), `{"room_id":"R","since":"t6","wait":30}`)
	if len(events) != 1 || events[0]["id"] != float64(8) {
		t.Errorf("events: got %v, want message 8 (7 is our own)", events)
	}
	if next != "t8" {
		t.Errorf("next_since: got %q, want t8", next)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; must return on the first message, not after wait", elapsed)
	}
}

func TestServeStdio_SlowToolDoesNotBlockNextRequest(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/rooms/R/events" {
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			<-release // a subscribe with nothing to deliver
			return
		}
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer api.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- NewServer(api.URL, "tk_test").ServeStdio(context.Background(), inR, outW, io.Discard) }()
	go func() {
		fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"messages.subscribe","arguments":{"room_id":"R","wait":30}}}`)
		fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	}()

	first := make(chan string, 1)
	go func() {
		var resp struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(outR).Decode(&resp)
		first <- string(resp.ID)
	}()
	select {
	case id := <-first:
		if id != "2" {
			t.Errorf("first response id %s, want 2 (ping must not wait behind subscribe)", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ping blocked behind a pending subscribe")
	}
	close(release)
	inW.Close()
	go io.Copy(io.Discard, outR) // drain the subscribe response
	if err := <-done; err != nil {
		t.Errorf("ServeStdio: %v", err)
	}
}

func TestSubscribe_PagesPastFullPageOfOwnMessages(t *testing.T) {
	whoamiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			whoamiCalls++
			if whoamiCalls == 1 {
				http.Error(w, `{"error":{"code":"rate_limited"}}`, http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, `{"id":1}`)
		case "/v1/rooms/R/messages":
			var page []string
			if r.URL.Query().Get("since") == "t0" {
				for i := 1; i <= 500; i++ {
					page = append(page, msg(i, 1)) // a full page of our own
				}
			} else if r.URL.Query().Get("since") == "t500" {
				page = []string{msg(501, 2)}
			}
			fmt.Fprintf(w, `{"messages":[%s]}`, strings.Join(page, ","))
		case "/v1/rooms/R/events":
			fmt.Fprint(w, ": ok\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer api.Close()
	s := NewServer(api.URL, "tk_test")

	// First call: whoami fails, so nothing is filtered yet.
	if events, _, _ := subscribeResult(t, s, `{"room_id":"R","since":"t500","wait":5}`); len(events) != 1 {
		t.Fatalf("events: %v", events)
	}
	// Second call: whoami is retried (not latched at 0), our 500
	// messages are skipped and paging finds 501.
	events, next, elapsed := subscribeResult(t, s, `{"room_id":"R","since":"t0","wait":30}`)
	if len(events) != 1 || events[0]["id"] != float64(501) {
		t.Errorf("events: got %d, want only message 501", len(events))
	}
	if next != "t501" || elapsed > 5*time.Second {
		t.Errorf("next_since=%q after %v, want t501 without waiting", next, elapsed)
	}
}
