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
	"sync/atomic"
	"time"
)

// Server is the MCP transport. It wraps an *http.Client pointed at
// the local Conclave HTTP API.
type Server struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	selfID atomic.Int64 // caller's key id from /v1/whoami; 0 = not resolved yet
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

	// Requests run concurrently: a messages.subscribe blocking for
	// minutes (or abandoned by a cancelled client) must not hold up
	// the agent's next tool call. Writes are serialized so two
	// responses never interleave bytes. JSON-RPC notifications (no
	// id) get no response.
	var (
		writeMu  sync.Mutex
		writeErr error
		wg       sync.WaitGroup
	)
	defer wg.Wait()
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := s.handle(ctx, req)
			if resp == nil {
				return // notification, no response
			}
			writeMu.Lock()
			defer writeMu.Unlock()
			if err := enc.Encode(resp); err != nil && writeErr == nil {
				writeErr = fmt.Errorf("write response: %w", err)
			}
		}()
	}
	wg.Wait()
	if writeErr != nil {
		return writeErr
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
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: toolResult(s.callTool(ctx, p.Name, p.Arguments))}
	case "ping":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "notifications/initialized", "notifications/cancelled":
		// Notifications: no response.
		return nil
	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

// toolResult wraps a tool's output in the MCP CallToolResult
// shape. Tool failures become isError results (not JSON-RPC
// errors) so the model sees the message and can react.
func toolResult(v any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return toolResult(nil, err)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

func errorResponse(id json.RawMessage, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
}

// tool is one MCP tool. props/required become the JSON Schema
// inputSchema that MCP clients (Claude Code etc.) require.
type tool struct {
	name, desc string
	props      map[string]any
	required   []string
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

var (
	roomIDProp      = str("Room id (ULID) from rooms.list or rooms.create.")
	keyIDProp       = num("API key id from keys.list.")
	sinceProp       = str("Return only messages after this ts. Pass back next_since from the previous call.")
	trustOriginProp = enum("Trust origin.", "internal", "external")
)

var tools = []tool{
	{"rooms.list", "List rooms the caller is a member of.",
		map[string]any{"visibility": enum("Filter by visibility.", "public", "private")}, nil},
	{"rooms.create", "Create a new room; the caller becomes owner.",
		map[string]any{"name": str("Room name, 1..128 chars."), "description": str("Optional, <= 1024 chars."),
			"visibility": enum("Default private.", "public", "private")}, []string{"name"}},
	{"rooms.delete", "Archive (soft-delete) a room. Owner only.",
		map[string]any{"room_id": roomIDProp}, []string{"room_id"}},
	{"rooms.invite", "Invite an API key to a room. Owner only.",
		map[string]any{"room_id": roomIDProp, "key_id": keyIDProp, "role": enum("Member role.", "owner", "writer", "reader")},
		[]string{"room_id", "key_id", "role"}},
	{"rooms.remove", "Remove an API key from a room. Owner only.",
		map[string]any{"room_id": roomIDProp, "key_id": keyIDProp}, []string{"room_id", "key_id"}},
	{"messages.send", "Send a message to a room. The server stamps author_key_id and the author's trust_origin; the sender cannot override them.",
		map[string]any{"room_id": roomIDProp, "content": str("Message body, <= 256 KiB."),
			"content_type": enum("Default text/plain.", "text/plain", "text/markdown", "application/json"),
			"message_type": enum("Intent tag.", "question", "answer", "decision", "handoff", "status_update", "task_assignment", "alert", "system"),
			"metadata":     map[string]any{"type": "object", "description": "Free-form JSON, e.g. {\"files\": [\"src/auth/\"]}."}},
		[]string{"room_id", "content"}},
	{"messages.list", "List messages in a room, oldest first, paginated by sinceProp.",
		map[string]any{"room_id": roomIDProp, "since": sinceProp, "limit": num("Max messages, default 50, max 500."), "trust_origin": trustOriginProp},
		[]string{"room_id"}},
	{"messages.subscribe", "Wait for the next messages from OTHER members of a room (your own are skipped). With sinceProp: returns at once if anything newer than sinceProp exists, otherwise blocks until the first new message or until wait seconds pass (timed_out=true). Loop by passing next_since back as sinceProp.",
		map[string]any{"room_id": roomIDProp, "since": sinceProp, "wait": num("Seconds to block, default 60, max 300.")},
		[]string{"room_id"}},
	{"messages.delete", "Soft-delete a message. Owner only.",
		map[string]any{"room_id": roomIDProp, "msg_id": num("Message id.")}, []string{"room_id", "msg_id"}},
	{"keys.list", "List API keys (id, label, trust_origin). Use it to find the key_id of another agent.",
		map[string]any{}, nil},
	{"keys.create", "Create a new API key; returns the plaintext once. Admin key only.",
		map[string]any{"label": str("Unique label, 1..128 chars."), "trust_origin": trustOriginProp}, []string{"label"}},
	{"keys.revoke", "Revoke an API key. Admin key only.",
		map[string]any{"id": keyIDProp}, []string{"id"}},
	{"audit.list", "List audit log entries with filters.",
		map[string]any{"actor": str("Actor key id."), "action": str("e.g. room.create."), "room_id": roomIDProp,
			"since": str("ISO-8601 ts."), "limit": num("Max entries.")}, nil},
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := map[string]any{"type": "object", "properties": t.props}
		if t.required != nil {
			schema["required"] = t.required
		}
		out = append(out, map[string]any{"name": t.name, "description": t.desc, "inputSchema": schema})
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
		return s.subscribe(ctx, p.RoomID, p.Since, p.Wait)
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

// subscribe implements messages.subscribe as a long-poll that
// returns the next message(s) from other members:
//
//	{"events": [...], "next_since": "<ts>", "timed_out": bool, "has_more": bool}
//
// It opens the SSE stream (ADR-0009) first and only then reads
// the backlog after since, so a message sent in between is never
// lost. A non-empty backlog returns at once; otherwise the first
// live message does, or the stream closes after wait seconds.
// The caller's own messages are skipped but still advance
// next_since.
func (s *Server) subscribe(ctx context.Context, roomID, since string, wait int) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wait+5)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.BaseURL+"/v1/rooms/"+roomID+"/events?wait="+itoaInt(wait), nil)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("subscribe: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	sc := bufio.NewScanner(resp.Body)
	// 256 KiB of content can JSON-escape to ~1.5 MiB in one data line.
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	// The server sends ": ok" once the subscription is live.
	for sc.Scan() && !bytes.HasPrefix(sc.Bytes(), []byte(":")) {
	}

	self := s.selfKeyID(ctx)
	next := since
	var seen int64 // highest message id already handled
	envelope := func(events []json.RawMessage, hasMore bool) map[string]any {
		if events == nil {
			events = []json.RawMessage{}
		}
		return map[string]any{"events": events, "next_since": next, "timed_out": len(events) == 0, "has_more": hasMore}
	}

	// Backlog: page on while pages are full but hold only our own
	// messages, so a long run of them cannot hide a reply.
	const pageSize = 500 // server max
	for cursor := since; cursor != ""; {
		v, err := s.httpGet(ctx, "/v1/rooms/"+roomID+"/messages?limit="+itoaInt(pageSize)+"&since="+url.QueryEscape(cursor))
		if err != nil {
			return nil, err
		}
		var page struct {
			Messages []json.RawMessage `json:"messages"`
		}
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &page)
		var events []json.RawMessage
		for _, m := range page.Messages {
			if id, ts, mine := msgInfo(m, self); id != 0 {
				next, seen = ts, id
				if !mine {
					events = append(events, m)
				}
			}
		}
		full := len(page.Messages) >= pageSize
		if len(events) > 0 {
			return envelope(events, full), nil
		}
		if !full || next == cursor {
			break
		}
		cursor = next
	}

	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data[:0], bytes.TrimPrefix(after, []byte(" "))...)
			continue
		}
		if len(line) != 0 || len(data) == 0 {
			continue
		}
		m := json.RawMessage(append([]byte(nil), data...))
		data = data[:0]
		id, ts, mine := msgInfo(m, self) // ping events have no id
		if id <= seen {
			continue // heartbeat, or already covered by the backlog
		}
		next, seen = ts, id
		if !mine {
			return envelope([]json.RawMessage{m}, false), nil
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("subscribe: read stream: %w", err)
	}
	// Stream closed after wait seconds (or our timeout fired).
	return envelope(nil, false), nil
}

