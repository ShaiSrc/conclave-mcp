// Command conclave-mcp is the entrypoint for the Conclave MCP server.
//
// Conclave MCP is a local-first agent communication server. It exposes
// an HTTP control plane on /v1 and a stdio MCP transport wrapping it.
//
// See SPEC-0001 for the v0.1 scope and ADR-0005 for the vault-as-source
// of-truth policy.
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/cmd/admin"
	"github.com/hermes/conclave-mcp/cmd/audit"
	"github.com/hermes/conclave-mcp/cmd/keys"
	mcpcli "github.com/hermes/conclave-mcp/cmd/mcp"
	"github.com/hermes/conclave-mcp/cmd/messages"
	"github.com/hermes/conclave-mcp/cmd/rooms"
	"github.com/hermes/conclave-mcp/cmd/serve"
)

var version = "0.1.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "conclave-mcp",
		Short:         "Conclave MCP — local-first agent communication server",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("conclave-mcp {{.Version}}\n")
	root.AddCommand(serve.NewCmd())
	root.AddCommand(mcpcli.NewCmd())
	root.AddCommand(rooms.NewCmd())
	root.AddCommand(messages.NewCmd())
	root.AddCommand(keys.NewCmd())
	root.AddCommand(audit.NewCmd())
	root.AddCommand(admin.NewCmd())
	root.AddCommand(newVersionCmd())

	// Global flags shared by every CLI subcommand that talks to a server.
	root.PersistentFlags().String("server", "", "conclave-mcp server URL (env CONCLAVE_BASE_URL, default http://127.0.0.1:7432)")
	root.PersistentFlags().String("token", "", "bearer token (env CONCLAVE_API_KEY)")
	root.PersistentFlags().Bool("quiet", false, "suppress stderr diagnostics (mcp-serve only)")
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and exit",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println("conclave-mcp " + version)
		},
	}
}

func main() {
	cmd := newRootCmd()
	err := cmd.Execute()
	if err == nil {
		return
	}
	// rooms subcommands carry a typed exit code via *rooms.exitError.
	// Detect it before delegating to exitClassifier (which only knows
	// about cobra's built-in error categories).
	code := rooms.ExitCode(err)
	if code == 0 || code == 2 {
		// 0 = wrapped nil (shouldn't reach here, cmd.Execute returned err).
		// 2 = either from rooms (caller-misuse) or from exitClassifier
		// (unknown command/flag). exitClassifier is the authority on 2.
		if c := exitClassifier(err); c != 0 {
			code = c
		}
	}
	// messages.ExitCode has the same shape and overrides only if rooms
	// didn't already pick a specific code.
	if code == 0 || code == 2 {
		if c := messages.ExitCode(err); c == 1 || c == 3 {
			code = c
		}
	}
	// keys.ExitCode same story.
	if code == 0 || code == 2 {
		if c := keys.ExitCode(err); c == 1 || c == 3 {
			code = c
		}
	}
	// audit.ExitCode same.
	if code == 0 || code == 2 {
		if c := audit.ExitCode(err); c == 1 || c == 3 {
			code = c
		}
	}
	// admin.ExitCode same.
	if code == 0 || code == 2 {
		if c := admin.ExitCode(err); c == 1 || c == 3 {
			code = c
		}
	}
	if code != 0 {
		os.Exit(code)
	}
}
