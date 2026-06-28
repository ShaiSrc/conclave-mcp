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
	"fmt"
	"log/slog"
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
	var (
		flagHost string
		flagPort int
		flagDB   string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Conclave MCP server",
		Long: `serve loads configuration, initializes storage, and starts the
HTTP control plane on the configured host:port.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load([]string{
				"--host", flagHost,
				"--port", fmt.Sprintf("%d", flagPort),
				"--db", flagDB,
			})
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			return Execute(cmd.Context(), cfg, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&flagHost, "host", "127.0.0.1", "bind host (CONCLAVE_HOST)")
	cmd.Flags().IntVar(&flagPort, "port", 7432, "bind port (CONCLAVE_PORT)")
	cmd.Flags().StringVar(&flagDB, "db", "./.conclave-mcp/data.db", "SQLite file path (CONCLAVE_DB_PATH)")
	return cmd
}

// Execute is the testable core of the serve subcommand. It opens
// the SQLite database, runs migrations, builds the HTTP server,
// and blocks until ctx is cancelled or the listener fails.
//
// stderr is the destination for the boot-time logger. After Start,
// request logging goes through the server's own slog.Logger.
func Execute(ctx context.Context, cfg *config.Config, stderr interface {
	Write(p []byte) (int, error)
}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	bootLog := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	bootLog.Info("config loaded",
		"host", cfg.Host,
		"port", cfg.Port,
		"db_path", cfg.DBPath,
		"max_message_bytes", cfg.MaxMessageBytes,
		"max_concurrent_verify", cfg.MaxConcurrentVerify,
		"rate_limit_per_min", cfg.RateLimitPerMin,
		"audit_retention", cfg.AuditRetention.String(),
	)

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
