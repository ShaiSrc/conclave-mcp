package messages

// `conclave-mcp status [room]`: what is going on in a room, for the
// human watching the agents.

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/claims"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// now is the clock, a seam for tests.
var now = time.Now

// NewStatusCmd builds `status [room] [--since 24h]`.
func NewStatusCmd() *cobra.Command {
	var flagSince time.Duration
	cmd := &cobra.Command{
		Use:   "status [room]",
		Short: "Show open requests, agents and claims of a room",
		Long: `Shows what is going on in a room:

- Requests without a reply: questions, tasks, handoffs, alerts and
  decisions of the last --since (24h) that no other agent answered
  with reply_to, oldest first.
- Agents: every key that wrote in the room, with its last message.
- Active claims.

` + roomHelp,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				roomID, err := roomFor(ctx, c, args)
				if err != nil {
					return err
				}
				msgs, err := agent.ListAll(ctx, c, roomID, "")
				if err != nil {
					return mapErr(err)
				}
				var held struct {
					Claims []claims.Claim `json:"claims"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms/" + roomID + "/claims", Out: &held}); err != nil {
					return mapErr(err)
				}
				return renderStatus(out, c.BaseURL, agent.LoadRoom(ctx, c, roomID, ""), roomID, msgs, held.Claims, flagSince)
			})
		},
	}
	cmd.Flags().DurationVar(&flagSince, "since", 24*time.Hour, "how far back to look for requests without a reply")
	return cmd
}

// renderStatus writes the three status sections to out.
func renderStatus(out io.Writer, server string, room agent.Room, roomID string, msgs []agent.Msg, held []claims.Claim, since time.Duration) error {
	n, labels := now(), room.Labels
	fmt.Fprintf(out, "Room %s (%s) on %s", agent.OneLine(room.Name), roomID, server)
	if room.Home != "" {
		fmt.Fprintf(out, ", hosted by %s", agent.OneLine(room.Home))
	}
	fmt.Fprint(out, "\n\n")

	cutoff := n.Add(-since)
	var open []agent.Msg
	for _, m := range agent.Unanswered(msgs) {
		if t, err := time.Parse(time.RFC3339Nano, m.TS); err == nil && t.After(cutoff) {
			open = append(open, m)
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Requests without a reply (last %s):\n", short(since))
	if len(open) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, m := range open {
		fmt.Fprintf(w, "  %s#%d\t%s\t%s\t%s\t%s\n", ext(m), m.ID, ago(m.TS, n), agent.Who(m, labels), m.MessageType, excerpt(m.Content, 60))
	}

	// Last message per author, most recent first.
	var order []int64
	last := map[int64]agent.Msg{}
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, seen := last[msgs[i].Author]; !seen {
			last[msgs[i].Author] = msgs[i]
			order = append(order, msgs[i].Author)
		}
	}
	fmt.Fprintln(w, "\nAgents (last message in this room):")
	if len(order) == 0 {
		fmt.Fprintln(w, "  nobody has written yet")
	}
	for _, id := range order {
		m := last[id]
		typ := m.MessageType
		if typ == "" {
			typ = "-"
		}
		fmt.Fprintf(w, "  %s%s\t%s\t%s\n", ext(m), agent.Who(m, labels), ago(m.TS, n), typ)
	}

	fmt.Fprintln(w, "\nActive claims:")
	if len(held) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, c := range held {
		until := c.ExpiresAt
		if t, err := time.Parse(time.RFC3339Nano, c.ExpiresAt); err == nil {
			until = t.Local().Format("15:04")
		}
		pre := ""
		if c.TrustOrigin == "external" {
			pre = "[EXT] "
		}
		fmt.Fprintf(w, "  %s%s\t%s\tuntil %s\t%s\n", pre, agent.OneLine(c.Path), agent.OneLine(c.Label), agent.OneLine(until), agent.OneLine(c.Note))
	}
	return w.Flush()
}

// short renders d as "24h" or "90m" when it is whole.
func short(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// ago renders ts relative to n: "just now", "45m ago", "10h ago",
// "3d ago"; ts itself if it does not parse.
func ago(ts string, n time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := n.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ext marks an entry from an external key or the operator.
func ext(m agent.Msg) string {
	if m.Operator {
		return "[OPERATOR] "
	}
	if m.External() {
		return "[EXT] "
	}
	return ""
}
