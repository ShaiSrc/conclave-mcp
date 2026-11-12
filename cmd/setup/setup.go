// Package setup implements `conclave-mcp init` (prepare a repository
// for coordinated Claude Code agents) and `conclave-mcp agent add`
// (create an agent key and invite it to the repository's room).
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
	"github.com/hermes/conclave-mcp/internal/cli/keystore"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// Seams for tests.
var (
	storeKey = keystore.Put
	keyWhere = keystore.Where
)

// Error is a setup failure; main exits 1 for it.
type Error struct{ Err error }

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ExitCode is 1 for setup errors and 0 otherwise.
func ExitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return 1
	}
	return 0
}

func fail(err error) error {
	if err == nil {
		return nil
	}
	return &Error{err}
}

// openClient is a seam for tests.
var openClient = func(cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

func NewInitCmd() *cobra.Command {
	var flagRoom string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare this repository for coordinated Claude Code agents",
		Long: `Run inside a git repository with the admin key. It finds or creates the
room for this repository (named after the origin remote, e.g.
acme/widgets; --room overrides), then:

  .conclave.json         created if missing (room, shared files, claim TTL)
  .mcp.json              conclave MCP server added, other servers kept
  .claude/settings.json  conclave-mcp hook added for all events, other hooks kept
  CLAUDE.md              coordination rules between conclave markers

Safe to re-run. Next: conclave-mcp agent add <label> --rights all --trust-origin internal, once per agent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return fail(err)
			}
			repo, err := agent.FindRepo(wd)
			if err != nil {
				return fail(err)
			}
			if flagRoom != "" {
				repo.Config.Room = flagRoom
			}
			name := repo.RoomName()
			c, err := openClient(cmd)
			if err != nil {
				return fail(err)
			}
			roomID, created, err := ensureRoom(cmd.Context(), c, name)
			if err != nil {
				return fail(err)
			}
			changed, err := WriteFiles(repo.Root, name)
			if err != nil {
				return fail(err)
			}
			errOut := cmd.ErrOrStderr()
			verb := "found"
			if created {
				verb = "created"
			}
			fmt.Fprintf(errOut, "room %q %s (room_id %s)\n", name, verb, roomID)
			for _, f := range changed {
				fmt.Fprintf(errOut, "updated %s\n", f)
			}
			fmt.Fprintln(errOut, "next: commit these files, then per agent: eval \"$(conclave-mcp agent add <label> --rights all --trust-origin internal --store)\" && claude")
			return nil
		},
	}
	cmd.Flags().StringVar(&flagRoom, "room", "", "room name (default: owner/repo of the origin remote)")
	return cmd
}

// ensureRoom finds the room among the caller's rooms or creates it.
func ensureRoom(ctx context.Context, c *client.Client, name string) (id string, created bool, err error) {
	id, err = agent.ResolveRoom(ctx, c, name)
	if err == nil || !errors.Is(err, agent.ErrNoRoom) {
		return id, false, err
	}
	var room struct {
		ID string `json:"id"`
	}
	_, err = c.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms", Out: &room,
		Body: map[string]any{"name": name, "description": "Coordination room for coding agents on " + name}})
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return "", false, fmt.Errorf("room %q exists but is archived or not visible to this key; run init with the admin key", name)
	}
	return room.ID, err == nil, err
}

func NewAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage the coding agents of a repository's room",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newAgentAddCmd())
	return cmd
}

func newAgentAddCmd() *cobra.Command {
	var flagRoom, flagRights, flagTrust string
	var flagStore bool
	cmd := &cobra.Command{
		Use:   "add <label>",
		Short: "Create a key for a new agent and invite it to the repository's room",
		Long: `Needs the admin key. Creates a key for the agent, invites it to the
repository's room with --rights and prints "export CONCLAVE_API_KEY=..."
on stdout (shown once, not stored anywhere) and the status on stderr.
--rights defaults to none: the key can do nothing until rights are
granted. Your own coding agents get --rights all --trust-origin internal;
--trust-origin defaults to external. Start the agent with it:

  eval "$(conclave-mcp agent add agent-c --rights all --trust-origin internal)" && claude

With --store the key goes to the macOS Keychain (elsewhere a 0600 file
in the user config dir) and stdout gets "export CONCLAVE_AGENT=<label>"
instead, so the same eval works. Sessions that do not start from a
shell (Claude desktop app, IDE) pick the agent up from
.claude/settings.local.json: {"env": {"CONCLAVE_AGENT": "<label>"}}.
For an agent in a room with external agents see docs/shared-rooms.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagStore {
				if err := keystore.ValidLabel(args[0]); err != nil {
					return fail(err)
				}
			}
			name := flagRoom
			if name == "" {
				wd, err := os.Getwd()
				if err != nil {
					return fail(err)
				}
				repo, err := agent.FindRepo(wd)
				if err != nil {
					return fail(fmt.Errorf("%w (or pass --room)", err))
				}
				name = repo.RoomName()
			}
			c, err := openClient(cmd)
			if err != nil {
				return fail(err)
			}
			ctx := cmd.Context()
			roomID, err := agent.ResolveRoom(ctx, c, name)
			if err != nil {
				return fail(err)
			}
			var key struct {
				ID  int64  `json:"id"`
				Key string `json:"key"`
			}
			if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/keys", Out: &key,
				Body: map[string]any{"label": args[0], "trust_origin": flagTrust}}); err != nil {
				return fail(err)
			}
			if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + roomID + "/invite",
				Body: map[string]any{"key_id": key.ID, "rights": rights.Split(flagRights)}}); err != nil {
				// Do not leave behind a key that can reach nothing.
				_, _ = c.Do(ctx, client.Request{Method: "DELETE", Path: fmt.Sprintf("/v1/keys/%d", key.ID)})
				return fail(fmt.Errorf("invite to %q: %w", name, err))
			}
			granted := flagRights
			if granted == "" {
				granted = "none"
			}
			if flagStore {
				if err := storeKey(args[0], key.Key); err != nil {
					_, _ = c.Do(ctx, client.Request{Method: "DELETE", Path: fmt.Sprintf("/v1/keys/%d", key.ID)})
					return fail(fmt.Errorf("store key: %w (key revoked)", err))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "export CONCLAVE_AGENT=%s\n", args[0])
				fmt.Fprintf(cmd.ErrOrStderr(), "agent %q (key %d) joined %q with rights %s; key stored in %s\n"+
					"desktop/IDE sessions: put {\"env\": {\"CONCLAVE_AGENT\": %q}} in .claude/settings.local.json\n",
					args[0], key.ID, name, granted, keyWhere(args[0]), args[0])
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "export CONCLAVE_API_KEY=%s\n", key.Key)
			fmt.Fprintf(cmd.ErrOrStderr(), "agent %q (key %d) joined %q with rights %s; the key is shown only once\n", args[0], key.ID, name, granted)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagRoom, "room", "", "room name (default: the current repository's room)")
	cmd.Flags().StringVar(&flagRights, "rights", "", "comma-separated rights in the room (read, claim, message types) or all; default none")
	cmd.Flags().StringVar(&flagTrust, "trust-origin", "external", "external (default) or internal (your own agents)")
	cmd.Flags().BoolVar(&flagStore, "store", false, "store the key (macOS Keychain or a 0600 file) and print export CONCLAVE_AGENT=<label>")
	return cmd
}
