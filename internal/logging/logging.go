// Package logging configures the structured logger for conclave-mcp.
//
// The server uses log/slog (Go 1.21+ stdlib) with a JSON handler in
// production and a text handler in development. All log lines go to
// stderr so stdout can be reserved for future CLI data output
// (SPEC-0001 C7: no secrets in logs).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Setup installs a slog.Logger with the requested level. Returns the
// logger so the caller can attach it to context and so tests can
// inspect emitted records.
//
// Level is one of: debug, info, warn, error. Unknown values produce
// a warn-level fallback and a synthetic record explaining the choice.
func Setup(level string, stderr io.Writer) *slog.Logger {
	if stderr == nil {
		stderr = os.Stderr
	}
	lvl, err := parseLevel(level)
	if err != nil {
		// We can't return an error from a void-style setup, so we
		// log a synthetic record via a fresh logger and continue
		// with info level.
		fallback := slog.New(slog.NewJSONHandler(stderr, nil))
		fallback.Warn("invalid log level, falling back to info",
			"requested", level, "allowed", "debug|info|warn|error")
		return fallback.With("fallback_reason", "invalid_level")
	}
	handler := slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, &unknownLevelError{s: s}
	}
}

type unknownLevelError struct{ s string }

func (e *unknownLevelError) Error() string {
	return "unknown log level: " + e.s
}
