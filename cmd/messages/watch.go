package messages

// `conclave-mcp watch [room]`: a room's conversation, live, for the
// human watching the agents.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// watchRetry is the pause before reconnecting after an error.
var watchRetry = 5 * time.Second

// notify shows a desktop notification. Title and body go to the
// notifier as arguments, never into a script, so message text cannot
// run anything.
var notify = func(title, body string) error {
	if err := canNotify(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", title, body).Run()
	}
	return exec.Command("notify-send", "--", title, body).Run()
}

// canNotify reports whether this machine has a notifier.
var canNotify = func() error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	if _, err := exec.LookPath("notify-send"); err != nil {
		return errors.New("--notify needs osascript (macOS) or notify-send")
	}
	return nil
}

// NewWatchCmd builds `watch [room] [--last N] [--notify]`.
func NewWatchCmd() *cobra.Command {
	var flagLast int
	var flagNotify bool
	cmd := &cobra.Command{
		Use:   "watch [room]",
		Short: "Follow a room's messages live (Ctrl-C to stop)",
		Long: `Prints the last --last messages of a room, then every new one as it
arrives, with author, type, the #id it answers and full content. Your
own messages are marked (you). Reconnects by itself if the server
restarts.

--notify also shows a desktop notification (macOS, or notify-send)
for every question, task, handoff, alert or decision from another
agent.

` + roomHelp,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagNotify {
				if err := canNotify(); err != nil {
					return &exitCodeError{code: 2, err: err}
				}
			}
			errOut := cmd.ErrOrStderr()
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				var me agent.State
				if err := agent.EnsureSelf(ctx, c, &me); err != nil {
					return mapErr(err)
				}
				all, err := agent.ListAll(ctx, c, roomID, "")
				if err != nil {
					return mapErr(err)
				}
				labels := agent.Labels(ctx, c)
				var lastID int64
				since := ""
				show := func(m agent.Msg, live bool) {
					if m.ID <= lastID {
						return // already shown (the stream opens before the catch-up read)
					}
					lastID, since = m.ID, m.TS
					if _, ok := labels[m.Author]; !ok {
						labels = agent.Labels(ctx, c) // a new agent
					}
					if t, err := time.Parse(time.RFC3339Nano, m.TS); err == nil {
						m.TS = t.Local().Format("2006-01-02 15:04:05")
					}
					fmt.Fprint(out, agent.Line(m, labels, me.SelfID))
					if live && flagNotify && agent.Actionable(m) && m.Author != me.SelfID {
						title := fmt.Sprintf("Conclave: %s <%s>", agent.Who(m, labels), m.MessageType)
						if err := notify(title, excerpt(m.Content, 200)); err != nil {
							fmt.Fprintf(errOut, "conclave-mcp watch: notification failed: %v\n", err)
						}
					}
				}
				for i := max(0, len(all)-flagLast); i < len(all); i++ {
					show(all[i], false)
				}
				if len(all) > 0 { // older messages count as shown
					lastID, since = all[len(all)-1].ID, all[len(all)-1].TS
				}
				fmt.Fprintf(errOut, "Watching room %s, Ctrl-C to stop.\n", roomName(ctx, c, roomID))
				for {
					err := follow(ctx, c, roomID, since, func(m agent.Msg) { show(m, true) })
					if ctx.Err() != nil {
						return nil
					}
					if err != nil {
						fmt.Fprintf(errOut, "conclave-mcp watch: %v; reconnecting in %s\n", err, watchRetry)
						select {
						case <-ctx.Done():
							return nil
						case <-time.After(watchRetry):
						}
					}
				}
			})
		},
	}
	cmd.Flags().IntVar(&flagLast, "last", 10, "how many earlier messages to show first")
	cmd.Flags().BoolVar(&flagNotify, "notify", false, "desktop notification for questions, tasks, handoffs, alerts and decisions from others")
	return cmd
}

// follow subscribes once, reads what arrived since `since`, then
// hands every new message to show until the stream ends (the server
// ends it after its wait limit; the caller subscribes again).
func follow(ctx context.Context, c *client.Client, roomID, since string, show func(agent.Msg)) error {
	stream, err := agent.Subscribe(ctx, c, roomID, 5*time.Minute)
	if err != nil {
		return err
	}
	defer stream.Close()
	missed, err := agent.ListAll(ctx, c, roomID, since)
	if err != nil {
		return err
	}
	for _, m := range missed {
		show(m)
	}
	for {
		m, ok, err := stream.Next()
		if !ok {
			return err
		}
		show(m)
	}
}
