package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
