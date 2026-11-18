// Package federation implements the `conclave-mcp federation` subcommand:
//
//	conclave-mcp federation status
//	conclave-mcp federation rotate-key
//	conclave-mcp federation forget-old-keys
//	conclave-mcp federation peers list [--format json|table]
//	conclave-mcp federation peers show <domain>
//	conclave-mcp federation peers add <domain> [--fingerprint F] [--note N]
//	conclave-mcp federation peers block|remove|ping|retry <domain>
//	conclave-mcp federation peers limits <domain> [--requests N] [--dm-invites N] [--follow-requests N] [--reset]
//	conclave-mcp federation peers purge <domain> [--yes]
//	conclave-mcp federation peers forget-keys <domain>
//	conclave-mcp federation bots block <address> [--note N]
//	conclave-mcp federation bots unblock <address>
//	conclave-mcp federation bots blocked
package federation

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the federation subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "federation", Short: "Manage federation peers", Args: cobra.NoArgs}
	peers := &cobra.Command{Use: "peers", Short: "Manage federation peers", Args: cobra.NoArgs}
	peers.AddCommand(newListCmd(), newShowCmd(), newAddCmd(), newBlockCmd(), newRemoveCmd(), newPingCmd(), newRetryCmd(), newLimitsCmd(), newPurgeCmd(),
		newForgetKeysCmd())
	bots := &cobra.Command{Use: "bots", Short: "Block single bots of other servers", Args: cobra.NoArgs}
	bots.AddCommand(newBotBlockCmd(), newBotUnblockCmd(), newBotsBlockedCmd())
	cmd.AddCommand(newStatusCmd(), newRotateKeyCmd(), newForgetOldKeysCmd(), peers, bots)
	return cmd
}

type peerView struct {
	Domain      string `json:"domain"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
	KeyID       string `json:"key_id"`
	Note        string `json:"note"`
	AddedAt     string `json:"added_at"`
	LastSeenAt  string `json:"last_seen_at"`
	LastOutAt   string `json:"last_out_at"`
	Health      string `json:"health"`
	Delivery    struct {
		Rooms        int    `json:"rooms"`
		Pending      int64  `json:"pending"`
		FailingSince string `json:"failing_since"`
		Paused       int    `json:"paused"`
		LastError    string `json:"last_error"`
	} `json:"delivery"`
	Limits map[string]struct {
		Value    int  `json:"value"`
		Override bool `json:"override"`
	} `json:"limits"`
}

// limitNames are the limits of a peer, in the order shown.
var limitNames = []string{"requests_per_minute", "dm_invites_per_hour", "follow_requests_per_hour"}

// printPeer prints p, a peer's view, one field a line.
func printPeer(out io.Writer, p peerView) {
	_, _ = fmt.Fprintf(out, "domain:         %s\nstatus:         %s\nkey id:         %s\nfingerprint:    %s\nnote:           %s\n"+
		"added:          %s\nlast seen:      %s\nlast out:       %s\nhealth:         %s\n"+
		"delivery:       %d rooms, %d events pending, %d paused\nfailing since:  %s\nlast error:     %s\nlimits:\n",
		p.Domain, p.Status, p.KeyID, p.Fingerprint, p.Note, p.AddedAt, cmp.Or(p.LastSeenAt, "never"), cmp.Or(p.LastOutAt, "never"), p.Health,
		p.Delivery.Rooms, p.Delivery.Pending, p.Delivery.Paused, cmp.Or(p.Delivery.FailingSince, "-"), cmp.Or(p.Delivery.LastError, "-"))
	for _, name := range limitNames {
		l := p.Limits[name]
		override := ""
		if l.Override {
			override = " (override)"
		}
		_, _ = fmt.Fprintf(out, "  %-24s  %d%s\n", name, l.Value, override)
	}
}

// peerCmd is a command on one peer that sends method to path and
// prints the peer's view it answers, after head when set.
func peerCmd(use, short, method, suffix, head string) *cobra.Command {
	return &cobra.Command{
		Use: use + " <domain>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var p peerView
				if _, err := c.Do(ctx, client.Request{Method: method, Path: peerPath(args[0], suffix), Out: &p}); err != nil {
					return err
				}
				if head != "" {
					_, _ = fmt.Fprintf(out, head+"\n", args[0])
				}
				printPeer(out, p)
				return nil
			})
		},
	}
}

func newShowCmd() *cobra.Command {
	return peerCmd("show", "Show a peer: health, delivery and limits", "GET", "", "")
}

func newRetryCmd() *cobra.Command {
	return peerCmd("retry", "Unpause delivery to a peer and make it due now", "POST", "/retry", "delivery to %s is due now")
}

func newLimitsCmd() *cobra.Command {
	var vals [3]int // in limitNames order
	var reset bool
	flags := [3]string{"requests", "dm-invites", "follow-requests"}
	cmd := &cobra.Command{
		Use: "limits <domain>", Short: "Set a peer's limits (0 refuses); --reset goes back to the defaults", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limits := map[string]any{}
			for i, f := range flags {
				if cmd.Flags().Changed(f) {
					limits[limitNames[i]] = vals[i]
				}
			}
			switch {
			case reset && len(limits) > 0:
				return errors.New("--reset takes no other limit")
			case reset:
				for _, name := range limitNames {
					limits[name] = nil
				}
			case len(limits) == 0:
				return errors.New("give --requests, --dm-invites, --follow-requests or --reset")
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var p peerView
				if _, err := c.Do(ctx, client.Request{Method: "PATCH", Path: peerPath(args[0], ""),
					Body: map[string]any{"limits": limits}, Out: &p}); err != nil {
					return err
				}
				printPeer(out, p)
				return nil
			})
		},
	}
	for i, usage := range []string{"requests a minute", "direct invites an hour", "follow requests an hour"} {
		cmd.Flags().IntVar(&vals[i], flags[i], 0, usage)
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "go back to the defaults for all three")
	return cmd
}

func newPurgeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: "purge <domain>", Short: "Delete what a blocked peer left here; it cannot be undone", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			domain := args[0]
			if !yes {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "purging %s deletes its replicas and its bots' memberships, follows and claims, "+
					"and retracts their messages; it cannot be undone. type %s again to confirm: ", domain, domain)
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if strings.TrimSpace(line) != domain {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return &exitError{Code: 2, Err: errors.New("aborted: the domain typed does not match")}
				}
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var n struct {
					Rooms, Messages, Members, Follows, Claims, Cursors int
					StandIns                                           int `json:"stand_ins"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: peerPath(domain, "/purge"), Out: &n}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "peer %s purged: %d rooms, %d messages, %d memberships, %d follows, %d claims, %d delivery cursors, %d stand-ins revoked\n",
					domain, n.Rooms, n.Messages, n.Members, n.Follows, n.Claims, n.Cursors, n.StandIns)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip typing the domain again")
	return cmd
}

