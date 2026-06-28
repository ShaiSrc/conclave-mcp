// Migration runner for Conclave MCP.
//
// Migrate applies every *.sql file from dir (an fs.FS) to the
// database, in lexicographic order, in a single transaction per
// file, and idempotently: a file whose numeric prefix is already
// recorded in meta.schema_version is skipped.
//
// The version is the integer prefix of the filename before the first
// underscore (e.g. "0001_initial.sql" -> 1).
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrate applies any pending migrations from dir to db. The current
// schema version is read from meta.schema_version; if absent (fresh
// DB or no meta table), all migrations in dir are applied.
//
// Returns the list of applied migration versions, in order. An empty
// slice means "nothing to do".
func Migrate(ctx context.Context, db *sql.DB, dir fs.FS, logger *slog.Logger) ([]int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	currentVersion, err := readSchemaVersion(ctx, db)
	if err != nil {
		return nil, err
	}

	files, err := collectMigrations(dir)
	if err != nil {
		return nil, err
	}

	var applied []int
	for _, m := range files {
		if m.version <= currentVersion {
			continue
		}
		start := time.Now()
		if err := applyOne(ctx, db, m); err != nil {
			return applied, fmt.Errorf("apply %s: %w", m.name, err)
		}
		logger.Info("migration applied",
			"version", m.version,
			"name", m.name,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		applied = append(applied, m.version)
	}

	return applied, nil
}

func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	row := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'")
	var v string
	err := row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		// meta table itself may not exist yet (fresh DB, pre-0001)
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("meta.schema_version is not an int: %q", v)
	}
	return n, nil
}

type migration struct {
	version int
	name    string
	content string
}

func collectMigrations(dir fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		base := strings.TrimSuffix(name, ".sql")
		// Migrations must be named "NNNN_..." or "NNNN". A leading
		// underscore (e.g. "_helpers.sql") is reserved for shared
		// SQL fragments and is ignored at migration-discovery time.
		if strings.HasPrefix(base, "_") {
			continue
		}
		numStr := base
		if idx := strings.IndexByte(base, '_'); idx > 0 {
			numStr = base[:idx]
		}
		num, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("migration %q: leading number not an int", name)
		}
		raw, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, migration{version: num, name: name, content: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	// One transaction per file. If any statement fails the whole
	// file is rolled back; the next boot will retry.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.content); err != nil {
		_ = tx.Rollback()
		return err
	}
	// After running the file, record the schema_version. We use
	// upsert so we don't depend on the SQL having inserted it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(m.version),
	); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
