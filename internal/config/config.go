// Package config loads Conclave MCP configuration from environment
// variables and command-line flags.
//
// All settings have safe defaults; required ones fail-fast at Load.
// See SPEC-0001 §Scope, SPEC-0003 §6 (limits), and ADR-0004 (retention).
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the validated runtime configuration for a single
// conclave-mcp process.
type Config struct {
	// Network
	Host string // bind host, default 127.0.0.1 (SPEC-0001 §7)
	Port int    // bind port, default 7432 (SPEC-0001 §7)

	// Storage
	DBPath string // SQLite file path, default ./.conclave-mcp/data.db

	// Limits (SPEC-0003 §6, ADR-0003, ADR-0004)
	MaxMessageBytes     int           // 256 KiB default, payload_too_large at 413
	MaxConcurrentVerify int           // 32 default, semaphore cap for argon2id
	RateLimitPerMin     int           // 600 default per key
	AuditRetention      time.Duration // 730d default, 720h floor (ADR-0004)
	ListLimitMessages   int           // 50 default (SPEC-0003 §6)
	ListLimitAudit      int           // 100 default (SPEC-0003 §6)

	// Logging
	LogLevel string // debug|info|warn|error, default info
}

// Load builds a Config from defaults, then overlays environment
// variables, then overlays command-line flags. It returns an error
// if any value fails validation.
func Load(args []string) (*Config, error) {
	c := Defaults()

	if err := applyEnv(c); err != nil {
		return nil, err
	}

	if err := applyFlags(c, args); err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return c, nil
}

// Defaults returns a Config populated with safe defaults per
// SPEC-0001, SPEC-0003 §6, and ADR-0004. Exported so other packages
// (e.g. cmd/serve) can build test fixtures without re-deriving them.
func Defaults() *Config {
	return defaults()
}

func defaults() *Config {
	return &Config{
		Host:                "127.0.0.1",
		Port:                7432,
		DBPath:              "./.conclave-mcp/data.db",
		MaxMessageBytes:     262144, // 256 KiB
		MaxConcurrentVerify: 32,
		RateLimitPerMin:     600,
		AuditRetention:      730 * 24 * time.Hour, // ADR-0004
		ListLimitMessages:   50,
		ListLimitAudit:      100,
		LogLevel:            "info",
	}
}

func applyEnv(c *Config) error {
	// helper: empty string from t.Setenv("X", "") should not override.
	lookup := func(key string) (string, bool) {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return "", false
		}
		return v, true
	}

	if v, ok := lookup("CONCLAVE_HOST"); ok {
		c.Host = v
	}
	if v, ok := lookup("CONCLAVE_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CONCLAVE_PORT: %w", err)
		}
		c.Port = n
	}
	if v, ok := lookup("CONCLAVE_DB_PATH"); ok {
		c.DBPath = v
	}
	if v, ok := lookup("CONCLAVE_MAX_MESSAGE_BYTES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CONCLAVE_MAX_MESSAGE_BYTES: %w", err)
		}
		c.MaxMessageBytes = n
	}
	if v, ok := lookup("MAX_CONCURRENT_VERIFY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("MAX_CONCURRENT_VERIFY: %w", err)
		}
		c.MaxConcurrentVerify = n
	}
	if v, ok := lookup("RATE_LIMIT_PER_MIN"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("RATE_LIMIT_PER_MIN: %w", err)
		}
		c.RateLimitPerMin = n
	}
	if v, ok := lookup("CONCLAVE_AUDIT_RETENTION"); ok {
		d, err := parseRetention(v)
		if err != nil {
			return fmt.Errorf("CONCLAVE_AUDIT_RETENTION: %w", err)
		}
		c.AuditRetention = d
	}
	if v, ok := lookup("CONCLAVE_LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	return nil
}

func applyFlags(c *Config, args []string) error {
	fs := flag.NewFlagSet("conclave-mcp", flag.ContinueOnError)
	// Suppress flag library's own output on parse error; we return it.
	fs.SetOutput(os.Stderr)

	host := fs.String("host", c.Host, "bind host (CONCLAVE_HOST)")
	port := fs.Int("port", c.Port, "bind port (CONCLAVE_PORT)")
	dbPath := fs.String("db", c.DBPath, "SQLite file path (CONCLAVE_DB_PATH)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	c.Host = *host
	c.Port = *port
	c.DBPath = *dbPath
	return nil
}

var (
	ErrInvalidPort            = errors.New("port must be 1..65535")
	ErrInvalidMaxMessageBytes = errors.New("max message bytes must be > 0")
	ErrInvalidMaxConcurrent   = errors.New("max concurrent verify must be > 0")
	ErrInvalidRateLimit       = errors.New("rate limit per min must be > 0")
	ErrRetentionTooShort      = errors.New("audit retention must be >= 720h (30d)")
	ErrInvalidLogLevel        = errors.New("log level must be one of: debug, info, warn, error")
)

// parseRetention accepts Go duration strings plus shorthand d (days)
// and w (weeks) so operators can write "60d" or "12w".
func parseRetention(s string) (time.Duration, error) {
	if len(s) > 1 && (s[len(s)-1] == 'd' || s[len(s)-1] == 'w') {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid day/week count in %q", s)
		}
		if s[len(s)-1] == 'd' {
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d: %w", c.Port, ErrInvalidPort)
	}
	if c.MaxMessageBytes <= 0 {
		return ErrInvalidMaxMessageBytes
	}
	if c.MaxConcurrentVerify <= 0 {
		return ErrInvalidMaxConcurrent
	}
	if c.RateLimitPerMin <= 0 {
		return ErrInvalidRateLimit
	}
	if c.AuditRetention < 720*time.Hour {
		return fmt.Errorf("%w: got %s", ErrRetentionTooShort, c.AuditRetention)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("%q: %w", c.LogLevel, ErrInvalidLogLevel)
	}
	return nil
}