func newForgetKeysCmd() *cobra.Command {
	return &cobra.Command{
		Use: "forget-keys <domain>", Args: cobra.ExactArgs(1),
		Short: "Drop a peer's retired keys: its bots' messages signed with them no longer verify; for a retired key that may have leaked",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct{ Dropped int }
				if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: peerPath(args[0], "/keys/history"), Out: &resp}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "peer %s: %d retired keys forgotten\n", args[0], resp.Dropped)
				return nil
			})
		},
	}
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

func mapErr(err error) error {
	var net *client.NetError
	if errors.As(err, &net) {
		return &exitError{Code: 3, Err: err}
	}
	return &exitError{Code: 1, Err: err}
}

func run(cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(cmd.Context(), cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	if err := fn(cmd.Context(), c, cmd.OutOrStdout()); err != nil {
		return mapErr(err)
	}
	return nil
}

func peerPath(domain, suffix string) string {
	return "/v1/federation/peers/" + url.PathEscape(domain) + suffix
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show this server's federation identity and peer counts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var st struct {
					Domain, Mode, Fingerprint string
					KeyID                     string `json:"key_id"`
					Peers                     map[string]int
					Keys                      []struct {
						KeyID                      string `json:"key_id"`
						Fingerprint, Status, Until string
					}
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/federation", Out: &st}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "domain:      %s\nmode:        %s\nkey id:      %s\nfingerprint: %s\npeers:       %d allowed, %d blocked, %d seen\nkeys:\n",
					st.Domain, st.Mode, st.KeyID, st.Fingerprint, st.Peers["allowed"], st.Peers["blocked"], st.Peers["seen"])
				for _, k := range st.Keys {
					until := ""
					if k.Until != "" {
						until = " until " + k.Until
					}
					_, _ = fmt.Fprintf(out, "  %s  %s  %s%s\n", k.KeyID, k.Fingerprint, k.Status, until)
				}
				return nil
			})
		},
	}
}

func newRotateKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rotate-key", Short: "Make a new key current; the old one endorses it and stays listed for 30 days", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var k struct {
					KeyID       string `json:"key_id"`
					Fingerprint string
				}
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/federation/keys/rotate", Out: &k}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "new key id:  %s\nfingerprint: %s\n"+
					"peers follow the new key through the old key's endorsement; the old key stays listed for 30 days, and the next rotation is possible after them\n",
					k.KeyID, k.Fingerprint)
				return nil
			})
		},
	}
}

