// Package direct implements the `conclave-mcp direct` subcommand:
//
//	conclave-mcp direct send <name[@domain]> --content S [--message-type T]
//	conclave-mcp direct list
//	conclave-mcp direct read <name[@domain]>
package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the direct subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "direct", Short: "Direct conversations with other bots", Args: cobra.NoArgs}
	cmd.AddCommand(newSendCmd(), newListCmd(), newReadCmd())
	return cmd
}

// openClient is a seam for tests.
var openClient = func(_ context.Context, cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

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

func run(cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(cmd.Context(), cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	if err := fn(cmd.Context(), c, cmd.OutOrStdout()); err != nil {
		var net *client.NetError
		if errors.As(err, &net) {
			return &exitError{Code: 3, Err: err}
		}
		return &exitError{Code: 1, Err: err}
	}
	return nil
}

func newSendCmd() *cobra.Command {
	var content, msgType string
	cmd := &cobra.Command{
		Use: "send <name[@domain]> --content TEXT", Short: "Send a message to a bot, starting the conversation if needed", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if content == "" {
				return &exitError{Code: 2, Err: errors.New("--content is required")}
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					RoomID  string `json:"room_id"`
					Message struct {
						ID int64 `json:"id"`
					} `json:"message"`
				}
				body := map[string]any{"to": args[0], "content": content, "message_type": msgType}
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/direct", Body: body, Out: &resp}); err != nil {
					return err
				}
				_, err := fmt.Fprintf(out, "sent #%d to %s (room %s)\n", resp.Message.ID, agent.OneLine(args[0]), agent.OneLine(resp.RoomID))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&content, "content", "", "message text")
	cmd.Flags().StringVar(&msgType, "message-type", "question", "message type (a direct member may not send system)")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List your direct conversations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				convs, err := agent.ListDirect(ctx, c)
				if err != nil {
					return err
				}
				var b strings.Builder
				if len(convs) == 0 {
					b.WriteString("No direct conversations.\n")
				}
				for _, cv := range convs {
					last := agent.OneLine(cv.LastMessageAt)
					if last == "" {
						last = "never"
					}
					fmt.Fprintf(&b, "- %s: last %s, %d unread (room %s)\n", agent.OneLine(cv.Address), last, cv.Unread, agent.OneLine(cv.RoomID))
				}
				_, err = io.WriteString(out, b.String())
				return err
			})
		},
	}
}

func newReadCmd() *cobra.Command {
	return &cobra.Command{
		Use: "read <name[@domain]>", Short: "Print a direct conversation (marks it read)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				convs, err := agent.ListDirect(ctx, c)
				if err != nil {
					return err
				}
				var cv *agent.Conversation
				for i := range convs {
					if convs[i].Matches(args[0]) {
						cv = &convs[i]
					}
				}
				if cv == nil {
					return fmt.Errorf("no direct conversation with %q", args[0])
				}
				msgs, err := agent.ListAll(ctx, c, cv.RoomID, "")
				if err != nil {
					return err
				}
				var me agent.State
				if err := agent.EnsureSelf(ctx, c, &me); err != nil {
					return err
				}
				room := agent.LoadRoom(ctx, c, cv.RoomID, "direct with "+cv.Address)
				_, err = io.WriteString(out, agent.FormatDirect(room, cv.Address, msgs, me.SelfID))
				return err
			})
		},
	}
}
