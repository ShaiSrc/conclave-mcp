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
				news, err := waitNew(ctx, c, roomID, statePath, flagWait)
				if err != nil || len(news) == 0 {
					return err
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
	cmd.Flags().DurationVar(&flagWait, "wait", 0, "wait up to this long (e.g. 30s, 8h) for a message from another agent")
	return cmd
}

// waitNew reads new messages; with wait > 0 and none there, it waits
// for one from another agent. The stream is opened before each read,
// so nothing sent in between is missed, and the state lock is not
// held while waiting (the agent's hooks share the cursor file).
func waitNew(ctx context.Context, c *client.Client, roomID, statePath string, wait time.Duration) ([]agent.Msg, error) {
	deadline := time.Now().Add(wait)
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	for {
		var stream *agent.Stream
		if wait > 0 {
			var err error
			if stream, err = agent.Subscribe(ctx, c, roomID, time.Until(deadline)); err != nil {
				if ctx.Err() != nil {
					return nil, nil // time is up
				}
				return nil, err
			}
		}
		news, self, err := readNew(ctx, c, roomID, statePath)
		if err != nil || len(news) > 0 || wait <= 0 {
			stream.Close()
			return news, err
		}
		_, err = stream.NextFromOther(self)
		stream.Close()
		if ctx.Err() != nil {
			return nil, nil // time is up
		}
		if err != nil {
			return nil, err
		}
		// Another agent wrote, or the server ended the stream at its
		// own wait limit: read again (a hook may have taken it).
	}
}

// readNew is one locked read of the cursor and the messages after it.
func readNew(ctx context.Context, c *client.Client, roomID, statePath string) ([]agent.Msg, int64, error) {
	unlock, err := agent.LockState(statePath)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	st := agent.LoadState(statePath)
	news, err := agent.FetchNew(ctx, c, roomID, &st)
	if err != nil {
		return nil, 0, err
	}
	return news, st.SelfID, agent.SaveState(statePath, st)
}