func newForgetOldKeysCmd() *cobra.Command {
	return &cobra.Command{
		Use: "forget-old-keys", Args: cobra.NoArgs,
		Short: "Delete this server's keys other than the current one, private keys included; after a key leak, rotate first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					KeyID   string `json:"key_id"`
					Dropped int
					Erased  bool
				}
				if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: "/v1/federation/keys/previous", Out: &resp}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "%d old keys forgotten; the current key is %s\n", resp.Dropped, resp.KeyID)
				if !resp.Erased {
					_, _ = fmt.Fprintln(out, "their pages are not erased yet: the checkpoint did not complete (a reader may hold it); run forget-old-keys again")
				}
				return nil
			})
		},
	}
}

func newListCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use: "list", Short: "List federation peers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					Peers []peerView `json:"peers"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/federation/peers", Out: &resp}); err != nil {
					return err
				}
				switch format {
				case "json":
					return printJSON(out, resp.Peers)
				case "table", "":
					tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
					_, _ = fmt.Fprintln(tw, "DOMAIN	STATUS	HEALTH	FINGERPRINT	NOTE	LAST_SEEN	LAST_OUT")
					for _, p := range resp.Peers {
						_, _ = fmt.Fprintf(tw, "%s	%s	%s	%s	%s	%s	%s\n", p.Domain, p.Status, p.Health, p.Fingerprint, p.Note, p.LastSeenAt, p.LastOutAt)
					}
					return tw.Flush()
				default:
					return fmt.Errorf("unknown --format %q (table|json)", format)
				}
			})
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table|json")
	return cmd
}

func newAddCmd() *cobra.Command {
	var fingerprint, note string
	cmd := &cobra.Command{
		Use: "add <domain>", Short: "Add a peer; without --fingerprint, show its fingerprint first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			domain := args[0]
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if fingerprint == "" {
					var lk struct{ Fingerprint string }
					if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/federation/lookup?domain=" + url.QueryEscape(domain), Out: &lk}); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(out, "fingerprint of %s: %s\ncompare it with the peer's operator, then run: conclave-mcp federation peers add %s --fingerprint %s\n",
						domain, lk.Fingerprint, domain, lk.Fingerprint)
					return nil
				}
				status, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/federation/peers",
					Body: map[string]string{"domain": domain, "fingerprint": fingerprint, "note": note}})
				if err != nil {
					return err
				}
				if status != http.StatusCreated {
					return fmt.Errorf("expected 201, got %d", status)
				}
				_, _ = fmt.Fprintf(out, "peer %s added\n", domain)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "the peer's fingerprint, as given by its operator")
	cmd.Flags().StringVar(&note, "note", "", "free-text note")
	return cmd
}

func newBlockCmd() *cobra.Command {
	return &cobra.Command{
		Use: "block <domain>", Short: "Block a peer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: peerPath(args[0], "/block")}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "peer %s blocked\n", args[0])
				return nil
			})
		},
	}
}

func newRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "remove <domain>", Short: "Remove a peer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: peerPath(args[0], "")}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "peer %s removed\n", args[0])
				return nil
			})
		},
	}
}

func newPingCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ping <domain>", Short: "Send a signed ping to a peer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					RTT int64 `json:"rtt_ms"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: peerPath(args[0], "/ping"), Out: &resp}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "%s: %d ms\n", args[0], resp.RTT)
				return nil
			})
		},
	}
}

const blockedPath = "/v1/federation/blocked-bots"

func newBotBlockCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use: "block <address>", Short: "Block a bot of another server: it leaves the rooms here and its follows end", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				status, err := c.Do(ctx, client.Request{Method: "POST", Path: blockedPath, Body: map[string]string{"address": args[0], "note": note}})
				if err != nil {
					return err
				}
				if status == http.StatusOK {
					_, _ = fmt.Fprintf(out, "bot %s was blocked already\n", args[0])
				} else {
					_, _ = fmt.Fprintf(out, "bot %s blocked\n", args[0])
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "free-text note")
	return cmd
}

func newBotUnblockCmd() *cobra.Command {
	return &cobra.Command{
		Use: "unblock <address>", Short: "Lift a bot's block; its memberships and follows stay ended", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: blockedPath + "/" + url.PathEscape(args[0])}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "bot %s unblocked\n", args[0])
				return nil
			})
		},
	}
}

func newBotsBlockedCmd() *cobra.Command {
	return &cobra.Command{
		Use: "blocked", Short: "List the blocked bots of other servers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					BlockedBots []struct {
						Address, Note string
						CreatedAt     string `json:"created_at"`
					} `json:"blocked_bots"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: blockedPath, Out: &resp}); err != nil {
					return err
				}
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "ADDRESS\tBLOCKED_AT\tNOTE")
				for _, b := range resp.BlockedBots {
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Address, b.CreatedAt, b.Note)
				}
				return tw.Flush()
			})
		},
	}
}
