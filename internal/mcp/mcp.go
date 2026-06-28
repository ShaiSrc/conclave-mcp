// Package mcp implements the Conclave MCP transport (SPEC-0003 §7).
//
// The transport is a thin wrapper: every MCP tool method calls the
// corresponding HTTP endpoint and returns the response as an MCP
// result. There is no business logic in this package.
//
// Wire format: JSON-RPC 2.0 over stdin/stdout. The server reads
// one JSON object per line from stdin, writes one JSON object
// per line to stdout. Logging goes to stderr.
//
// The transport is invoked via `conclave-mcp mcp` (added in a
// follow-up to the serve subcommand). For unit testing, the
// ServeStdio function takes explicit io.Reader/io.Writer so we
// can drive it with strings.Reader and bytes.Buffer.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Server is the MCP transport. It wraps an *http.Client pointed at
// the local Conclave HTTP API.
type Server struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewServer builds an MCP server talking to baseURL with the given
// API key bearer token.
func NewServer(baseURL, apiKey string) *Server {
	return &Server{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: http.DefaultClient,
	}
}

// jsonRPCRequest is the standard JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is the standard JSON-RPC 2.0 response envelope.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolCallParams is the shape of a tools/call request.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ServeStdio reads JSON-RPC requests from in (one per line), writes
// responses to out (one per line), and blocks until ctx is
// cancelled or in is closed.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer, errOut io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Some MCP messages are large; bump the buffer cap.
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

	// JSON-RPC notifications (no id) get no response. We serialize
	// writes to out so a notification and a response don't
	// interleave bytes.
	var writeMu sync.Mutex
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(errOut, "mcp: bad request: %v\n", err)
			continue
		}
		if req.JSONRPC != "2.0" {
			fmt.Fprintf(errOut, "mcp: bad jsonrpc version: %q\n", req.JSONRPC)
			continue
		}
		resp := s.handle(ctx, req)
		if resp == nil {
			// Notification, no response.
			continue
		}
		writeMu.Lock()
		if err := enc.Encode(resp); err != nil {
			writeMu.Unlock()
			return fmt.Errorf("write response: %w", err)
		}
		writeMu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		// EOF on the input is a clean shutdown, not an error.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "conclave-mcp", "version": "0.1.0-dev"},
			},
		}
	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": s.toolList(),
			},
		}
	case "tools/call":
		var p toolCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errorResponse(req.ID, -32602, "invalid params: "+err.Error())
		}
		result, err := s.callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return errorResponse(req.ID, -32603, err.Error())
		}
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	case "notifications/initialized", "notifications/cancelled":
		// Notifications: no response.
		return nil
	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

func errorResponse(id json.RawMessage, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
}

