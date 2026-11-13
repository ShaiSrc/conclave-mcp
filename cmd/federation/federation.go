// Package federation implements the `conclave-mcp federation` subcommand:
//
//	conclave-mcp federation status
//	conclave-mcp federation peers list [--format json|table]
//	conclave-mcp federation peers add <domain> [--fingerprint F] [--note N]
//	conclave-mcp federation peers block|remove|ping <domain>
package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the federation subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "federation", Short: "Manage federation peers", Args: cobra.NoArgs}
	peers := &cobra.Command{Use: "peers", Short: "Manage federation peers", Args: cobra.NoArgs}
	peers.AddCommand(newListCmd(), newAddCmd(), newBlockCmd(), newRemoveCmd(), newPingCmd())
	cmd.AddCommand(newStatusCmd(), peers)
	return cmd
}

type peerView struct {
	Domain      string `json:"domain"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
	Note        string `json:"note"`
	AddedAt     string `json:"added_at"`
	LastSeenAt  string `json:"last_seen_at"`
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
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/federation", Out: &st}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "domain:      %s\nmode:        %s\nkey id:      %s\nfingerprint: %s\npeers:       %d allowed, %d blocked, %d seen\n",
					st.Domain, st.Mode, st.KeyID, st.Fingerprint, st.Peers["allowed"], st.Peers["blocked"], st.Peers["seen"])
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
					_, _ = fmt.Fprintln(tw, "DOMAIN	STATUS	FINGERPRINT	NOTE	LAST_SEEN")
					for _, p := range resp.Peers {
						_, _ = fmt.Fprintf(tw, "%s	%s	%s	%s	%s\n", p.Domain, p.Status, p.Fingerprint, p.Note, p.LastSeenAt)
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
