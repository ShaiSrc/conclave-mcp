package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestSetup_DefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup("info", &buf)
	logger.Info("hello", "key", "value")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw: %s", err, buf.String())
	}
	if record["msg"] != "hello" {
		t.Errorf("msg: got %v, want hello", record["msg"])
	}
	if record["level"] != "INFO" {
		t.Errorf("level: got %v, want INFO", record["level"])
	}
	if record["key"] != "value" {
		t.Errorf("key: got %v, want value", record["key"])
	}
}

func TestSetup_RespectsDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup("debug", &buf)
	logger.Debug("debug-msg")
	logger.Info("info-msg")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "debug-msg") {
		t.Errorf("first line should be debug, got %s", lines[0])
	}
	if !strings.Contains(lines[1], "info-msg") {
		t.Errorf("second line should be info, got %s", lines[1])
	}
}

func TestSetup_RespectsWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup("warn", &buf)
	logger.Info("info-msg")
	logger.Warn("warn-msg")
	logger.Error("error-msg")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("expected info to be filtered, got %d lines: %s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "warn-msg") {
		t.Errorf("first line should be warn, got %s", lines[0])
	}
	if !strings.Contains(lines[1], "error-msg") {
		t.Errorf("second line should be error, got %s", lines[1])
	}
}

func TestSetup_InvalidLevelFallsBack(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup("verbose", &buf)
	logger.Info("after-fallback")

	if !strings.Contains(buf.String(), "invalid log level") {
		t.Errorf("expected warn about invalid level, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "after-fallback") {
		t.Errorf("expected info record to be emitted, got %s", buf.String())
	}
}

func TestSetup_NilStderrUsesOS(t *testing.T) {
	// Just confirm nil stderr doesn't panic. We can't easily intercept
	// os.Stderr here, but we can at least make sure Setup returns
	// a usable logger.
	logger := Setup("info", nil)
	if logger == nil {
		t.Fatal("Setup returned nil logger for nil stderr")
	}
	logger.Info("ping")
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{"", slog.LevelInfo, false},
		{" warn ", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"verbose", slog.LevelInfo, true},
		{"trace", slog.LevelInfo, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseLevel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseLevel(%q): err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseLevel(%q): got %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
