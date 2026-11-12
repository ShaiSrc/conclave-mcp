// Package rooms implements the `conclave-mcp rooms` subcommand:
//
//	conclave-mcp rooms list
//	conclave-mcp rooms create --name <name> [--description ...]           (admin key)
//	conclave-mcp rooms get <id>
//	conclave-mcp rooms archive <id>                                       (admin key)
//	conclave-mcp rooms invite <room-id> <api-key-id> [--rights a,b] [--allow-multi-room]  (admin key)
//	conclave-mcp rooms rights <room-id> <api-key-id> <rights>             (admin key)
//	conclave-mcp rooms members [room]
//	conclave-mcp rooms remove <room-id> <api-key-id>                      (admin key)
//
// All subcommands talk to a running server via the CLI client. They
// never touch SQLite directly.
package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/cli/client"
	"github.com/hermes/conclave-mcp/internal/rights"
)

// NewCmd builds the `rooms` subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rooms",
		Short: "Manage rooms (create, list, get, archive, invite, rights, members, remove)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newCreateCmd())
	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newArchiveCmd())
	cmd.AddCommand(newInviteCmd())
	cmd.AddCommand(newRightsCmd())
	cmd.AddCommand(newMembersCmd())
	cmd.AddCommand(newRemoveCmd())
	return cmd
}

// roomView mirrors the JSON shape returned by GET/POST /v1/rooms/*.
// Defined locally so the CLI doesn't import internal/rooms (which
// would break the package boundary since the server is meant to be
// the only caller of internal/rooms).
type roomView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	OwnerKeyID  int64  `json:"owner_key_id"`
	CreatedAt   string `json:"created_at"`
	ArchivedAt  string `json:"archived_at,omitempty"`
	// MyRights is the caller's own rights (GET /v1/rooms/{id} only).
	MyRights []string `json:"my_rights,omitempty"`
}

type listResponse struct {
	Rooms []roomView `json:"rooms"`
}

// openClient is a seam for tests: real code uses New, tests can swap.
var openClient = func(_ context.Context, cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	as, _ := cmd.Flags().GetString("as")
	return client.New(client.Options{Server: srv, Token: tok, Agent: as})
}

// exitError lets subcommands request a specific non-zero exit code
// without polluting the cobra error message shown to the user.
type exitError struct {
	Code int
	Err  error
}

func (e *exitError) Error() string { return e.Err.Error() }
func (e *exitError) Unwrap() error { return e.Err }

func (e *exitError) toCobra() *cobra.Command { return nil } // satisfies unused symbol guard

// makeClientAndDo wires up the client, runs fn, maps errors to exit
// codes per SPEC-0004 §4:
//
//	network  -> 3
//	401      -> 1 (server rejected our key)
//	2xx/4xx  -> 1 (server returned an error, including 404)
//	success  -> 0
func makeClientAndDo(ctx context.Context, cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out io.Writer) error) error {
	c, err := openClient(ctx, cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	out := cmd.OutOrStdout()
	if err := fn(ctx, c, out); err != nil {
		return mapErr(err)
	}
	return nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var net *client.NetError
	if errors.As(err, &net) {
		return &exitError{Code: 3, Err: err}
	}
	return &exitError{Code: 1, Err: err}
}

// ---------- list ----------

func newListCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your rooms (an admin key sees every room)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp listResponse
				_, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms", Out: &resp})
				if err != nil {
					return err
				}
				return renderRooms(format, resp.Rooms, out)
			})
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table|json")
	return cmd
}

func renderRooms(format string, rooms []roomView, out io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rooms)
	case "table", "":
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tOWNER\tCREATED\tARCHIVED")
		for _, r := range rooms {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
				r.ID, r.Name, r.OwnerKeyID, r.CreatedAt, r.ArchivedAt)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown --format %q (table|json)", format)
	}
}

// ---------- create ----------

func newCreateCmd() *cobra.Command {
	var (
		name        string
		description string
		visibility  string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new room (admin key only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var created roomView
				status, err := c.Do(ctx, client.Request{
					Method: "POST",
					Path:   "/v1/rooms",
					Body:   map[string]string{"name": name, "description": description, "visibility": visibility},
					Out:    &created,
				})
				if err != nil {
					return err
				}
				if status != http.StatusCreated {
					return fmt.Errorf("expected 201, got %d", status)
				}
				return renderRooms("json", []roomView{created}, out)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "room name (required, 1..128 chars)")
	cmd.Flags().StringVar(&description, "description", "", "optional room description (max 1024 chars)")
	cmd.Flags().StringVar(&visibility, "visibility", "private", "room visibility: 'private' (default) or 'public'")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// ---------- get ----------

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a room and your rights in it (my_rights)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var r roomView
				_, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms/" + args[0], Out: &r})
				if err != nil {
					return err
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			})
		},
	}
	return cmd
}

// ---------- archive ----------

func newArchiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <id>",
		Short: "Soft-archive a room (admin key only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				_, err := c.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + args[0] + "/archive"})
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "room %s archived\n", args[0])
				return nil
			})
		},
	}
	return cmd
}

