package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, as the server's

	"github.com/hermes/conclave-mcp/internal/cli/client"
	"github.com/hermes/conclave-mcp/internal/config"
)

// newBackupCmd builds `backup <file>`: a consistent copy of the database
// while the server runs. It is CLI only: the copy holds the server's
// private federation key, so no endpoint or tool returns it.
func newBackupCmd() *cobra.Command {
	var flagDB string
	cmd := &cobra.Command{
		Use:   "backup <file>",
		Short: "Copy the database to a new file while the server runs",
		Long: `backup writes a consistent copy of the database with VACUUM INTO,
over a read-only connection, while the server keeps running. It refuses
an existing file and writes the copy with mode 0600: it holds the
server's private federation key and every key hash. The copy is made in
a private temporary directory next to the file and linked into place
when complete, so the directory must allow hard links.

It finds the database as serve does: --db, then CONCLAVE_DB_PATH, then
./.conclave-mcp/data.db.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagDB == "" {
				flagDB = os.Getenv("CONCLAVE_DB_PATH")
			}
			if flagDB == "" {
				flagDB = config.Defaults().DBPath
			}
			return runBackup(cmd, flagDB, args[0])
		},
	}
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite file the server runs on (default CONCLAVE_DB_PATH, else ./.conclave-mcp/data.db)")
	return cmd
}

func runBackup(cmd *cobra.Command, dbPath, dest string) error {
	// sql.Open would create a missing database; a typo must not.
	if _, err := os.Stat(dbPath); err != nil {
		return &exitCodeError{Code: 1, Err: fmt.Errorf("database: %w", err)}
	}
	exists := &exitCodeError{Code: 2, Err: fmt.Errorf("%s exists: name a new file", dest)}
	if _, err := os.Lstat(dest); err == nil { // early, before a long copy; the link below decides
		return exists
	}
	// The copy is written in a fresh directory only we can enter (0700)
	// next to dest, and linked to dest once complete: no other local user
	// can swap the file while SQLite writes it by name, and dest never
	// names an empty or half-written file. Link refuses an existing dest.
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".conclave-backup-")
	if err != nil {
		return &exitCodeError{Code: 1, Err: fmt.Errorf("backup: %w", err)}
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, "backup.db")
	if err := vacuumInto(cmd, dbPath, file); err != nil {
		return &exitCodeError{Code: 1, Err: fmt.Errorf("backup: %w", err)}
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return &exitCodeError{Code: 1, Err: fmt.Errorf("backup: %w", err)}
	}
	if err := os.Link(file, dest); errors.Is(err, fs.ErrExist) {
		return exists
	} else if err != nil {
		return &exitCodeError{Code: 1, Err: fmt.Errorf("backup: %w", err)}
	}
	fi, err := os.Stat(dest)
	if err != nil {
		return &exitCodeError{Code: 1, Err: err}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "backup written: %s (%d bytes)\n", dest, fi.Size())
	return nil
}

func vacuumInto(cmd *cobra.Command, dbPath, dest string) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(cmd.Context(), "VACUUM INTO ?", dest)
	return err
}

// serverInfo is the answer of GET /v1/server.
type serverInfo struct {
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
	StartedAt     string `json:"started_at"`
	Federation    struct {
		Enabled  bool   `json:"enabled"`
		Domain   string `json:"domain"`
		Mode     string `json:"mode"`
		Protocol string `json:"protocol"`
	} `json:"federation"`
}

// newServerCmd builds `server`: what the running server is (admin key).
func newServerCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Show the running server: version, schema version, start time, federation (admin key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("unknown --format %q (text|json)", format)
			}
			tok, _ := cmd.Flags().GetString("token")
			srv, _ := cmd.Flags().GetString("server")
			as, _ := cmd.Flags().GetString("as")
			c, err := client.New(client.Options{Server: srv, Token: tok, Agent: as})
			if err != nil {
				return fmt.Errorf("client: %w", err)
			}
			var info serverInfo
			if _, err := c.Do(cmd.Context(), client.Request{Method: "GET", Path: "/v1/server", Out: &info}); err != nil {
				var netErr *client.NetError
				if errors.As(err, &netErr) {
					return &exitCodeError{Code: 3, Err: err}
				}
				return &exitCodeError{Code: 1, Err: err}
			}
			out := cmd.OutOrStdout()
			if format == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fed := "off"
			if f := info.Federation; f.Enabled {
				fed = fmt.Sprintf("on, domain %s, mode %s", f.Domain, f.Mode)
			}
			_, _ = fmt.Fprintf(out, "version:         %s\nschema version:  %d\nstarted:         %s\nfederation:      %s\nprotocol:        %s\n",
				info.Version, info.SchemaVersion, info.StartedAt, fed, info.Federation.Protocol)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}
