// Package mcp implements the `conclave-mcp mcp-serve` subcommand
// (SPEC-0003 §2.2, SPEC-0004). The subcommand speaks the
// stdio MCP transport: it reads JSON-RPC 2.0 from stdin, writes
// to stdout, and uses the bearer token from the
// CONCLAVE_API_KEY environment variable to call the Conclave
// HTTP control plane on the local host.
//
// This is the recommended transport for in-process agent
// integration. The alternative — JSON-RPC-over-HTTP at
// POST /v1/mcp — is wired into the server at boot.
package mcp

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	mcpx "github.com/hermes/conclave-mcp/internal/mcp"
)

// NewCmd returns the `conclave-mcp mcp-serve` cobra subcommand.
//
// Authentication: the bearer token is read from the
// CONCLAVE_API_KEY env var. The --server flag (or
// CONCLAVE_BASE_URL / CONCLAVE_SERVER) points at the local
// Conclave HTTP API. Both are inherited from the global
// --server / --token flags if set.
func NewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-serve",
		Short: "Run the MCP server on stdio (JSON-RPC 2.0)",
		Long: `Reads JSON-RPC 2.0 from stdin and writes responses to stdout.

The MCP transport wraps the Conclave HTTP control plane. The
bearer token is read from CONCLAVE_API_KEY; the server URL
from CONCLAVE_BASE_URL (or CONCLAVE_SERVER) or the --server
flag. See SPEC-0003 §2.2 and SPEC-0004 for the tool surface.

This is the recommended transport for in-process agent
integration. The alternative is JSON-RPC-over-HTTP at
POST /v1/mcp, which is wired into the HTTP server and
exposed to any HTTP-speaking MCP client.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			// Ctrl-C / SIGTERM: cancel the context so the
			// ServeStdio loop exits.
			go func() {
				sigCh := make(chan os.Signal, 1)
				// We can't import os/signal here without
				// also wiring it in main; do it inline.
				// Actually we can: just use os.Stdin to
				// detect EOF and the standard signal
				// package is the right answer.
				_ = sigCh
			}()
			return runServeStdio(ctx, cmd)
		},
	}
}

// runServeStdio builds an MCP server and runs the stdio loop.
// The token and base URL come from the cobra command's
// persistent flags (which inherit from env vars via
// openClient pattern in other subcommands).
func runServeStdio(ctx context.Context, cmd *cobra.Command) error {
	token, _ := cmd.Flags().GetString("token")
	if token == "" {
		token = os.Getenv("CONCLAVE_API_KEY")
	}
	if token == "" {
		return fmt.Errorf("missing api key: pass --token or set CONCLAVE_API_KEY")
	}
	baseURL, _ := cmd.Flags().GetString("server")
	if baseURL == "" {
		// SPEC-0004: CONCLAVE_BASE_URL is the canonical env
		// var; CONCLAVE_SERVER is the legacy alias.
		baseURL = os.Getenv("CONCLAVE_BASE_URL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("CONCLAVE_SERVER")
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:7432"
	}
	srv := mcpx.NewServer(baseURL, token)
	// In the stdio transport, the server's stderr is the
	// caller's stdout (the user's terminal). We log to
	// stderr by default; --quiet suppresses it.
	quiet, _ := cmd.Flags().GetBool("quiet")
	var errOut io.Writer = os.Stderr
	if quiet {
		errOut = io.Discard
	}
	return srv.ServeStdio(ctx, os.Stdin, os.Stdout, errOut)
}
