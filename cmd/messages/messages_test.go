package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func newSubCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("server", "", "")
	c.Flags().String("token", "", "")
	return c
}

func TestSend_FromFlag(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms/01M72DWQRAZXY3X8HWGJ0QZGNW/messages" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("method=%s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "room_id": "01M72DWQRAZXY3X8HWGJ0QZGNW", "content_type": "text/plain",
			"size": 5, "created_at": "2026-01-01T00:00:00Z", "content": "hello",
		})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	sub := newSendCmd()
	if err := sub.Flags().Set("content", "hello"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["content"] != "hello" {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(buf.String(), `"id": 1`) {
		t.Errorf("out missing id: %s", buf.String())
	}
}

func TestSend_FromStdin(t *testing.T) {
	// Capture the body the server receives.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "room_id": "01M72DWQRAZXY3X8HWGJ0QZGNW", "content_type": "text/plain",
			"size": 5, "created_at": "2026-01-01T00:00:00Z", "content": "stdin-payload",
		})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	// Swap stdin for the test.
	ResetStdin(strings.NewReader("stdin-payload\n"))
	defer ResetStdin(nil)

	var buf bytes.Buffer
	sub := newSendCmd()
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["content"] != "stdin-payload\n" {
		t.Errorf("body=%v", gotBody)
	}
}

func TestSend_ConflictingFlags(t *testing.T) {
	sub := newSendCmd()
	if err := sub.Flags().Set("content", "x"); err != nil {
		t.Fatal(err)
	}
	if err := sub.Flags().Set("content-file", "y"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&bytes.Buffer{})
	sub.SetErr(&bytes.Buffer{})
	sub.SetContext(context.Background())
	err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"})
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestSend_413IsExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"code":"too_large","message":"exceeds 65536 bytes"}}`))
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	var buf bytes.Buffer
	sub := newSendCmd()
	if err := sub.Flags().Set("content", "x"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); ExitCode(err) != 1 {
		t.Fatalf("ExitCode=%d (err=%v)", ExitCode(err), err)
	}
}

func TestList_TableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/keys" && !strings.Contains(r.URL.RawQuery, "since=m0") {
			t.Errorf("query=%q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": 1, "room_id": "01M72DWQRAZXY3X8HWGJ0QZGNW", "content_type": "text/plain", "size": 5, "created_at": "2026-01-01T00:00:00Z"},
				{"id": 2, "room_id": "01M72DWQRAZXY3X8HWGJ0QZGNW", "content_type": "text/markdown", "size": 12, "created_at": "2026-01-02T00:00:00Z"},
			},
			"next_since": "m2",
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
	if err := sub.Flags().Set("since", "m0"); err != nil {
		t.Fatal(err)
	}
	if err := sub.Flags().Set("limit", "50"); err != nil {
		t.Fatal(err)
	}
	sub.SetOut(&buf)
	sub.SetErr(&buf)
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(buf.String(), "1") || !strings.Contains(buf.String(), "2") {
		t.Errorf("table missing ids: %s", buf.String())
	}
}

func TestList_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages":   []map[string]any{{"id": int64(1), "room_id": "01M72DWQRAZXY3X8HWGJ0QZGNW"}},
			"next_since": "m1",
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
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got []msgView
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v\nout=%s", err, buf.String())
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("got=%+v", got)
	}
}

func TestResolveContent(t *testing.T) {
	if s, err := resolveContent("a", ""); err != nil || s != "a" {
		t.Errorf("content: %q %v", s, err)
	}
	if _, err := resolveContent("a", "b"); err == nil {
		t.Error("expected conflict")
	}
	// --content-file "-" -> stdin
	ResetStdin(strings.NewReader("from-stdin"))
	defer ResetStdin(nil)
	if s, err := resolveContent("", "-"); err != nil || s != "from-stdin" {
		t.Errorf("stdin via -: %q %v", s, err)
	}
	// empty flags -> stdin
	ResetStdin(strings.NewReader("from-stdin-default"))
	if s, err := resolveContent("", ""); err != nil || s != "from-stdin-default" {
		t.Errorf("stdin default: %q %v", s, err)
	}
}

func TestMapErr(t *testing.T) {
	if ec := ExitCode(nil); ec != 0 {
		t.Errorf("nil=%d", ec)
	}
	if ec := ExitCode(mapErr(nil)); ec != 0 {
		t.Errorf("mapErr(nil)=%d", ec)
	}
	if ec := ExitCode(mapErr(&client.NetError{Err: errors.New("x")})); ec != 3 {
		t.Errorf("net=%d", ec)
	}
	if ec := ExitCode(mapErr(&client.APIError{Status: 404})); ec != 1 {
		t.Errorf("api=%d", ec)
	}
}

func TestList_SinceIsURLEscaped(t *testing.T) {
	const since = "2026-11-07T11:00:00+01:00"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); r.URL.Path != "/v1/keys" && got != since {
			t.Errorf("server got since=%q, want %q", got, since)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()

	sub := newListCmd()
	_ = sub.Flags().Set("since", since)
	sub.SetOut(&bytes.Buffer{})
	sub.SetContext(context.Background())
	if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
		t.Fatal(err)
	}
}

func TestSend_ReplyTo(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
	}))
	defer srv.Close()
	oldOC := openClient
	openClient = func(_ context.Context, _ *cobra.Command) (*client.Client, error) {
		return client.New(client.Options{Server: srv.URL, Token: "stub"})
	}
	defer func() { openClient = oldOC }()
	send := func(flags map[string]string) {
		gotBody = nil
		sub := newSendCmd()
		for k, v := range flags {
			if err := sub.Flags().Set(k, v); err != nil {
				t.Fatal(err)
			}
		}
		sub.SetOut(&bytes.Buffer{})
		sub.SetContext(context.Background())
		if err := sub.RunE(sub, []string{"01M72DWQRAZXY3X8HWGJ0QZGNW"}); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	send(map[string]string{"content": "done", "reply-to": "12"})
	if gotBody["reply_to"] != float64(12) {
		t.Errorf("--reply-to 12: body=%v", gotBody)
	}
	send(map[string]string{"content": "plain"})
	if _, ok := gotBody["reply_to"]; ok {
		t.Errorf("no --reply-to: body has reply_to: %v", gotBody)
	}
}
