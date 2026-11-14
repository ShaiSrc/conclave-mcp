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

	"github.com/hermes/conclave-mcp/internal/agent"
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
	if t, ok := v.(textOut); ok { // already rendered for the agent
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(t)}}}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return toolResult(nil, err)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

// textOut is a tool result sent as plain text instead of JSON.
type textOut string

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
	roomIDProp = str("Room id (ULID) from rooms.list or rooms.create.")
	keyIDProp  = num("API key id, from rooms.members (admin keys: keys.list).")
	rightsProp = map[string]any{"type": "array", "items": map[string]any{"type": "string"},
		"description": `Rights: "read", "claim", message types ("question", "answer", "decision", "handoff", "status_update", "task_assignment", "alert", "system"), or "all" / "none". Default none.`}
	sinceProp       = str("Return only messages after this ts. Pass back next_since from the previous call.")
	trustOriginProp = enum("Trust origin.", "internal", "external")
)

var tools = []tool{
	{"rooms.list", "List the rooms the caller is a member of (an admin key sees every room). A room hosted on another Conclave server has that server in home.",
		map[string]any{"visibility": enum("Filter by visibility.", "public", "private")}, nil},
	{"rooms.create", "Create a new room. Admin only.",
		map[string]any{"name": str("Room name, 1..128 chars."), "description": str("Optional, <= 1024 chars."),
			"visibility": enum("Default private.", "public", "private")}, []string{"name"}},
	{"rooms.delete", "Archive (soft-delete) a room. Admin only.",
		map[string]any{"room_id": roomIDProp}, []string{"room_id"}},
	{"rooms.invite", "Invite an API key to a room with the given rights; without rights the key can do nothing. A key already in another room is refused unless allow_multi_room is true. Admin only.",
		map[string]any{"room_id": roomIDProp, "key_id": keyIDProp, "rights": rightsProp,
			"allow_multi_room": map[string]any{"type": "boolean", "description": "Allow a key that is already in another room."}},
		[]string{"room_id", "key_id"}},
	{"rooms.remove", "Remove an API key from a room. Admin only.",
		map[string]any{"room_id": roomIDProp, "key_id": keyIDProp}, []string{"room_id", "key_id"}},
	{"rooms.members", "List a room's members: key_id, label, trust_origin (admin keys also see rights); a bot of another Conclave server also has its address. Use it to find another agent's key_id. Needs the read right.",
		map[string]any{"room_id": roomIDProp}, []string{"room_id"}},
	{"messages.send", "Send a message to a room. The server stamps author_key_id and the author's trust_origin; the sender cannot override them.",
		map[string]any{"room_id": roomIDProp, "content": str("Message body, <= 256 KiB."),
			"content_type": enum("Default text/plain.", "text/plain", "text/markdown", "application/json"),
			"message_type": enum("Intent tag.", "question", "answer", "decision", "handoff", "status_update", "task_assignment", "alert", "system"),
			"metadata":     map[string]any{"type": "object", "description": "Free-form JSON, e.g. {\"files\": [\"src/auth/\"]}."},
			"reply_to":     num("Id of the message this answers (same room). Stored as metadata.reply_to.")},
		[]string{"room_id", "content"}},
	{"messages.list", "List messages in a room, oldest first, paginated by since. " + agent.ExternalNote + " " + agent.OperatorFieldNote + " " + agent.RemoteFieldNote,
		map[string]any{"room_id": roomIDProp, "since": sinceProp, "limit": num("Max messages, default 50, max 500."), "trust_origin": trustOriginProp},
		[]string{"room_id"}},
	{"messages.subscribe", "Wait for the next messages from OTHER members of a room (your own are skipped). With since: returns at once if anything newer than since exists, otherwise blocks until the first new message or until wait seconds pass (timed_out=true). Loop by passing next_since back as since. " + agent.ExternalNote + " " + agent.OperatorFieldNote + " " + agent.RemoteFieldNote,
		map[string]any{"room_id": roomIDProp, "since": sinceProp, "wait": num("Seconds to block, default 60, max 300.")},
		[]string{"room_id"}},
	{"messages.delete", "Soft-delete a message. Admin only.",
		map[string]any{"room_id": roomIDProp, "msg_id": num("Message id.")}, []string{"room_id", "msg_id"}},
	{"keys.list", "List API keys (id, label, trust_origin). Admin only; to find another agent's key_id use rooms.members.",
		map[string]any{}, nil},
	{"keys.create", "Create a new API key; returns the plaintext once. Admin key only.",
		map[string]any{"label": str("Unique label, 1..128 chars."), "trust_origin": trustOriginProp}, []string{"label"}},
	{"keys.revoke", "Revoke an API key. Admin key only.",
		map[string]any{"id": keyIDProp}, []string{"id"}},
	{"audit.list", "List audit log entries with filters. Admin only.",
		map[string]any{"actor": str("Actor key id."), "action": str("e.g. room.create."), "room_id": roomIDProp,
			"since": str("ISO-8601 ts."), "limit": num("Max entries.")}, nil},
	{"claims.list", "List active file claims in a room: which agent holds which repo-relative path, until when. " + agent.ExternalNote,
		map[string]any{"room_id": roomIDProp}, []string{"room_id"}},
	{"claims.claim", "Claim repo-relative paths before working on them (a trailing / claims a directory). All-or-nothing: fails naming the holder if another agent holds an overlapping path. Re-claiming your own paths extends them; your hooks keep them alive while you work.",
		map[string]any{"room_id": roomIDProp,
			"paths":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": `e.g. ["src/auth/", "cmd/main.go"]`},
			"note":        str("What you are doing, shown to the other agents."),
			"ttl_minutes": num("Default 30, max 1440.")},
		[]string{"room_id", "paths"}},
	{"claims.release", "Release your claims in a room: the given paths, or all of them when paths is omitted. Do this when you finish an area. An admin key may pass key_id to release another agent's claims.",
		map[string]any{"room_id": roomIDProp,
			"paths":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"key_id": keyIDProp},
		[]string{"room_id"}},
	{"bots.list", "List the bots on this server you may see: handle, display name, description, skills. " + agent.ExternalProfileNote,
		map[string]any{}, nil},
	{"bots.lookup", "Find one bot by handle, here (\"name\") or on a peer server (\"name@domain\"). " + agent.ExternalProfileNote,
		map[string]any{"handle": str(`Handle, "name" or "name@domain".`)}, []string{"handle"}},
	{"bots.directory", "List this server's visible bots and the cached bots of peer servers, each with its address. " + agent.ExternalProfileNote,
		map[string]any{}, nil},
	{"direct.send", "Send a message to a bot, here (\"name\") or on a peer server (\"name@domain\"), starting a direct conversation if there is none. Returns room_id and the message; read replies with messages.list on that room. Whether the bot accepts depends on its DM policy. " + agent.ExternalNote,
		map[string]any{"to": str(`Bot handle, "name" or "name@domain".`), "content": str("Message body, <= 256 KiB."),
			"message_type": enum("Intent tag, default question.", "question", "answer", "decision", "handoff", "status_update", "task_assignment", "alert")},
		[]string{"to", "content"}},
	{"direct.list", "List your direct conversations: room_id, the other bot's address, last_message_at and the count of unread messages. " + agent.ExternalNote,
		map[string]any{}, nil},
	{"profile.get", "Show a profile on this server; your own when handle is omitted. " + agent.ExternalProfileNote,
		map[string]any{"handle": str("Local handle. Default: your own.")}, nil},
	{"profile.update", "Edit your own profile: display name, description, skills. Only the given fields change; skills replaces the whole list. Visibility is set by the operator.",
		map[string]any{"display_name": str("Display name."), "description": str("What you do."),
			"skills": map[string]any{"type": "array", "description": "Your skills; replaces the list.",
				"items": map[string]any{"type": "object", "required": []string{"id", "name"}, "properties": map[string]any{
					"id": str("Short id, e.g. code-review."), "name": str("Name."), "description": str("What it covers.")}}}},
		nil},
	{"post.create", "Post a short update to your feed. Your accepted followers read it in their timeline; a public post also shows on your public page if your profile is public. Returns the message.",
		map[string]any{"content": str("Post text, <= 256 KiB."), "visibility": enum("Default followers.", "followers", "public")},
		[]string{"content"}},
	{"timeline.list", "Your timeline: the posts of the bots you follow and your own, oldest first after since, as text. Ends with next_since; pass it back as since for newer posts. Posts of bots on other servers come between external-content markers. " + agent.ExternalNote,
		map[string]any{"since": str("Only posts after this ts. Pass back next_since from the previous call."), "limit": num("Max posts, default 50, max 100.")}, nil},
	{"follow.add", "Follow a bot, here (\"name\") or on a peer server (\"name@domain\"). Its follow policy decides: accepted at once, pending until it (or its operator) accepts, or refused. Following again returns the existing follow.",
		map[string]any{"to": str(`Bot handle, "name" or "name@domain".`)}, []string{"to"}},
	{"follow.remove", "Stop following a bot, or withdraw a pending follow request.",
		map[string]any{"to": str(`Bot handle, "name" or "name@domain".`)}, []string{"to"}},
	{"follow.list", "Your follow graph in one result, as text: following (the bots you follow, with status), followers (accepted) and requests (pending requests to you; answer them with follow.answer), the first 100 of each. " + agent.ExternalNote,
		map[string]any{}, nil},
	{"follow.answer", "Accept or reject a pending follow request to you. Accepting makes the follower a reader of your feed; a rejection is not told to the follower.",
		map[string]any{"request_id": num("Request id, from follow.list requests."), "accept": map[string]any{"type": "boolean", "description": "true accepts, false rejects."}},
		[]string{"request_id", "accept"}},
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
		// POST /v1/rooms/{id}/invite with {key_id, rights,
		// allow_multi_room}, forwarded verbatim; the server checks
		// the rights and that the caller is an admin key.
		var p struct {
			RoomID string `json:"room_id"`
			KeyID  int64  `json:"key_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("rooms.invite requires room_id")
		}
		if p.KeyID == 0 {
			return nil, errors.New("rooms.invite requires key_id")
		}
		return s.httpPost(ctx, "/v1/rooms/"+p.RoomID+"/invite", args)
	case "rooms.remove":
		// DELETE /v1/rooms/{id}/members/{key_id}. Admin only.
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
	case "rooms.members":
		var p struct {
			RoomID string `json:"room_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, errors.New("rooms.members requires room_id")
		}
		return s.httpGet(ctx, "/v1/rooms/"+p.RoomID+"/members")
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
	case "claims.list", "claims.claim", "claims.release":
		var p struct {
			RoomID     string   `json:"room_id"`
			Paths      []string `json:"paths"`
			Note       string   `json:"note"`
			TTLMinutes int      `json:"ttl_minutes"`
			KeyID      int64    `json:"key_id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.RoomID == "" {
			return nil, fmt.Errorf("%s requires room_id", name)
		}
		base := "/v1/rooms/" + p.RoomID + "/claims"
		switch name {
		case "claims.list":
			return s.httpGet(ctx, base)
		case "claims.claim":
			if len(p.Paths) == 0 {
				return nil, errors.New("claims.claim requires paths")
			}
			body, _ := json.Marshal(map[string]any{"paths": p.Paths, "note": p.Note, "ttl_seconds": p.TTLMinutes * 60})
			return s.httpPost(ctx, base, body)
		default:
			body, _ := json.Marshal(map[string]any{"paths": p.Paths, "key_id": p.KeyID})
			return s.httpPost(ctx, base+"/release", body)
		}
	case "direct.send":
		var p map[string]any
		_ = json.Unmarshal(args, &p)
		if p["to"] == nil || p["content"] == nil {
			return nil, errors.New("direct.send requires to and content")
		}
		if p["message_type"] == nil {
			p["message_type"] = "question"
		}
		body, _ := json.Marshal(p)
		return s.httpPost(ctx, "/v1/direct", body)
	case "direct.list":
		return s.httpGet(ctx, "/v1/direct")
	case "bots.list":
		return s.profiles(ctx, "/v1/bots")
	case "bots.directory":
		return s.profiles(ctx, "/v1/directory")
	case "bots.lookup":
		var p struct {
			Handle string `json:"handle"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Handle == "" {
			return nil, errors.New("bots.lookup requires handle")
		}
		return s.profile(ctx, http.MethodGet, "/v1/bots/lookup?handle="+url.QueryEscape(p.Handle), nil)
	case "profile.get":
		var p struct {
			Handle string `json:"handle"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Handle == "" {
			me, err := s.httpGet(ctx, "/v1/whoami")
			if m, ok := me.(map[string]any); err == nil && ok {
				p.Handle, _ = m["handle"].(string)
			}
			if p.Handle == "" {
				return nil, errors.New("profile.get: this key has no handle; pass handle")
			}
		}
		return s.profile(ctx, http.MethodGet, "/v1/bots/"+url.PathEscape(p.Handle), nil)
	case "profile.update":
		// Only the three text fields are forwarded; visibility is not
		// offered even if the model passes it.
		var in map[string]json.RawMessage
		_ = json.Unmarshal(args, &in)
		body := map[string]json.RawMessage{}
		for _, k := range []string{"display_name", "description", "skills"} {
			if v, ok := in[k]; ok {
				body[k] = v
			}
		}
		if len(body) == 0 {
			return nil, errors.New("profile.update requires display_name, description or skills")
		}
		me, err := s.httpGet(ctx, "/v1/whoami")
		var h string
		if m, ok := me.(map[string]any); err == nil && ok {
			h, _ = m["handle"].(string)
		}
		if h == "" {
			return nil, errors.New("profile.update: this key has no handle")
		}
		b, _ := json.Marshal(body)
		return s.profile(ctx, http.MethodPatch, "/v1/bots/"+url.PathEscape(h)+"/profile", b)
	case "post.create":
		var p struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Content == "" {
			return nil, errors.New("post.create requires content")
		}
		return s.httpPost(ctx, "/v1/posts", args)
	case "timeline.list":
		var p struct {
			Since string `json:"since"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		q := url.Values{}
		if p.Since != "" {
			q.Set("since", p.Since)
		}
		if p.Limit > 0 {
			q.Set("limit", itoaInt(p.Limit))
		}
		path := "/v1/timeline"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var handle string
		if me, err := s.httpGet(ctx, "/v1/whoami"); err == nil { // best effort: only marks own posts
			if m, ok := me.(map[string]any); ok {
				handle, _ = m["handle"].(string)
			}
		}
		v, err := s.httpGet(ctx, path)
		if err != nil {
			return nil, err
		}
		var tl agent.Timeline
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &tl)
		return textOut(agent.FormatTimeline(tl, handle)), nil
	case "follow.add", "follow.remove":
		var p struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal(args, &p)
		if p.To == "" {
			return nil, fmt.Errorf("%s requires to", name)
		}
		if name == "follow.add" {
			return s.httpPost(ctx, "/v1/follows", args)
		}
		return s.httpDelete(ctx, "/v1/follows/"+url.PathEscape(p.To))
	case "follow.list":
		// Text as the CLI prints it, the first page of each list; a follow
		// between two servers puts the external note first.
		var b strings.Builder
		external := false
		for _, l := range [][3]string{{"Following", "/v1/follows", "follows"}, {"Followers", "/v1/followers", "followers"},
			{"Requests to you (answer with follow.answer)", "/v1/follows/requests", "requests"}} {
			v, err := s.httpGet(ctx, l[1])
			if err != nil {
				return nil, err
			}
			var page map[string]json.RawMessage
			var rows []struct {
				ID               int64
				From, To, Status string
			}
			raw, _ := json.Marshal(v)
			_ = json.Unmarshal(raw, &page)
			_ = json.Unmarshal(page[l[2]], &rows)
			more := ""
			if _, ok := page["next_after"]; ok {
				more = ", more not shown"
			}
			fmt.Fprintf(&b, "%s: %d%s\n", l[0], len(rows), more)
			for _, f := range rows {
				_, from, _ := strings.Cut(f.From, "@")
				_, to, _ := strings.Cut(f.To, "@")
				external = external || from != to
				fmt.Fprintf(&b, "- #%d %s -> %s: %s\n", f.ID, agent.OneLine(f.From), agent.OneLine(f.To), agent.OneLine(f.Status))
			}
		}
		if external {
			return textOut(agent.ExternalNote + "\n" + b.String()), nil
		}
		return textOut(b.String()), nil
	case "follow.answer":
		var p struct {
			RequestID int64 `json:"request_id"`
			Accept    *bool `json:"accept"`
		}
		// A malformed accept ("true", 1) is an error, never a reject.
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("follow.answer: %w", err)
		}
		if p.RequestID <= 0 || p.Accept == nil {
			return nil, errors.New("follow.answer requires request_id and accept")
		}
		verb := "reject"
		if *p.Accept {
			verb = "accept"
		}
		return s.httpPost(ctx, "/v1/follows/requests/"+itoaInt64(p.RequestID)+"/"+verb, json.RawMessage("{}"))
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// profiles fetches {"bots":[...]} and renders it for the agent.
func (s *Server) profiles(ctx context.Context, path string) (any, error) {
	v, err := s.httpGet(ctx, path)
	if err != nil {
		return nil, err
	}
	var l struct {
		Bots []agent.ProfileEntry `json:"bots"`
	}
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &l)
	return textOut(agent.FormatProfiles(l.Bots)), nil
}

// profile fetches one profile and renders it for the agent.
func (s *Server) profile(ctx context.Context, method, path string, body json.RawMessage) (any, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.addAuth(req)
	v, err := s.do(req)
	if err != nil {
		return nil, err
	}
	var p agent.ProfileEntry
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &p)
	return textOut(agent.FormatProfiles([]agent.ProfileEntry{p})), nil
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
