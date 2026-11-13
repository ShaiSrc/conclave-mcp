// Package bots implements the `conclave-mcp bots` subcommand:
//
//	conclave-mcp bots list
//	conclave-mcp bots show <handle>
//	conclave-mcp bots lookup <name[@domain]>
//	conclave-mcp bots directory
//	conclave-mcp bots handle <key-id> <name>      (admin; "" removes)
//	conclave-mcp bots profile <handle> [--display-name S] [--description S] [--skill id:name:description ...] [--visibility V]
package bots

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the bots subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "bots", Short: "Bot handles, profiles and the directory", Args: cobra.NoArgs}
	cmd.AddCommand(newListCmd(), newShowCmd(), newLookupCmd(), newDirectoryCmd(), newHandleCmd(), newProfileCmd())
	return cmd
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

func run(cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(cmd.Context(), cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	if err := fn(cmd.Context(), c, cmd.OutOrStdout()); err != nil {
		var net *client.NetError
		if errors.As(err, &net) {
			return &exitError{Code: 3, Err: err}
		}
		return &exitError{Code: 1, Err: err}
	}
	return nil
}

type list struct {
	Bots []agent.ProfileEntry `json:"bots"`
}

func getList(path string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
			var l list
			if _, err := c.Do(ctx, client.Request{Method: "GET", Path: path, Out: &l}); err != nil {
				return err
			}
			_, err := io.WriteString(out, agent.FormatProfiles(l.Bots))
			return err
		})
	}
}

func getOne(path func(string) string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
			var p agent.ProfileEntry
			if _, err := c.Do(ctx, client.Request{Method: "GET", Path: path(args[0]), Out: &p}); err != nil {
				return err
			}
			_, err := io.WriteString(out, agent.FormatProfiles([]agent.ProfileEntry{p}))
			return err
		})
	}
}

func newListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List the local bots you may see", Args: cobra.NoArgs, RunE: getList("/v1/bots")}
}

func newDirectoryCmd() *cobra.Command {
	return &cobra.Command{Use: "directory", Short: "List local bots and the cached bots of peers", Args: cobra.NoArgs, RunE: getList("/v1/directory")}
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show <handle>", Short: "Show a local bot", Args: cobra.ExactArgs(1),
		RunE: getOne(func(h string) string { return "/v1/bots/" + url.PathEscape(h) })}
}

func newLookupCmd() *cobra.Command {
	return &cobra.Command{Use: "lookup <name[@domain]>", Short: "Find a bot here or on a peer", Args: cobra.ExactArgs(1),
		RunE: getOne(func(h string) string { return "/v1/bots/lookup?handle=" + url.QueryEscape(h) })}
}

func newHandleCmd() *cobra.Command {
	return &cobra.Command{
		Use: "handle <key-id> <name>", Short: "Set a key's handle (admin); an empty name removes it", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return &exitError{Code: 2, Err: fmt.Errorf("key-id must be a positive number, got %q", args[0])}
			}
			path := "/v1/keys/" + args[0] + "/handle"
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if args[1] == "" {
					if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: path}); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(out, "handle of key %d removed\n", id)
					return nil
				}
				if _, err := c.Do(ctx, client.Request{Method: "PUT", Path: path, Body: map[string]string{"handle": args[1]}}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "key %d is now %s\n", id, args[1])
				return nil
			})
		},
	}
}

func newProfileCmd() *cobra.Command {
	var name, desc, vis string
	var skills []string
	cmd := &cobra.Command{
		Use: "profile <handle>", Short: "Edit a profile; only the given fields change (--skill replaces the whole list)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			f := cmd.Flags()
			if f.Changed("display-name") {
				body["display_name"] = name
			}
			if f.Changed("description") {
				body["description"] = desc
			}
			if f.Changed("visibility") {
				body["visibility"] = vis
			}
			if f.Changed("skill") {
				list := []map[string]string{}
				for _, s := range skills {
					parts := strings.SplitN(s, ":", 3)
					if len(parts) != 3 {
						return &exitError{Code: 2, Err: fmt.Errorf("--skill %q: want id:name:description", s)}
					}
					list = append(list, map[string]string{"id": parts[0], "name": parts[1], "description": parts[2]})
				}
				body["skills"] = list
			}
			if len(body) == 0 {
				return &exitError{Code: 2, Err: errors.New("nothing to change: pass --display-name, --description, --skill or --visibility")}
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var p agent.ProfileEntry
				if _, err := c.Do(ctx, client.Request{Method: "PATCH", Path: "/v1/bots/" + url.PathEscape(args[0]) + "/profile", Body: body, Out: &p}); err != nil {
					return err
				}
				_, err := io.WriteString(out, agent.FormatProfiles([]agent.ProfileEntry{p}))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&name, "display-name", "", "display name")
	cmd.Flags().StringVar(&desc, "description", "", "description")
	cmd.Flags().StringArrayVar(&skills, "skill", nil, "skill as id:name:description (repeatable)")
	cmd.Flags().StringVar(&vis, "visibility", "", "private|peers|public")
	return cmd
}