// msgInfo returns a message's id, ts and whether self wrote it.
// id is 0 for payloads that are not messages (heartbeats).
func msgInfo(m json.RawMessage, self int64) (id int64, ts string, mine bool) {
	var v struct {
		ID     int64  `json:"id"`
		Author int64  `json:"author_key_id"`
		TS     string `json:"ts"`
	}
	if json.Unmarshal(m, &v) != nil {
		return 0, "", false
	}
	return v.ID, v.TS, self != 0 && v.Author == self
}

// selfKeyID resolves the caller's key id via /v1/whoami and caches
// it. A failed lookup is retried on the next call; until then it
// returns 0 (own messages are not filtered).
func (s *Server) selfKeyID(ctx context.Context) int64 {
	if id := s.selfID.Load(); id != 0 {
		return id
	}
	v, err := s.httpGet(ctx, "/v1/whoami")
	if m, ok := v.(map[string]any); err == nil && ok {
		if id, ok := m["id"].(float64); ok {
			s.selfID.Store(int64(id))
		}
	}
	return s.selfID.Load()
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		// invite (201), remove/revoke (204) answer with no body.
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("http %d", resp.StatusCode)
		}
		return map[string]any{"ok": true}, nil
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return result, nil
}

func itoaInt(n int) string     { return fmt.Sprintf("%d", n) }
func itoaInt64(n int64) string { return fmt.Sprintf("%d", n) }

// ServeHTTP is the JSON-RPC-over-HTTP entrypoint (SPEC-0003 §2.2,
// ADR-0007 §Q9): one JSON-RPC request per POST, one JSON response
// back (the JSON-response mode of MCP Streamable HTTP). It
// dispatches through the same handle() as stdio, so both
// transports speak the identical protocol.
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
	resp := s.handle(r.Context(), req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	q := url.Values{}
	for k, v := range m {
		q.Set(k, fmt.Sprint(v))
	}
	return q.Encode()
}
