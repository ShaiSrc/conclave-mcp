package messages

// `conclave-mcp messages thread [room] <msg-id>`: one conversation,
// question and replies together, optionally waiting for the next reply.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// newThreadCmd builds `messages thread [room] <msg-id> [--wait D] [--format text|json]`.
func newThreadCmd() *cobra.Command {
	var flagWait time.Duration
	var flagFormat string
	cmd := &cobra.Command{
		Use:   "thread [room] <msg-id>",
		Short: "Print a message with all replies to it (and wait for the next reply)",
		Long: `Prints the conversation a message belongs to, oldest first, with
full content: the message it started from and every reply to it
(reply_to), directly or through other replies. Your own messages are
marked (you).

--wait 1h: if the newest message in the thread is yours, waits up to
1h for another agent to reply, then prints the thread; prints nothing
when the time is up. If the last message is already someone else's,
prints at once.

` + roomHelp,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[len(args)-1], 10, 64)
			if err != nil || id <= 0 {
				return &exitCodeError{code: 2, err: fmt.Errorf("msg-id must be a positive integer, got %q", args[len(args)-1])}
			}
			if flagFormat != "text" && flagFormat != "json" {
				return &exitCodeError{code: 2, err: fmt.Errorf("unknown --format %q (want text or json)", flagFormat)}
			}
			err = makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args[:len(args)-1])
				if err != nil {
					return err
				}
				var me agent.State
				if err := agent.EnsureSelf(ctx, c, &me); err != nil {
					return err
				}
				var thread []agent.Msg
				done, err := agent.Await(ctx, c, roomID, flagWait, func(ctx context.Context) (int64, bool, error) {
					all, err := agent.ListAll(ctx, c, roomID, "")
					if err != nil {
						return me.SelfID, false, err
					}
					if thread = agent.Thread(all, id); thread == nil {
						return me.SelfID, false, &exitCodeError{code: 1, err: fmt.Errorf("no message #%d in this room (or it was deleted)", id)}
					}
					return me.SelfID, flagWait <= 0 || thread[len(thread)-1].Author != me.SelfID, nil
				})
				if err != nil || !done {
					return err
				}
				room := agent.LoadRoom(ctx, c, roomID, roomID)
				if flagFormat == "json" {
					for i := range thread {
						thread[i].AuthorLabel = room.Labels[thread[i].Author]
					}
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(thread)
				}
				_, err = fmt.Fprint(out, agent.FormatThread(room, thread, me.SelfID))
				return err
			})
			var ec *exitCodeError
			if errors.As(err, &ec) {
				return err
			}
			return mapErr(err)
		},
	}
	cmd.Flags().DurationVar(&flagWait, "wait", 0, "if the last message is yours, wait up to this long (e.g. 30s, 1h) for a reply")
	cmd.Flags().StringVar(&flagFormat, "format", "text", "output format: text or json")
	return cmd
}
