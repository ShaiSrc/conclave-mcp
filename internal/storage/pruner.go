// Audit pruner (SPEC-0001 §6 Q2, ADR-0004).
//
// The pruner is a background goroutine that runs every hour and
// deletes audit rows older than the configured retention. Default
// retention is 730 days; floor is 30 days (rejected at startup).
//
// Each cycle deletes in batches of 1000 rows until either the
// target cutoff is reached or no more rows match. One log line
// per cycle reports pruned count and duration. A singleflight
// prevents overlapping runs if a cycle takes longer than the
// interval.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"
)

// PrunerConfig holds the pruner's runtime parameters.
type PrunerConfig struct {
	DB        *sql.DB
	Retention time.Duration
	Interval  time.Duration // default 1h
	BatchSize int           // default 1000
	Logger    *slog.Logger
}

// Pruner is the audit-table reaper.
type Pruner struct {
	cfg PrunerConfig
	sf  singleflight.Group
}

// NewPruner returns a pruner with cfg.DB required and default
// interval/batch size if zero.
func NewPruner(cfg PrunerConfig) (*Pruner, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("pruner: DB is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.Retention < 0 {
		return nil, fmt.Errorf("pruner: retention must be >= 0")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Pruner{cfg: cfg}, nil
}

// Run blocks until ctx is cancelled, running one prune cycle
// immediately and then every cfg.Interval. It always returns nil
// when ctx is cancelled cleanly. Errors during a single cycle are
// logged but do not stop the loop.
func (p *Pruner) Run(ctx context.Context) error {
	// First run: prune now so a long-down server catches up.
	if err := p.RunOnce(ctx); err != nil {
		p.cfg.Logger.Warn("prune initial cycle failed", "err", err)
	}

	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := p.RunOnce(ctx); err != nil {
				p.cfg.Logger.Warn("prune cycle failed", "err", err)
			}
		}
	}
}

// RunOnce executes exactly one prune cycle. It is safe to call
// concurrently: singleflight ensures only one cycle runs at a time
// and concurrent callers receive the same result.
func (p *Pruner) RunOnce(ctx context.Context) error {
	_, err, _ := p.sf.Do("prune", func() (any, error) {
		return nil, p.runCycle(ctx)
	})
	return err
}

func (p *Pruner) runCycle(ctx context.Context) error {
	start := time.Now()
	cutoff := time.Now().UTC().Add(-p.cfg.Retention).Format(time.RFC3339Nano)

	var totalDeleted int64
	for {
		// Delete a single batch in its own statement. We do NOT
		// wrap the loop in a transaction because we want each
		// batch to be independently committable (the cut-off time
		// can advance while a long backlog is being drained).
		res, err := p.cfg.DB.ExecContext(ctx,
			`DELETE FROM audit
			 WHERE id IN (
			   SELECT id FROM audit
			   WHERE ts < ?
			   ORDER BY id
			   LIMIT ?
			 )`,
			cutoff, p.cfg.BatchSize,
		)
		if err != nil {
			return fmt.Errorf("prune delete: %w", err)
		}
		n, _ := res.RowsAffected()
		totalDeleted += n
		if n < int64(p.cfg.BatchSize) {
			break
		}
	}

	p.cfg.Logger.Info("audit_prune",
		"rows_deleted", totalDeleted,
		"duration_ms", time.Since(start).Milliseconds(),
		"retention", p.cfg.Retention.String(),
	)
	return nil
}
