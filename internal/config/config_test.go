package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env that could leak in from the test runner.
	for _, k := range []string{
		"CONCLAVE_HOST", "CONCLAVE_PORT", "CONCLAVE_DB_PATH",
		"CONCLAVE_MAX_MESSAGE_BYTES", "MAX_CONCURRENT_VERIFY",
		"RATE_LIMIT_PER_MIN", "CONCLAVE_AUDIT_RETENTION",
		"CONCLAVE_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}

	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if c.Host != "127.0.0.1" {
		t.Errorf("Host: got %q, want 127.0.0.1", c.Host)
	}
	if c.Port != 7432 {
		t.Errorf("Port: got %d, want 7432", c.Port)
	}
	if c.MaxMessageBytes != 262144 {
		t.Errorf("MaxMessageBytes: got %d, want 262144 (256 KiB)", c.MaxMessageBytes)
	}
	if c.MaxConcurrentVerify != 32 {
		t.Errorf("MaxConcurrentVerify: got %d, want 32", c.MaxConcurrentVerify)
	}
	if c.RateLimitPerMin != 600 {
		t.Errorf("RateLimitPerMin: got %d, want 600", c.RateLimitPerMin)
	}
	if c.AuditRetention != 730*24*time.Hour {
		t.Errorf("AuditRetention: got %s, want 730d", c.AuditRetention)
	}
	if c.ListLimitMessages != 50 {
		t.Errorf("ListLimitMessages: got %d, want 50", c.ListLimitMessages)
	}
	if c.ListLimitAudit != 100 {
		t.Errorf("ListLimitAudit: got %d, want 100", c.ListLimitAudit)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want info", c.LogLevel)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("CONCLAVE_HOST", "0.0.0.0")
	t.Setenv("CONCLAVE_PORT", "9000")
	t.Setenv("CONCLAVE_MAX_MESSAGE_BYTES", "1024")
	t.Setenv("MAX_CONCURRENT_VERIFY", "8")
	t.Setenv("RATE_LIMIT_PER_MIN", "120")
	t.Setenv("CONCLAVE_AUDIT_RETENTION", "60d")
	t.Setenv("CONCLAVE_LOG_LEVEL", "debug")

	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if c.Host != "0.0.0.0" {
		t.Errorf("Host: got %q", c.Host)
	}
	if c.Port != 9000 {
		t.Errorf("Port: got %d", c.Port)
	}
	if c.MaxMessageBytes != 1024 {
		t.Errorf("MaxMessageBytes: got %d", c.MaxMessageBytes)
	}
	if c.MaxConcurrentVerify != 8 {
		t.Errorf("MaxConcurrentVerify: got %d", c.MaxConcurrentVerify)
	}
	if c.RateLimitPerMin != 120 {
		t.Errorf("RateLimitPerMin: got %d, want 120", c.RateLimitPerMin)
	}
	if got, want := c.AuditRetention, 60*24*time.Hour; got != want {
		t.Errorf("AuditRetention: got %s, want %s", got, want)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q", c.LogLevel)
	}
}

func TestLoad_FlagOverridesEnv(t *testing.T) {
	t.Setenv("CONCLAVE_PORT", "9000")

	c, err := Load([]string{"--port", "8000"})
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if c.Port != 8000 {
		t.Errorf("Port: got %d, want 8000 (flag should win over env)", c.Port)
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr error
	}{
		{
			name:    "port too high",
			mutate:  func(c *Config) { c.Port = 70000 },
			wantErr: ErrInvalidPort,
		},
		{
			name:    "port zero",
			mutate:  func(c *Config) { c.Port = 0 },
			wantErr: ErrInvalidPort,
		},
		{
			name:    "max message bytes zero",
			mutate:  func(c *Config) { c.MaxMessageBytes = 0 },
			wantErr: ErrInvalidMaxMessageBytes,
		},
		{
			name:    "max concurrent zero",
			mutate:  func(c *Config) { c.MaxConcurrentVerify = 0 },
			wantErr: ErrInvalidMaxConcurrent,
		},
		{
			name:    "rate limit zero",
			mutate:  func(c *Config) { c.RateLimitPerMin = 0 },
			wantErr: ErrInvalidRateLimit,
		},
		{
			name:    "retention below 30d floor",
			mutate:  func(c *Config) { c.AuditRetention = 10 * 24 * time.Hour },
			wantErr: ErrRetentionTooShort,
		},
		{
			name:    "invalid log level",
			mutate:  func(c *Config) { c.LogLevel = "verbose" },
			wantErr: ErrInvalidLogLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			tt.mutate(c)
			err := c.validate()
			if err == nil {
				t.Fatalf("validate: expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validate: got %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_EnvParseErrors(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		envVal string
	}{
		{"bad port", "CONCLAVE_PORT", "not-a-number"},
		{"bad max bytes", "CONCLAVE_MAX_MESSAGE_BYTES", "huge"},
		{"bad concurrent", "MAX_CONCURRENT_VERIFY", "many"},
		{"bad rate", "RATE_LIMIT_PER_MIN", "fast"},
		{"bad retention", "CONCLAVE_AUDIT_RETENTION", "forever"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envKey, tt.envVal)
			_, err := Load([]string{})
			if err == nil {
				t.Fatalf("Load: expected error for %s=%q, got nil", tt.envKey, tt.envVal)
			}
			if !strings.Contains(err.Error(), tt.envKey) {
				t.Errorf("error %q should mention env var %s", err, tt.envKey)
			}
		})
	}
}

func TestValidate_RetentionAtFloor(t *testing.T) {
	// 720h is the minimum, should pass
	c := defaults()
	c.AuditRetention = 720 * time.Hour
	if err := c.validate(); err != nil {
		t.Errorf("validate at 720h floor: unexpected error %v", err)
	}

	// 719h should fail
	c.AuditRetention = 719 * time.Hour
	if err := c.validate(); !errors.Is(err, ErrRetentionTooShort) {
		t.Errorf("validate at 719h: got %v, want ErrRetentionTooShort", err)
	}
}

func TestParseRetention(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"60d", 60 * 24 * time.Hour, false},
		{"12w", 12 * 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"", 0, true},
		{"d", 0, true},   // no number
		{"0d", 0, true},  // zero not allowed
		{"-5d", 0, true}, // negative
		{"forever", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseRetention(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRetention(%q): err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseRetention(%q): got %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
