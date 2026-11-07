package messages

// `conclave-mcp messages inbox <room-id>`: new messages from other
// agents since the last call, for scripts and older hook setups.
// New setups use `conclave-mcp hook` (see cmd/hook).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

func newInboxCmd() *cobra.Command {
	var flagState string
	cmd := &cobra.Command{
		Use:   "inbox <room-id>",
		Short: "Print new messages from other agents since the last call (for agent hooks)",
		Long: `Prints messages from other keys that arrived since the previous
call and advances a cursor (one file per key and room, in the user
cache dir unless --state is given). Prints nothing when there is
nothing new. The first call shows the latest 20 messages.

When stdin carries Claude Code hook JSON, the output is the hook's
additionalContext envelope. For new setups prefer ` + "`conclave-mcp hook`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			roomID := args[0]
			event := agent.ReadHookInput(cmd.InOrStdin()).Event
			// Exit 2 would make Claude Code block the prompt or the tool
			// call, so every failure is a plain exit 1.
			err := makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				statePath := flagState
				if statePath == "" {
					statePath = agent.StatePath("", c.Token, roomID)
				}
				unlock, err := agent.LockState(statePath)
				if err != nil {
					return err
				}
				defer unlock()
				st := agent.LoadState(statePath)
				news, err := agent.FetchNew(ctx, c, roomID, &st)
				if err != nil {
					return err
				}
				if err := agent.SaveState(statePath, st); err != nil {
					return err
				}
				if len(news) == 0 {
					return nil
				}
				text := agent.Format(roomID, news, agent.Labels(ctx, c))
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
	return cmd
}