// ---------- invite ----------

func newInviteCmd() *cobra.Command {
	var flagRights string
	var flagMulti bool
	cmd := &cobra.Command{
		Use:   "invite <room-id> <api-key-id>",
		Short: "Add an API key to a room with the given rights (admin key only)",
		Long: `Adds an API key to a room (admin key only). --rights is a
comma-separated list of read, claim and message types (question, answer,
decision, handoff, status_update, task_assignment, alert, system), or
all / none. Without --rights the key gets no rights. A key that is
already in another room is refused unless --allow-multi-room is given.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			roomID := args[0]
			keyIDInt, err := parseInt64(args[1])
			if err != nil {
				return &exitError{Code: 2, Err: fmt.Errorf("api-key-id: %w", err)}
			}
			list := rights.Split(flagRights)
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				_, err := c.Do(ctx, client.Request{
					Method: "POST",
					Path:   "/v1/rooms/" + roomID + "/members",
					Body:   map[string]any{"key_id": keyIDInt, "rights": list, "allow_multi_room": flagMulti},
				})
				if err != nil {
					return err
				}
				if len(list) == 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "key %d has no rights in room %s; grant them with: conclave-mcp rooms rights %s %d <rights>\n",
						keyIDInt, roomID, roomID, keyIDInt)
				}
				_, _ = fmt.Fprintf(out, "invited key %d to room %s with rights %s\n", keyIDInt, roomID, rightsText(list))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&flagRights, "rights", "", "comma-separated rights, or all / none (default: none)")
	cmd.Flags().BoolVar(&flagMulti, "allow-multi-room", false, "allow a key that is already in another room")
	return cmd
}

// ---------- rights ----------

func newRightsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rights <room-id> <api-key-id> <rights>",
		Short: "Replace a member's rights (admin key only)",
		Long:  "Replaces a member's rights (admin key only). <rights> is a comma-separated list, or all / none.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			roomID, keyID := args[0], args[1]
			if _, err := parseInt64(keyID); err != nil {
				return &exitError{Code: 2, Err: fmt.Errorf("api-key-id: %w", err)}
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				var resp struct {
					Rights []string `json:"rights"`
				}
				if _, err := c.Do(ctx, client.Request{
					Method: "PATCH",
					Path:   "/v1/rooms/" + roomID + "/members/" + keyID,
					Body:   map[string]any{"rights": rights.Split(args[2])},
					Out:    &resp,
				}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "key %s in room %s: %s\n", keyID, roomID, rightsText(resp.Rights))
				return nil
			})
		},
	}
}

// ---------- members ----------

func newMembersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "members [room]",
		Short: "List a room's members (needs read; rights shown to admin keys)",
		Long: `Lists a room's members: key id, label, internal/external, and each
member's rights when the caller is an admin key. ROOM is a room id or
name; without it: $CONCLAVE_ROOM, then the room in the .conclave.json of
the repository you are in.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				arg := ""
				if len(args) > 0 {
					arg = args[0]
				}
				roomID, err := agent.RoomID(ctx, c, arg)
				if err != nil {
					return err
				}
				var resp struct {
					Members []struct {
						KeyID       int64  `json:"key_id"`
						Label       string `json:"label"`
						TrustOrigin string `json:"trust_origin"`
						// Only admin keys get rights; JSON [] decodes to an
						// empty non-nil slice, a missing field to nil.
						Rights []string `json:"rights"`
					} `json:"members"`
				}
				if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms/" + roomID + "/members", Out: &resp}); err != nil {
					return err
				}
				admin := len(resp.Members) > 0 && resp.Members[0].Rights != nil
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				header := "ID\tLABEL\tTRUST"
				if admin {
					header += "\tRIGHTS"
				}
				_, _ = fmt.Fprintln(tw, header)
				for _, m := range resp.Members {
					line := fmt.Sprintf("%d\t%s\t%s", m.KeyID, m.Label, m.TrustOrigin)
					if admin {
						line += "\t" + rightsText(m.Rights)
					}
					_, _ = fmt.Fprintln(tw, line)
				}
				return tw.Flush()
			})
		},
	}
}

// rightsText renders rights for humans: "read,handoff", or "none".
func rightsText(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ",")
}

// ---------- remove ----------

func newRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <room-id> <api-key-id>",
		Short: "Remove an API key from a room (admin key only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			roomID, keyID := args[0], args[1]
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out io.Writer) error {
				_, err := c.Do(ctx, client.Request{
					Method: "DELETE",
					Path:   "/v1/rooms/" + roomID + "/members/" + keyID,
				})
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "removed key %s from room %s\n", keyID, roomID)
				return nil
			})
		},
	}
	return cmd
}

// ---------- helpers ----------

// ExitError unwraps a *exitError to its int code. The cmd/conclave-mcp
// main package uses this to map to process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 2 // client misuse
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int64(ch-'0')
	}
	return n, nil
}

// Used at init time to keep some symbols in case future subcommands need them.
var _ = http.StatusOK
