// Package keys implements the `conclave-mcp keys` subcommand.
//
// Per SPEC-0004 §5:
//
//	conclave-mcp keys create [--label LABEL] [--scopes ...]
//	conclave-mcp keys list [--format json|table]
//	conclave-mcp keys revoke <id> [--yes]
//
// The freshly created key's plaintext is printed to STDOUT exactly
// once. Status messages and prompts go to STDERR so that
//
//	conclave-mcp keys create | tee key.txt
//
// saves the key in key.txt without contaminating it with human chatter.
package keys

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// NewCmd builds the keys subcommand tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys (create, list, revoke)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCreateCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newRevokeCmd())
	return cmd
}

// keyView mirrors the JSON shape of GET /v1/keys entries.
// Note: no "key" field — that is only present on POST /v1/keys response.
type keyView struct {
	ID          int64  `json:"id"`
	Prefix      string `json:"prefix"`
	Label       string `json:"label,omitempty"`
	Scopes      string `json:"scopes,omitempty"`
	CreatedAt   string `json:"created_at"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	RevokedAt   string `json:"revoked_at,omitempty"`
	TrustOrigin string `json:"trust_origin"`
}

type createResponse struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	Label       string `json:"label,omitempty"`
	TrustOrigin string `json:"trust_origin,omitempty"`
}

type listResponse struct {
	Keys []keyView `json:"keys"`
}

// openClient is a seam for tests.
var openClient = func(_ context.Context, cmd *cobra.Command) (*client.Client, error) {
	tok, _ := cmd.Flags().GetString("token")
	srv, _ := cmd.Flags().GetString("server")
	return client.New(client.Options{Server: srv, Token: tok})
}

// exitError mirrors cmd/rooms' exitError.
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
	if err == nil {
		return nil
	}
	var net *client.NetError
	if errors.As(err, &net) {
		return &exitError{Code: 3, Err: err}
	}
	return &exitError{Code: 1, Err: err}
}

func makeClientAndDo(ctx context.Context, cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, out, errOut io.Writer) error) error {
	c, err := openClient(ctx, cmd)
	if err != nil {
		return &exitError{Code: 2, Err: fmt.Errorf("client: %w", err)}
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	if err := fn(ctx, c, out, errOut); err != nil {
		return mapErr(err)
	}
	return nil
}

// ---------- create ----------

func newCreateCmd() *cobra.Command {
	var (
		flagLabel       string
		flagScopes      string
		flagFormat      string
		flagTrustOrigin string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API key (plaintext shown once, to STDOUT)",
		Long: `Creates a new API key. The plaintext key is written to STDOUT
exactly once. Pipe to a file or password manager; it cannot be retrieved
later. Use --label to attach a human-readable identifier (max 128 chars).
Use --trust-origin=external to mark the key as belonging to an invited
collaborator whose agent we do not trust by default; the server will
denormalise the value onto every message the key writes (SPEC-0001 Q11,
ADR-0008 revision). Default: 'internal'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out, errOut io.Writer) error {
				body := map[string]any{}
				if flagLabel != "" {
					body["label"] = flagLabel
				}
				if flagScopes != "" {
					body["scopes"] = strings.Split(flagScopes, ",")
				}
				if flagTrustOrigin != "" {
					// Validate locally before the request; the
					// server validates too but a 400 is friendlier
					// than a network round-trip.
					if flagTrustOrigin != "internal" && flagTrustOrigin != "external" {
						return fmt.Errorf("--trust-origin must be 'internal' or 'external' (got %q)", flagTrustOrigin)
					}
					body["trust_origin"] = flagTrustOrigin
				}
				var resp createResponse
				status, err := c.Do(ctx, client.Request{
					Method: "POST",
					Path:   "/v1/keys",
					Body:   body,
					Out:    &resp,
				})
				if err != nil {
					return err
				}
				if status != http.StatusCreated {
					return fmt.Errorf("expected 201, got %d", status)
				}
				// Per SPEC-0004 §5: plaintext to STDOUT.
				// Default format: just the bare key on one line (pipe-friendly).
				// --format json: full envelope.
				switch flagFormat {
				case "json":
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(resp)
				case "", "plain":
					_, err := fmt.Fprintln(out, resp.Key)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(errOut, "key id=%d label=%q trust_origin=%q created; store the plaintext now, it will not be shown again.\n",
						resp.ID, resp.Label, resp.TrustOrigin)
					return err
				default:
					return fmt.Errorf("unknown --format %q (plain|json)", flagFormat)
				}
			})
		},
	}
	cmd.Flags().StringVar(&flagLabel, "label", "", "human-readable label for the key (max 128 chars)")
	cmd.Flags().StringVar(&flagScopes, "scopes", "", "comma-separated scopes (server-defined, optional)")
	cmd.Flags().StringVar(&flagFormat, "format", "plain", "output format: plain|json")
	cmd.Flags().StringVar(&flagTrustOrigin, "trust-origin", "",
		"trust classification: 'internal' (default) or 'external' (SPEC-0001 Q11)")
	return cmd
}

// ---------- list ----------

func newListCmd() *cobra.Command {
	var flagFormat string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all API keys (never shows plaintext or hash)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out, _ io.Writer) error {
				var resp listResponse
				_, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/keys", Out: &resp})
				if err != nil {
					return err
				}
				return renderKeys(flagFormat, resp.Keys, out)
			})
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "table", "output format: table|json")
	return cmd
}

func renderKeys(format string, keys []keyView, out io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(keys)
	case "table", "":
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ID	PREFIX	LABEL	TRUST	CREATED	LAST_USED	REVOKED")
		for _, k := range keys {
			_, _ = fmt.Fprintf(tw, "%d	%s	%s	%s	%s	%s	%s\n",
				k.ID, k.Prefix, k.Label, k.TrustOrigin, k.CreatedAt, k.LastUsedAt, k.RevokedAt)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown --format %q (table|json)", format)
	}
}

// ---------- revoke ----------

func newRevokeCmd() *cobra.Command {
	var flagYes bool
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API key (soft-delete, irreversible)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			// Confirm unless --yes. Confirmation prompt goes to STDERR;
			// the answer is read from STDIN.
			if !flagYes {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "revoke key %s? [y/N] ", id)
				reader := bufio.NewReader(stdinReader)
				line, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return &exitError{Code: 2, Err: errors.New("user aborted")}
				}
			}
			return makeClientAndDo(cmd.Context(), cmd, func(ctx context.Context, c *client.Client, out, _ io.Writer) error {
				_, err := c.Do(ctx, client.Request{Method: "DELETE", Path: "/v1/keys/" + id})
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "key %s revoked\n", id)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&flagYes, "yes", false, "skip confirmation prompt")
	return cmd
}

// stdinReader is swappable from tests.
var stdinReader io.Reader = os.Stdin

// ResetStdin replaces stdin for tests. Pass nil to restore.
func ResetStdin(r io.Reader) {
	if r == nil {
		stdinReader = os.Stdin
		return
	}
	stdinReader = r
}
