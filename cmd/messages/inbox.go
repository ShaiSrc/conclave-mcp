package messages

// `conclave-mcp messages inbox [room]`: new messages from other
// agents since the last call, for scripts, background waiters
// (--wait) and older hook setups. Hooks use `conclave-mcp hook`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// newInboxCmd builds `messages inbox [room] [--wait D] [--state FILE]`.
func newInboxCmd() *cobra.Command {
	var flagState string
	var flagWait time.Duration
	cmd := &cobra.Command{
		Use:   "inbox [room]",
		Short: "Print new messages from other agents since the last call (for agent hooks)",
		Long: `Prints messages from other keys that arrived since the previous
call and advances a cursor (one file per key and room, in the user
cache dir unless --state is given). Prints nothing when there is
nothing new. The first call shows the latest 20 messages.

--wait 8h blocks until another agent writes (or the time is up, then
it prints nothing), so an agent can run it in the background and be
woken by its exit.

When stdin carries Claude Code hook JSON, the output is the hook's
additionalContext envelope. For new setups prefer ` + "`conclave-mcp hook`" + `.

` + roomHelp,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			event := agent.ReadHookInput(cmd.InOrStdin()).Event
			// Exit 2 would make Claude Code block the prompt or the tool
			// call, so every failure is a plain exit 1.
			err := makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				statePath := flagState
				if statePath == "" {
					statePath = agent.StatePath("", c.Token, roomID)
				}
				news, err := agent.WaitFor(ctx, c, roomID, statePath, flagWait, agent.Any)
				if err != nil || len(news) == 0 {
					return err
				}
				text := agent.Format(agent.LoadRoom(ctx, c, roomID, roomID), news)
				if event == "" {
					_, err := fmt.Fprint(out, text)
					return err
				}
				return json.NewEncoder(out).Encode(agent.ContextOutput(event, text))
			})
			if err != nil {
				return &exitCodeError{code: 1, err: err}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagState, "state", "", "cursor file (default: <user cache dir>/conclave-mcp/inbox-<key prefix>-<room>.json)")
	cmd.Flags().DurationVar(&flagWait, "wait", 0, "wait up to this long (e.g. 30s, 8h) for a message from another agent")
	return cmd
}
