// Package audit implements the `conclave-mcp audit` subcommand.
//
// Per SPEC-0004 §5:
//
//	conclave-mcp audit list [--actor KEY-ID] [--action KIND] [--room-id R] [--since ISO] [--before ID] [--limit N] [--format json|table]
//
// Requires an admin-scoped key on the server. The server returns
// 403 with a clear error if the caller lacks the scope; the CLI
// surfaces that as exit code 1 (server error).
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the audit subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Query the audit log (admin-scope required)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newListCmd())
	return cmd
}

// entry mirrors the JSON shape of /v1/audit entries; --format json
// prints them with the API's field names.
type entry struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	ActorKeyID *int64 `json:"actor_key_id,omitempty"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	RoomID     string `json:"room_id,omitempty"`
	Result     string `json:"result"`
	Detail     string `json:"detail,omitempty"`
}

type listResponse struct {
	Entries    []entry `json:"entries"`
	NextBefore int64   `json:"next_before"`
}

// openClient is a seam for tests.
var openClient = func(_ context.Context, cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

// exitError mirrors cmd/rooms.
type exitError struct {
	Code int
	Err  error
}

func (e *exitError) Error() string { return e.Err.Error() }
func (e *exitError) Unwrap() error { return e.Err }

// ExitCode is exported so cmd/conclave-mcp can use it.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 2
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var net *client.NetError
	if errors.As(err, &net) {
		return &exitError{Code: 3, Err: err}
	}
	return &exitError{Code: 1, Err: err}
}

func makeClientAndDo(ctx context.Context, cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(ctx, cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	out := cmd.OutOrStdout()
	if err := fn(ctx, c, out); err != nil {
		return mapErr(err)
	}
	return nil
}

// ---------- list ----------

func newListCmd() *cobra.Command {
	var (
		flagActor  int64
		flagBefore int64
		flagAction string
		flagSince  string
		flagLimit  int
		flagFormat string
		flagRoomID string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List audit entries (admin-scope required)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if flagActor > 0 {
				q.Set("actor", strconv.FormatInt(flagActor, 10))
			}
			if flagBefore > 0 {
				q.Set("before", strconv.FormatInt(flagBefore, 10))
			}
			if flagAction != "" {
				q.Set("action", flagAction)
			}
			if flagRoomID != "" {
				// SPEC-0001 Q10 + SPEC-0003 §4.4: filter by
				// room id. The server populates audit.room_id
				// for every room/message mutation.
				q.Set("room_id", flagRoomID)
			}
			if flagSince != "" {
				// Validate the timestamp: empty after parse -> error.
				if _, err := time.Parse(time.RFC3339, flagSince); err != nil {
					return &exitError{Code: 2, Err: fmt.Errorf("--since must be RFC3339, got %q: %w", flagSince, err)}
				}
				q.Set("since", flagSince)
			}
			if flagLimit > 0 {
				q.Set("limit", strconv.Itoa(flagLimit))
			}
			path := "/v1/audit"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp listResponse
				_, err := c.Do(ctx, client.Request{Method: "GET", Path: path, Out: &resp})
				if err != nil {
					return err
				}
				if err := renderEntries(flagFormat, resp.Entries, out); err != nil {
					return err
				}
				if resp.NextBefore > 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "more entries: --before %d\n", resp.NextBefore)
				}
				return nil
			})
		},
	}
	cmd.Flags().Int64Var(&flagActor, "actor", 0, "filter by actor key id")
	cmd.Flags().Int64Var(&flagBefore, "before", 0, "only entries older than this entry id (the next page)")
	cmd.Flags().StringVar(&flagAction, "action", "", "filter by action (e.g. message.send, key.create)")
	cmd.Flags().StringVar(&flagRoomID, "room-id", "", "filter by room id (SPEC-0001 Q10)")
	cmd.Flags().StringVar(&flagSince, "since", "", "only entries at or after this ISO-8601 timestamp")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "max entries to return (server default 100, max 1000)")
	cmd.Flags().StringVar(&flagFormat, "format", "table", "output format: table|json")
	return cmd
}

func renderEntries(format string, entries []entry, out io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "table", "":
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tTS\tACTOR\tACTION\tTARGET\tROOM\tRESULT")
		for _, e := range entries {
			actor := "system"
			if e.ActorKeyID != nil {
				actor = strconv.FormatInt(*e.ActorKeyID, 10)
			}
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s/%s\t%s\t%s\n",
				e.ID, e.TS, actor, e.Action, e.TargetType, e.TargetID, e.RoomID, e.Result)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown --format %q (table|json)", format)
	}
}
