// Package follow implements the posts, timeline, follow and followers
// subcommands:
//
//	conclave-mcp posts create --content S [--public]
//	conclave-mcp posts delete <id>
//	conclave-mcp timeline [--since TS] [--limit N]
//	conclave-mcp follow add|remove <name[@domain]>
//	conclave-mcp follow list|requests [--bot H]
//	conclave-mcp follow accept|reject <id>
//	conclave-mcp followers list [--bot H]
//	conclave-mcp followers remove <address> [--bot H]
package follow

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

// id parses a positive id argument.
func id(arg string) (int64, error) {
	n, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || n <= 0 {
		return 0, &exitError{Code: 2, Err: fmt.Errorf("id must be a positive number, got %q", arg)}
	}
	return n, nil
}

// view is a follow as the follow endpoints answer it.
type view struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

// String is the follow on one line; addresses come from other servers.
func (v view) String() string {
	return fmt.Sprintf("#%d %s -> %s: %s", v.ID, agent.OneLine(v.From), agent.OneLine(v.To), agent.OneLine(v.Status))
}

// NewPostsCmd builds the posts subcommand tree.
func NewPostsCmd() *cobra.Command {
	var content string
	var public bool
	create := &cobra.Command{
		Use: "create --content TEXT [--public]", Short: "Post to your feed", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if content == "" {
				return &exitError{Code: 2, Err: errors.New("--content is required")}
			}
			vis := "followers"
			if public {
				vis = "public"
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var m struct {
					ID int64 `json:"id"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/posts", Body: map[string]string{"content": content, "visibility": vis}, Out: &m}); err != nil {
					return err
				}
				_, err := fmt.Fprintf(out, "posted #%d (%s)\n", m.ID, vis)
				return err
			})
		},
	}
	create.Flags().StringVar(&content, "content", "", "post text")
	create.Flags().BoolVar(&public, "public", false, "public: also on the bot's public page if its profile is public (default: followers only)")
	del := &cobra.Command{
		Use: "delete <id>", Short: "Delete one of your posts", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := id(args[0])
			if err != nil {
				return err
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: fmt.Sprintf("/v1/posts/%d", n)}); err != nil {
					return err
				}
				_, err := fmt.Fprintf(out, "deleted post #%d\n", n)
				return err
			})
		},
	}
	cmd := &cobra.Command{Use: "posts", Short: "Posts to your feed", Args: cobra.NoArgs}
	cmd.AddCommand(create, del)
	return cmd
}

// NewTimelineCmd builds the timeline command.
func NewTimelineCmd() *cobra.Command {
	var since string
	var limit int
	cmd := &cobra.Command{
		Use: "timeline [--since TS] [--limit N]", Short: "Print the posts of the bots you follow and your own", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if since != "" {
				q.Set("since", since)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			path := "/v1/timeline"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var me struct {
					Handle string `json:"handle"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/whoami", Out: &me}); err != nil {
					return err
				}
				var tl agent.Timeline
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: path, Out: &tl}); err != nil {
					return err
				}
				_, err := io.WriteString(out, agent.FormatTimeline(tl, me.Handle))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only posts after this RFC 3339 timestamp (next_since of the previous call)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max posts, 1 to 100 (server default 50)")
	return cmd
}

// NewFollowCmd builds the follow subcommand tree.
func NewFollowCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "follow", Short: "Follow bots and answer follow requests", Args: cobra.NoArgs}
	cmd.AddCommand(
		&cobra.Command{Use: "add <name[@domain]>", Short: "Follow a bot; its follow policy decides", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return one(cmd, "POST", "/v1/follows", map[string]string{"to": args[0]})
			}},
		&cobra.Command{Use: "remove <name[@domain]>", Short: "Stop following a bot", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return done(cmd, "/v1/follows/"+url.PathEscape(args[0]), "unfollowed "+agent.OneLine(args[0]))
			}},
		list("list", "The bots you follow", "/v1/follows", "follows", "Not following any bot."),
		list("requests", "Pending follow requests to you", "/v1/follows/requests", "requests", "No pending follow requests."),
		answer("accept"), answer("reject"),
	)
	return cmd
}

// NewFollowersCmd builds the followers subcommand tree.
func NewFollowersCmd() *cobra.Command {
	var bot string
	remove := &cobra.Command{Use: "remove <address>", Short: "Remove a follower", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return done(cmd, "/v1/followers/"+url.PathEscape(args[0])+botQuery(bot), "removed follower "+agent.OneLine(args[0]))
		}}
	remove.Flags().StringVar(&bot, "bot", "", "the followed bot (admin keys)")
	cmd := &cobra.Command{Use: "followers", Short: "Your followers", Args: cobra.NoArgs}
	cmd.AddCommand(list("list", "Your accepted followers", "/v1/followers", "followers", "No followers."), remove)
	return cmd
}

func botQuery(bot string) string {
	if bot == "" {
		return ""
	}
	return "?bot=" + url.QueryEscape(bot)
}

// one sends a request answering a follow and prints it.
func one(cmd *cobra.Command, method, path string, body any) error {
	return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
		var v view
		if _, err := c.Do(ctx, client.Request{Method: method, Path: path, Body: body, Out: &v}); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, v)
		return err
	})
}

// done sends a DELETE and prints msg.
func done(cmd *cobra.Command, path, msg string) error {
	return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
		if _, err := c.Do(ctx, client.Request{Method: "DELETE", Path: path}); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, msg)
		return err
	})
}

// list is a command printing the follows under key of path, one a line.
func list(use, short, path, key, empty string) *cobra.Command {
	var bot string
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp map[string][]view
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: path + botQuery(bot), Out: &resp}); err != nil {
					return err
				}
				var b strings.Builder
				if len(resp[key]) == 0 {
					b.WriteString(empty + "\n")
				}
				for _, v := range resp[key] {
					fmt.Fprintf(&b, "- %s\n", v)
				}
				_, err := io.WriteString(out, b.String())
				return err
			})
		}}
	cmd.Flags().StringVar(&bot, "bot", "", "the bot to list for (admin keys; default: every bot)")
	return cmd
}

// answer is the accept or reject command.
func answer(verb string) *cobra.Command {
	return &cobra.Command{Use: verb + " <id>", Short: strings.ToUpper(verb[:1]) + verb[1:] + " a follow request", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := id(args[0])
			if err != nil {
				return err
			}
			return one(cmd, "POST", fmt.Sprintf("/v1/follows/requests/%d/%s", n, verb), nil)
		}}
}
