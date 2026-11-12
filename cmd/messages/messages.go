// Package messages implements the `conclave-mcp messages` subcommand.
//
// Per SPEC-0004 §5:
//
//	conclave-mcp messages send [room] [--content "..." | --content-file PATH | stdin]
//	conclave-mcp messages list [room] [--since <ts>] [--limit N] [--format json|table]
//	conclave-mcp messages delete <room> <msg-id>
//	conclave-mcp messages subscribe [room] [--wait SECONDS] [--format json|table]
//	conclave-mcp messages inbox [room] [--wait DURATION]
//	conclave-mcp messages thread [room] <msg-id> [--wait DURATION] [--format text|json]
//
// [room] is a room id or name; without it, CONCLAVE_ROOM, then the
// room in the repository's .conclave.json (agent.RoomID).
//
// Messages are streamed, never typed twice: when --content-file is "-"
// or no flag is given, the message body is read from stdin.
package messages

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
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

const roomHelp = `ROOM is a room id or a room name. Without it: $CONCLAVE_ROOM, then the
room in the .conclave.json of the repository you are in.`

// roomFor resolves the optional room argument (agent.RoomID).
func roomFor(ctx context.Context, c *client.Client, args []string) (string, error) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	id, err := agent.RoomID(ctx, c, arg)
	if errors.Is(err, agent.ErrNoRoom) {
		return "", &exitCodeError{code: 1, err: err}
	}
	if err != nil {
		return "", mapErr(err)
	}
	return id, nil
}

// NewCmd builds the messages subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "messages",
		Short: "Send and list messages in a room",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newSendCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newDeleteCmd())
	cmd.AddCommand(newSubscribeCmd())
	cmd.AddCommand(newInboxCmd())
	cmd.AddCommand(newThreadCmd())
	return cmd
}