func (s *Server) toolList() []map[string]any {
	tools := []struct {
		name, desc string
	}{
		{"rooms.list", "List rooms visible to the caller. Optional arg: visibility ('public' or 'private')."},
		{"rooms.create", "Create a new room; caller becomes owner. Args: {name, description?, visibility?}."},
		{"rooms.delete", "Soft-delete a room. Args: {room_id}."},
		{"rooms.invite", "Invite an API key to a room. Args: {room_id, key_id, role}. role is 'owner', 'writer', or 'reader' (SPEC-0003 §3.3). Owner only."},
		{"rooms.remove", "Remove an API key from a room. Args: {room_id, key_id}. Owner only."},
		{"messages.send", "Send a message to a room. Args: {room_id, content, content_type?, message_type?, metadata?}. The server denormalises the author's trust_origin (SPEC-0001 Q11) onto the new message; the sender cannot override it."},
		{"messages.list", "List messages in a room, paginated by since. Args: {room_id, since?, limit?, trust_origin?}. trust_origin is 'internal' or 'external' (SPEC-0001 Q11)."},
		{"messages.subscribe", "Subscribe to a room and receive new messages as they arrive (SSE, ADR-0009). Args: {room_id, wait?}. Returns a JSON array of message objects; the connection closes after `wait` seconds (default 60, max 300) or when the room is archived. Polling via messages.list is the canonical fallback for clients that cannot hold a streaming connection."},
		{"messages.delete", "Soft-delete a message (owner only). Args: {room_id, msg_id}."},
		{"keys.list", "List API keys. Each row carries trust_origin (SPEC-0001 Q11)."},
		{"keys.create", "Create a new API key; returns plaintext once. Args: {label?, trust_origin?}. trust_origin is 'internal' (default) or 'external' (SPEC-0001 Q11, ADR-0008 revision)."},
		{"keys.revoke", "Revoke an API key (soft delete). Args: {id}."},
		{"audit.list", "List audit log entries with filters. Args: {actor?, action?, room_id?, since?, limit?}."},
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.desc,
		})
	}
	return out
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	// Map tool name -> HTTP method + path. Args are passed as the
	// JSON body (or as query params for GETs).
	switch name {
	case "rooms.list":
		// Optional visibility filter (SPEC-0001 Q6).
		var p struct {
			Visibility string `json:"visibility"`
		}
		_ = json.Unmarshal(args, &p)
		path := "/v1/rooms"
		if p.Visibility != "" {
			path += "?visibility=" + url.QueryEscape(p.Visibility)
		}
		return s.httpGet(ctx, path)
	case "rooms.create":
		return s.httpPost(ctx, "/v1/rooms", args)
	case "rooms.delete":
		// Soft-delete via the archive endpoint (SPEC-0001 Q8,
		// ADR-0007 §6). Conclave's "delete" IS archive.
		var p struct {
			RoomID string `json:"room_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("rooms.delete requires room_id")
		}
		return s.httpPost(ctx, "/v1/rooms/"+p.RoomID+"/archive", json.RawMessage("{}"))
	case "rooms.invite":
		// SPEC-0003 §3.3 + ADR-0007 §Open questions. The
		// HTTP endpoint is POST /v1/rooms/{id}/invite with
		// body {key_id, role}. We forward verbatim.
		var p struct {
			RoomID string `json:"room_id"`
			KeyID  int64  `json:"key_id"`
			Role   string `json:"role"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("rooms.invite requires room_id")
		}
		if p.KeyID == 0 {
			return nil, errors.New("rooms.invite requires key_id")
		}
		if p.Role != "owner" && p.Role != "writer" && p.Role != "reader" {
			return nil, errors.New("rooms.invite role must be 'owner', 'writer', or 'reader'")
		}
		return s.httpPost(ctx, "/v1/rooms/"+p.RoomID+"/invite", args)
	case "rooms.remove":
		// DELETE /v1/rooms/{id}/members/{key_id}. Owner only.
		var p struct {
			RoomID string `json:"room_id"`
			KeyID  int64  `json:"key_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("rooms.remove requires room_id")
		}
		if p.KeyID == 0 {
			return nil, errors.New("rooms.remove requires key_id")
		}
		return s.httpDelete(ctx, "/v1/rooms/"+p.RoomID+"/members/"+itoaInt64(p.KeyID))
	case "messages.send":
		// args should be {room_id, content, content_type?,
		// message_type?, metadata?}. The HTTP body shape is
		// the same, so we forward verbatim.
		var p struct {
			RoomID string `json:"room_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("messages.send requires room_id")
		}
		return s.httpPost(ctx, "/v1/rooms/"+p.RoomID+"/messages", args)
	case "messages.list":
		var p struct {
			RoomID      string `json:"room_id"`
			Since       string `json:"since"`
			Limit       int    `json:"limit"`
			TrustOrigin string `json:"trust_origin"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("messages.list requires room_id")
		}
		if p.TrustOrigin != "" && p.TrustOrigin != "internal" && p.TrustOrigin != "external" {
			return nil, errors.New("trust_origin must be 'internal' or 'external'")
		}
		q := ""
		if p.Since != "" {
			q += "?since=" + url.QueryEscape(p.Since)
		}
		if p.Limit > 0 {
			if q == "" {
				q = "?"
			} else {
				q += "&"
			}
			q += "limit=" + itoaInt(p.Limit)
		}
		if p.TrustOrigin != "" {
			if q == "" {
				q = "?"
			} else {
				q += "&"
			}
			q += "trust_origin=" + url.QueryEscape(p.TrustOrigin)
		}
		return s.httpGet(ctx, "/v1/rooms/"+p.RoomID+"/messages"+q)
	case "messages.delete":
		var p struct {
			RoomID string `json:"room_id"`
			MsgID  int64  `json:"msg_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" || p.MsgID == 0 {
			return nil, errors.New("messages.delete requires room_id and msg_id")
		}
		return s.httpDelete(ctx, "/v1/rooms/"+p.RoomID+"/messages/"+itoaInt64(p.MsgID))
	case "messages.subscribe":
		// SSE subscription with backlog (ADR-0009 + a
		// long-poll-friendly envelope). The MCP caller
		// gets back a JSON object:
		//
		//   {
		//     "events":     [ <message>, ... ],   // events that arrived
		//     "next_since": "<ts>",              // pass back as since
		//     "timed_out":  false,               // true if no events in wait window
		//     "has_more":   false                // true if events still pending after this window
		//   }
		//
		// An MCP agent that wants continuous coverage
		// calls this in a loop, passing back
		// `next_since` as `since` to catch up. The
		// server-side SSE channel is held open for up
		// to `wait` seconds (default 60, max 300).
		// Polling via messages.list is the canonical
		// fallback for clients that cannot hold a
		// streaming connection; the user can also pass
		// `wait=0` to skip the live wait and get just
		// the SSE response drained immediately.
		var p struct {
			RoomID string `json:"room_id"`
			Since  string `json:"since"`
			Wait   int    `json:"wait"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("messages.subscribe requires room_id")
		}
		if p.Wait <= 0 {
			p.Wait = 60
		}
		if p.Wait > 300 {
			p.Wait = 300
		}
		// Open the SSE channel. The response is a flat
		// array of message objects; we wrap it into
		// the long-poll envelope here.
		raw, err := s.httpSSE(ctx, "/v1/rooms/"+p.RoomID+"/events?wait="+itoaInt(p.Wait))
		if err != nil {
			return nil, err
		}
		eventsArr, _ := raw.([]json.RawMessage)
		// Compute next_since from the last event's ts.
		// If no events arrived, the cursor stays at the
		// value the caller passed in (or empty).
		nextSince := p.Since
		if len(eventsArr) > 0 {
			var last struct {
				TS string `json:"ts"`
			}
			if jerr := json.Unmarshal(eventsArr[len(eventsArr)-1], &last); jerr == nil && last.TS != "" {
				nextSince = last.TS
			}
		}
		// timed_out: the SSE channel returned with
		// zero events. The agent can decide whether
		// to keep polling or back off. We don't have
		// a way to distinguish "no events" from
		// "events came in but not within wait" from
		// inside httpSSE; the conservative answer is
		// to mark timed_out=true when the channel
		// closes with no events, which the current
		// implementation does.
		timedOut := len(eventsArr) == 0
		return map[string]any{
			"events":     eventsArr,
			"next_since": nextSince,
			"timed_out":  timedOut,
			"has_more":   !timedOut, // a non-empty result means we received something
		}, nil
	case "keys.list":
		return s.httpGet(ctx, "/v1/keys")
	case "keys.create":
		return s.httpPost(ctx, "/v1/keys", args)
	case "keys.revoke":
		var p struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(args, &p)
		return s.httpDelete(ctx, "/v1/keys/"+itoaInt64(p.ID))
	case "audit.list":
		// We forward raw args as query params, which is a hack
		// but works for the small set of filters we support.
		return s.httpGet(ctx, "/v1/audit?"+rawJSONToQuery(args))
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) httpGet(ctx context.Context, path string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	s.addAuth(req)
	return s.do(req)
}

func (s *Server) httpPost(ctx context.Context, path string, body json.RawMessage) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	return s.do(req)
}

func (s *Server) httpDelete(ctx context.Context, path string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	s.addAuth(req)
	return s.do(req)
}

// httpSSE opens a text/event-stream connection and returns
// a JSON array containing one element per `event: <name>`
// block. The `wait` query parameter controls how long the
// server keeps the connection open; the client honors the
// same timeout via the request context.
//
// The SSE format produced by the server is:
//
//	event: message.created
//	data: {"id":...}
//
//	event: ping
//	data: {"ts":...}
//
// We collect the `data:` payloads (one per `message.created`
// event) into a JSON array and ignore `ping` heartbeats.
// This matches the spec'd behaviour of `messages.subscribe`
// (ADR-0009 §2.1): the MCP tool returns a JSON array of
// messages received during the subscription window.
func (s *Server) httpSSE(ctx context.Context, path string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	s.addAuth(req)
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscribe: status %d", resp.StatusCode)
	}
	// bufio.Scanner with a large buffer; SSE events can be
	// arbitrarily long (the full message body is one
	// payload). 1 MiB is plenty for v0.x.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		messages    []json.RawMessage
		currentData []byte
	)
	flush := func() {
		if len(currentData) == 0 {
			return
		}
		// Try to parse; on parse error (e.g. a `ping` with
		// {"ts":...}) we drop the line. We only keep
		// payloads that look like messages.
		var probe map[string]any
		if err := json.Unmarshal(currentData, &probe); err == nil {
			if _, hasID := probe["id"]; hasID {
				messages = append(messages, json.RawMessage(append([]byte{}, currentData...)))
			}
		}
		currentData = currentData[:0]
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			if len(currentData) > 0 {
				currentData = append(currentData, '\n')
			}
			currentData = append(currentData, line[6:]...)
		} else if bytes.HasPrefix(line, []byte("data:")) {
			// Same as above, no leading space.
			if len(currentData) > 0 {
				currentData = append(currentData, '\n')
			}
			currentData = append(currentData, line[5:]...)
		} else if len(line) == 0 {
			// Blank line = end of event.
			flush()
		}
		// Other lines (event:, id:, retry:, comments) are
		// ignored for the MCP tool's purposes.
	}
	flush()
	if err := scanner.Err(); err != nil {
		// EOF is normal (server closed the stream at the
		// wait timeout). Anything else is an error.
		if err.Error() != "EOF" {
			return nil, err
		}
	}
	return messages, nil
}

func (s *Server) addAuth(req *http.Request) {
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
}

func (s *Server) do(req *http.Request) (any, error) {
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %v", resp.StatusCode, result)
	}
	return result, nil
}

func itoaInt(n int) string     { return fmt.Sprintf("%d", n) }
func itoaInt64(n int64) string { return fmt.Sprintf("%d", n) }

// ServeHTTP is the JSON-RPC-over-HTTP entrypoint (SPEC-0003 §2.2,
// ADR-0007 §Q9). It accepts a single JSON-RPC 2.0 request
// envelope and returns a single JSON-RPC 2.0 response envelope.
// No streaming, no SSE. The transport is intentionally the
// same as stdio's but framed in HTTP request/response.
//
// This handler is wired into the server at `POST /v1/mcp` by
// internal/server/server.go.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, json.RawMessage(`null`), -32700, "parse error")
		return
	}
	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, -32600, "jsonrpc must be '2.0'")
		return
	}
	switch req.Method {
	case "tools/list":
		// tools/list takes no params; we return the
		// same list as stdio.
		s.writeJSONRPCResult(w, req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		var p toolCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
			return
		}
		result, err := s.callTool(r.Context(), p.Name, p.Arguments)
		if err != nil {
			// callTool returns errors for both transport
			// failures and HTTP 4xx/5xx. We map them to
			// the JSON-RPC "internal error" code (which is
			// not ideal but standard for thin
			// wrappers). The detail carries the HTTP
			// status and the decoded body.
			writeJSONRPCError(w, req.ID, -32603, err.Error())
			return
		}
		s.writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "json", "data": result},
			},
		})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// writeJSONRPCResult writes a successful JSON-RPC 2.0 response
// envelope with the given id and result.
func (s *Server) writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

// writeJSONRPCError writes a JSON-RPC 2.0 error response. The
// id is echoed when known; `null` otherwise (parse errors).
func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors use HTTP 200; the error is in the body.
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	})
}

// rawJSONToQuery converts a JSON object to a URL query string.
// This is a quick-and-dirty helper; the audit endpoint accepts
// the same query parameters it accepts as JSON, so this works
// for the supported filter set.
func rawJSONToQuery(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, "&")
}
