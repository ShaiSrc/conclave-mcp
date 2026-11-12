// Package ui implements `conclave-mcp ui setup`: it asks the running
// server for a one-time setup token and prints the URL that registers
// the operator's first passkey (docs/web-ui.md).
package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the ui subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Web admin UI setup",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newSetupCmd())
	return cmd
}

// openClient is a seam for tests.
var openClient = func(cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Print the URL that registers the first passkey (admin key, server running)",
		Long: `Asks the running server for a setup token (admin key required) and
prints http://localhost:<port>/ui/#setup=<token> on stdout. Open it in a
browser on the machine the server runs on, within 10 minutes, and
register a passkey; the link works once. The token is in the URL
fragment, which the browser does not send to the server or to proxies.

Once a passkey exists, the server refuses new setup tokens: add more
passkeys on the UI's Passkeys page, or start over with
conclave-mcp admin ui-reset (service stopped).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
			}
			return run(cmd.Context(), cmd, c)
		},
	}
}

func run(ctx context.Context, cmd *cobra.Command, c *client.Client) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Port() == "" {
		return &exitError{Code: 2, Err: fmt.Errorf("server URL %q: want http://<host>:<port>", c.BaseURL)}
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/ui/setup-tokens", Out: &resp}); err != nil {
		var netErr *client.NetError
		if errors.As(err, &netErr) {
			return &exitError{Code: 3, Err: err}
		}
		return &exitError{Code: 1, Err: err}
	}
	until := resp.ExpiresAt
	if t, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt); err == nil {
		until = t.Local().Format("15:04")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "http://localhost:%s/ui/#setup=%s\n", u.Port(), resp.Token)
	fmt.Fprintf(cmd.ErrOrStderr(), "open this URL in a browser on this machine before %s and register a passkey; it works once\n", until)
	return nil
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