// msgView mirrors the JSON shape returned by /v1/rooms/{id}/messages.
// IDs are int64 to match the server's messageView.ID and api_keys.id
// (SPEC-0002 §3.2.1, §3.5.1). Earlier CLI versions declared ID as
// string; that drifted from the server, which the in-process
// unit tests (with mock servers sending string ids) missed. Smoke
// test on 2026-06-28 caught it.
type msgView struct {
	ID          int64  `json:"id"`
	RoomID      string `json:"room_id"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	// TS is the wire-format name on the server (SPEC-0002
	// §3.5.1). Earlier CLI versions called it CreatedAt and
	// used the json tag "created_at"; the server has always
	// returned "ts", so the tag drifted. Smoke test on
	// 2026-06-28 caught it. SPEC-0002 §3.5.1 uses "ts" too.
	TS string `json:"ts"`
	// TrustOrigin (SPEC-0001 Q11 / ADR-0008) is always present
	// in list responses; the server denormalises it from the
	// author_key at send time. The server is a dumb pipe, so
	// the CLI just surfaces the value for human inspection.
	TrustOrigin string `json:"trust_origin"`
	// Content is NOT included in list responses per SPEC-0003 §5.
	// The send response includes it for the sender's confirmation.
	Content string `json:"content,omitempty"`
}

type sendResponse struct {
	msgView
}

type listResponse struct {
	Messages  []agent.Msg `json:"messages"`
	NextSince string      `json:"next_since"`
}

// openClient is a seam for tests. The default opens from cmd flags;
// tests overwrite it before calling RunE.
var openClient = func(_ context.Context, cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

// makeClientAndDo opens a client and runs the action with a
// connected client and a STDOUT-bound writer.
func makeClientAndDo(ctx context.Context, cmd *cobra.Command, action func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(ctx, cmd)
	if err != nil {
		return err
	}
	return action(ctx, c, cmd.OutOrStdout())
}

// exitCodeError is a sentinel: tests (and the cobra glue) map
// it to os.Exit(<code>) via the ExitCode helper below.
type exitCodeError struct {
	code int
	err  error // optional cause, printed by main
}

func (e *exitCodeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

// resetStdinFn is the test seam for stdin swaps. Production
// code never calls this; tests use ResetStdin to inject a
// strings.Reader and the production code reads from
// stdinOverride when non-nil.
//
// We avoid assigning to os.Stdin because *os.File is a
// concrete type and our test reader is an io.Reader. The
// production code path checks stdinOverride first.
var stdinOverride io.Reader

// ResetStdin is the public test seam. Pass a non-nil reader
// to inject; pass nil to clear.
func ResetStdin(r io.Reader) { stdinOverride = r }

// ExitCode extracts the exit code from an error returned by a
// RunE function, or 0 if the error doesn't carry one. This is
// the test seam for the "exit N" checks below.
func ExitCode(err error) int {
	var e *exitCodeError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}

func exitWith(code int) error { return &exitCodeError{code: code} }

// mapErr converts a client error into a process exit code per
// SPEC-0004 §6: nil -> 0, *client.NetError -> 3 (transport
// failure), *client.APIError -> 1 (4xx/5xx business response),
// any other error -> 2 (programming / config error).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var netErr *client.NetError
	if errors.As(err, &netErr) {
		return &exitCodeError{code: 3, err: err}
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return &exitCodeError{code: 1, err: err}
	}
	return &exitCodeError{code: 2, err: err}
}

// ---------- send ----------

func newSendCmd() *cobra.Command {
	var (
		flagContent     string
		flagContentFile string
		flagContentType string
		flagMessageType string
		flagMetadata    string
		flagFormat      string
		flagReplyTo     int64
	)
	cmd := &cobra.Command{
		Use:   "send [room]",
		Short: "Send a message to a room",
		Long:  "Send a message to a room.\n\n" + roomHelp,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content, err := resolveContent(flagContent, flagContentFile)
			if err != nil {
				// --content and --content-file both set,
				// or stdin read failure. These are
				// programming/config errors from the
				// caller's side.
				return exitWith(2)
			}
			req := struct {
				Content     string          `json:"content"`
				ContentType string          `json:"content_type,omitempty"`
				MessageType string          `json:"message_type,omitempty"`
				Metadata    json.RawMessage `json:"metadata,omitempty"`
				ReplyTo     *int64          `json:"reply_to,omitempty"`
			}{
				Content:     content,
				ContentType: flagContentType,
				MessageType: flagMessageType,
			}
			if flagMetadata != "" {
				req.Metadata = json.RawMessage(flagMetadata)
			}
			if cmd.Flags().Changed("reply-to") {
				req.ReplyTo = &flagReplyTo
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				var resp sendResponse
				status, err := c.Do(ctx, client.Request{
					Method: "POST",
					Path:   "/v1/rooms/" + roomID + "/messages",
					Body:   req,
					Out:    &resp,
				})
				if err != nil {
					return mapErr(err)
				}
				// 201 (created) and 200 (ok, with body)
				// are both valid for a successful send.
				// The server side returns 201; tests that
				// mock the server may return 200.
				if status != http.StatusCreated && status != http.StatusOK {
					return mapErr(fmt.Errorf("server returned %d", status))
				}
				if flagFormat == "json" {
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(resp.msgView)
				}
				// Default: JSON. The plaintext path was
				// too quiet for scripting; the JSON
				// envelope is the contract. Use
				// --format=plain for piping.
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.msgView)
			})
		},
	}
	cmd.Flags().StringVar(&flagContent, "content", "", "message body (mutually exclusive with --content-file)")
	cmd.Flags().StringVar(&flagContentFile, "content-file", "", "read message body from file (use '-' for stdin)")
	cmd.Flags().StringVar(&flagContentType, "content-type", "text/plain", "MIME type of the message body")
	cmd.Flags().StringVar(&flagMessageType, "message-type", "", "intent tag (SPEC-0002 §3.5.1: question, answer, decision, handoff, status_update, task_assignment, alert, system)")
	cmd.Flags().StringVar(&flagMetadata, "metadata", "", "raw JSON metadata to attach to the message")
	cmd.Flags().Int64Var(&flagReplyTo, "reply-to", 0, "id of the message this answers (same room; stored as metadata.reply_to)")
	cmd.Flags().StringVar(&flagFormat, "format", "", "output format: json (default is plaintext body + status line)")
	return cmd
}

// resolveContent picks the body from --content, --content-file,
// or stdin (in that order). At most one of --content /
// --content-file may be set; if neither is set, the body is
// read from stdin up to EOF.
func resolveContent(contentFlag, fileFlag string) (string, error) {
	if contentFlag != "" && fileFlag != "" {
		return "", errors.New("--content and --content-file are mutually exclusive")
	}
	if contentFlag != "" {
		return contentFlag, nil
	}
	if fileFlag != "" {
		if fileFlag == "-" {
			b, err := io.ReadAll(stdinOrStdin())
			if err != nil {
				return "", fmt.Errorf("read stdin: %w", err)
			}
			return string(b), nil
		}
		b, err := os.ReadFile(fileFlag)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fileFlag, err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(stdinOrStdin())
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(b), nil
}

// stdinOrStdin returns the test override if set, otherwise
// os.Stdin. Keeps the production code path free of *os.File
// type assertions.
func stdinOrStdin() io.Reader {
	if stdinOverride != nil {
		return stdinOverride
	}
	return os.Stdin
}

// ---------- list ----------

func newListCmd() *cobra.Command {
	var (
		flagSince       string
		flagLimit       int
		flagFormat      string
		flagTrustOrigin string
	)
	cmd := &cobra.Command{
		Use:   "list [room]",
		Short: "List messages in a room, oldest first, paginated by timestamp",
		Long:  "List messages in a room, oldest first, paginated by timestamp.\n\n" + roomHelp,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagTrustOrigin != "" && flagTrustOrigin != "internal" && flagTrustOrigin != "external" {
				return fmt.Errorf("--trust-origin must be 'internal' or 'external'")
			}
			q := url.Values{}
			if flagSince != "" {
				q.Set("since", flagSince)
			}
			if flagLimit > 0 {
				q.Set("limit", strconv.Itoa(flagLimit))
			}
			if flagTrustOrigin != "" {
				q.Set("trust_origin", flagTrustOrigin)
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				path := "/v1/rooms/" + url.PathEscape(roomID) + "/messages"
				if len(q) > 0 {
					path += "?" + q.Encode()
				}
				var resp listResponse
				status, err := c.Do(ctx, client.Request{Method: "GET", Path: path, Out: &resp})
				if err != nil {
					return err
				}
				if status != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", status)
				}
				return renderMessageList(flagFormat, resp.Messages, agent.Labels(ctx, c, roomID), out)
			})
		},
	}
	cmd.Flags().StringVar(&flagSince, "since", "", "return messages after this RFC 3339 timestamp (pass back next_since)")
	cmd.Flags().IntVar(&flagLimit, "limit", 100, "max messages to return (server caps at 500)")
	cmd.Flags().StringVar(&flagFormat, "format", "table", "output format: json or table")
	cmd.Flags().StringVar(&flagTrustOrigin, "trust-origin", "", "filter by trust_origin (internal|external)")
	return cmd
}

// renderMessageList writes msgs as a table (author labels from
// labels) or as a JSON array with every field plus the author label.
func renderMessageList(format string, msgs []agent.Msg, labels map[int64]string, out io.Writer) error {
	switch format {
	case "json":
		// Per CLI tests, --format=json emits just the
		// messages array. The listResponse wrapper is an
		// HTTP-level concept; the CLI surface is a flat
		// list of messages for piping into `jq`.
		for i := range msgs {
			msgs[i].AuthorLabel = labels[msgs[i].Author]
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(msgs)
	case "table", "":
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tWHEN\tFROM\tTYPE\tRE\tTRUST\tCONTENT")
		for _, m := range msgs {
			typ, re := "-", "-"
			if m.MessageType != "" {
				typ = m.MessageType
			}
			if r := agent.ReplyTo(m); r > 0 {
				re = fmt.Sprintf("#%d", r)
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.ID, m.TS, agent.Who(m, labels), typ, re, m.TrustOrigin, excerpt(m.Content, 50))
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown --format %q (want json or table)", format)
	}
}

// excerpt is s on one line, without control characters (no escape
// sequences from other agents reach the terminal), cut after n runes.
// The full text: messages thread, or --format json.
func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---------- delete ----------

// newDeleteCmd soft-deletes a single message (SPEC-0001 Q8).
// Admin key only.
func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <room> <msg-id>",
		Short: "Soft-delete a message (admin key only)",
		Long:  "Soft-delete a message (admin key only). <room> is a room id or name.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			msgID, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || msgID <= 0 {
				return fmt.Errorf("msg-id must be a positive integer")
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args[:1])
				if err != nil {
					return err
				}
				status, err := c.Do(ctx, client.Request{
					Method: "DELETE",
					Path:   fmt.Sprintf("/v1/rooms/%s/messages/%d", roomID, msgID),
				})
				if err != nil {
					return err
				}
				if status != http.StatusNoContent {
					return fmt.Errorf("expected 204, got %d", status)
				}
				fmt.Fprintf(out, "message deleted: %d\n", msgID)
				return nil
			})
		},
	}
	return cmd
}

// ---------- subscribe ----------

// newSubscribeCmd opens an SSE stream on the server and prints
// arriving messages to STDOUT (ADR-0009). Useful for `tail`-style
// watching, scripting with `conclave-mcp messages subscribe ... | jq`,
// and ad-hoc debugging of the broker.
//
//	conclave-mcp messages subscribe <room-id> [--wait SECONDS]
//
// The server closes the connection after `wait` seconds (default
// 60, max 300). The CLI exits 0 on clean shutdown.
func newSubscribeCmd() *cobra.Command {
	var (
		flagWait   int
		flagFormat string
	)
	cmd := &cobra.Command{
		Use:   "subscribe [room]",
		Short: "Subscribe to a room and print new messages (SSE)",
		Long: `Open an SSE connection to GET /v1/rooms/{id}/events and print
arriving messages to STDOUT. The connection stays open for
up to --wait seconds (default 60, max 300) or until the
caller hits Ctrl-C. Polling via 'conclave-mcp messages list
<room>' is the canonical fallback for scripts that
cannot hold a streaming connection. To wait for the next message
from another agent, use 'messages inbox --wait'.

` + roomHelp,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagWait < 0 {
				return fmt.Errorf("--wait must be >= 0")
			}
			if flagFormat != "" && flagFormat != "json" && flagFormat != "table" {
				return fmt.Errorf("--format must be 'json' or 'table'")
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				// The client resolved server and token (flags, --as, env).
				return streamMessages(ctx, c.BaseURL, c.Token, roomID, flagWait, flagFormat, out)
			})
		},
	}
	cmd.Flags().IntVar(&flagWait, "wait", 60, "how long to keep the connection open (seconds, max 300)")
	cmd.Flags().StringVar(&flagFormat, "format", "json", "output format: json (one line per event) or table (compact summary)")
	return cmd
}

// streamMessages opens the SSE connection, parses the wire format,
// and writes one line per `event: message.created` to out. Heartbeat
// `ping` events are ignored. The function returns nil on clean
// shutdown (server closed the connection at the wait deadline, or
// caller cancelled via context) and a non-nil error on protocol
// violations or transport errors.
func streamMessages(ctx context.Context, server, token, roomID string, waitSecs int, format string, out io.Writer) error {
	if waitSecs == 0 {
		waitSecs = 60
	}
	if waitSecs > 300 {
		waitSecs = 300
	}
	url := fmt.Sprintf("%s/v1/rooms/%s/events?wait=%d", server, roomID, waitSecs)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	// No overall timeout: the server's `wait` controls how
	// long it holds the connection, and the caller can use
	// Ctrl-C to interrupt.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("subscribe: status %d, body: %s", resp.StatusCode, body)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var currentData []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			if len(currentData) > 0 {
				currentData = append(currentData, '\n')
			}
			currentData = append(currentData, line[6:]...)
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(currentData) > 0 {
				currentData = append(currentData, '\n')
			}
			currentData = append(currentData, line[5:]...)
			continue
		}
		// Blank line or any other line ends the event.
		if len(currentData) == 0 {
			continue
		}
		writeEvent(out, currentData, format)
		currentData = currentData[:0]
	}
	if err := scanner.Err(); err != nil {
		// EOF is the normal shutdown signal: the server
		// closed the connection at the wait deadline.
		if err.Error() == "EOF" {
			return nil
		}
		return err
	}
	// Trailing data without a blank line.
	if len(currentData) > 0 {
		writeEvent(out, currentData, format)
	}
	return nil
}

// writeEvent formats and prints a single SSE data payload. JSON
// payloads are echoed verbatim; table format prints a compact
// one-line summary.
func writeEvent(out io.Writer, data []byte, format string) {
	if format == "json" || format == "" {
		_, _ = out.Write(data)
		_, _ = out.Write([]byte("\n"))
		return
	}
	// table: parse the message JSON and print id, ts, content, trust_origin.
	var m struct {
		ID          int64  `json:"id"`
		TS          string `json:"ts"`
		Content     string `json:"content"`
		TrustOrigin string `json:"trust_origin"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		// Unknown event (e.g. ping), just print a marker.
		_, _ = fmt.Fprintf(out, "[event] %s\n", data)
		return
	}
	content := m.Content
	if len(content) > 80 {
		content = content[:77] + "..."
	}
	_, _ = fmt.Fprintf(out, "[%s] #%d %s [%s] %s\n",
		m.TS, m.ID, m.TrustOrigin, m.TS, content)
}
