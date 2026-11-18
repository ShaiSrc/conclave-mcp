// Package serve implements the `conclave-mcp serve` subcommand.
//
// In v0.1 this subcommand starts the real HTTP control plane
// (internal/server.Server). Earlier revisions of this file
// contained a stub that only logged "server starting"; the
// production server was always meant to be wired in here.
// This change closes the gap so the binary is functional, not
// just smoke-tested.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/config"
	"github.com/hermes/conclave-mcp/internal/events"
	"github.com/hermes/conclave-mcp/internal/server"
	"github.com/hermes/conclave-mcp/internal/storage"
	"github.com/hermes/conclave-mcp/migrations"
)

// NewCmd builds the serve subcommand. It is exported so the root
// command can wire it in. The run logic is split out into Execute
// so unit tests can drive it without going through cobra.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Conclave MCP server",
		Long: `serve loads configuration, initializes storage, and starts the
HTTP control plane on the configured host:port.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Flags are passed only when given, so the env vars apply
			// without them: flag, then env, then default.
			var args []string
			for _, name := range []string{"host", "port", "db", "federation-domain"} {
				if f := cmd.Flags().Lookup(name); f.Changed {
					args = append(args, "--"+name, f.Value.String())
				}
			}
			cfg, err := config.Load(args)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			if !cmd.Flags().Changed("db") && os.Getenv("CONCLAVE_DB_PATH") != "" {
				if err := checkOldDefault(cfg.DBPath); err != nil {
					return err
				}
			}
			// The root command carries the build's version (main.version).
			return Execute(cmd.Context(), cfg, cmd.Root().Version, cmd.ErrOrStderr())
		},
	}
	d := config.Defaults()
	cmd.Flags().String("host", d.Host, "bind host (CONCLAVE_HOST)")
	cmd.Flags().Int("port", d.Port, "bind port (CONCLAVE_PORT)")
	cmd.Flags().String("db", d.DBPath, "SQLite file path (CONCLAVE_DB_PATH)")
	cmd.Flags().String("federation-domain", "", "federation domain; empty turns federation off (CONCLAVE_FEDERATION_DOMAIN)")
	return cmd
}

// checkOldDefault refuses a CONCLAVE_DB_PATH that does not exist while
// the default database does: serve before 0.14 ignored the env var and
// used the default, so starting would leave the data behind and begin
// on an empty database (with a new federation key).
func checkOldDefault(path string) error {
	old := config.Defaults().DBPath
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	return fmt.Errorf("CONCLAVE_DB_PATH names %s, which does not exist, but %s does: serve before 0.14 ignored CONCLAVE_DB_PATH and used %s. "+
		"Stop, move that database with its -wal and -shm files to %s and start again, or set CONCLAVE_DB_PATH to the old path", path, old, old, path)
}

// Execute is the testable core of the serve subcommand. It opens
// the SQLite database, runs migrations, builds the HTTP server,
// and blocks until ctx is cancelled or the listener fails.
//
// version is the build's version, for GET /v1/server. stderr is the
// destination for the boot-time logger. After Start, request logging
// goes through the server's own slog.Logger.
func Execute(ctx context.Context, cfg *config.Config, version string, stderr interface {
	Write(p []byte) (int, error)
}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var level slog.Level                          // info unless set
	_ = level.UnmarshalText([]byte(cfg.LogLevel)) // validated by config.Load
	bootLog := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	bootLog.Info("config loaded",
		"version", version,
		"host", cfg.Host,
		"port", cfg.Port,
		"db_path", cfg.DBPath,
		"max_message_bytes", cfg.MaxMessageBytes,
		"max_concurrent_verify", cfg.MaxConcurrentVerify,
		"rate_limit_per_min", cfg.RateLimitPerMin,
		"audit_retention", cfg.AuditRetention.String(),
		"federation_domain", cfg.FederationDomain,
	)
	for _, w := range cfg.Warnings {
		bootLog.Warn(w)
	}

	// Use a fresh context for the init phase (storage.Open +
	// migrations) so that a user-cancel during the boot sequence
	// does not abort migrations mid-flight. The init phase is
	// bounded by an explicit 30s deadline; if migrations take
	// longer than that, something is genuinely wrong and we
	// want to fail loudly. The user-provided ctx is only used
	// for the serving phase below, so cancelling it triggers a
	// graceful shutdown without interrupting init.
	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()

	// Open storage. The Open helper enforces dir 0700 + file 0600
	// (SPEC-0002). If it fails, we exit with a clear error rather
	// than booting an HTTP server that cannot answer /healthz.
	db, cleanup, err := storage.Open(initCtx, cfg.DBPath)
	if err != nil {
		bootLog.Error("storage open failed", "err", err, "path", cfg.DBPath)
		return fmt.Errorf("open db %q: %w", cfg.DBPath, err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			bootLog.Warn("storage close", "err", err)
		}
	}()

	if _, err := storage.Migrate(initCtx, db, migrations.FS(), bootLog); err != nil {
		bootLog.Error("migrate failed", "err", err)
		return fmt.Errorf("migrate: %w", err)
	}

	// Build the server. The deps carry the open DB and the
	// request logger.
	// In-process broker for SSE events (ADR-0009). Lives
	// for the lifetime of the server.
	broker := events.NewBroker()
	defer broker.Close()
	srv, err := server.New(server.Config{
		Host:        cfg.Host,
		Port:        cfg.Port,
		ReadHeader:  5 * 1e9,   // 5s
		IdleTimeout: 120 * 1e9, // 120s
		Logger:      bootLog,
		Version:     version,

		FederationDomain:       cfg.FederationDomain,
		FederationHost:         cfg.FederationHost,
		FederationPort:         cfg.FederationPort,
		FederationMode:         cfg.FederationMode,
		FederationInsecureHTTP: cfg.FederationInsecureHTTP,
	}, server.Deps{
		DB:                  db,
		MaxMessageBytes:     cfg.MaxMessageBytes,
		MaxConcurrentVerify: cfg.MaxConcurrentVerify,
		RateLimitPerMin:     cfg.RateLimitPerMin,
		Logger:              bootLog,
		Broker:              broker,
	})
	if err != nil {
		bootLog.Error("server build failed", "err", err)
		return fmt.Errorf("server new: %w", err)
	}

	bootLog.Info("server starting", "bind", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
	if err := srv.Start(ctx); err != nil {
		bootLog.Error("server stopped with error", "err", err)
		return err
	}
	bootLog.Info("server stopped cleanly")
	return nil
}
