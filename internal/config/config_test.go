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
		"CONCLAVE_LOG_LEVEL", "CONCLAVE_FEDERATION_DOMAIN",
		"CONCLAVE_FEDERATION_HOST", "CONCLAVE_FEDERATION_PORT",
		"CONCLAVE_FEDERATION_MODE", "CONCLAVE_FEDERATION_INSECURE_HTTP",
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
	if c.FederationDomain != "" || c.FederationHost != "127.0.0.1" || c.FederationPort != 7433 ||
		c.FederationMode != "allowlist" || c.FederationInsecureHTTP {
		t.Errorf("federation defaults: got domain=%q host=%q port=%d mode=%q insecure=%v",
			c.FederationDomain, c.FederationHost, c.FederationPort, c.FederationMode, c.FederationInsecureHTTP)
	}
}

func TestLoad_FederationEnv(t *testing.T) {
	t.Setenv("CONCLAVE_FEDERATION_DOMAIN", "Conclave.Example.org")
	t.Setenv("CONCLAVE_FEDERATION_HOST", "0.0.0.0")
	t.Setenv("CONCLAVE_FEDERATION_PORT", "8433")
	t.Setenv("CONCLAVE_FEDERATION_MODE", "open")
	t.Setenv("CONCLAVE_FEDERATION_INSECURE_HTTP", "1")

	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FederationDomain != "conclave.example.org" {
		t.Errorf("FederationDomain: got %q, want it lowercased", c.FederationDomain)
	}
	if c.FederationHost != "0.0.0.0" || c.FederationPort != 8433 || c.FederationMode != "open" || !c.FederationInsecureHTTP {
		t.Errorf("got host=%q port=%d mode=%q insecure=%v",
			c.FederationHost, c.FederationPort, c.FederationMode, c.FederationInsecureHTTP)
	}

	c, err = Load([]string{"--federation-domain", "b.example:7433"})
	if err != nil {
		t.Fatalf("Load with flag: %v", err)
	}
	if c.FederationDomain != "b.example:7433" {
		t.Errorf("FederationDomain: got %q, want the flag to win over env", c.FederationDomain)
	}
}

// Without a federation domain the federation settings are not checked:
// federation off changes nothing.
func TestValidate_FederationOff(t *testing.T) {
	c := defaults()
	c.FederationMode = "bogus"
	c.FederationPort = c.Port
	if err := c.validate(); err != nil {
		t.Errorf("validate with federation off: %v", err)
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
		{
			name:    "invalid federation domain",
			mutate:  func(c *Config) { c.FederationDomain = "bad..example" },
			wantErr: ErrInvalidFederationDomain,
		},
		{
			name: "invalid federation mode",
			mutate: func(c *Config) {
				c.FederationDomain = "a.example"
				c.FederationMode = "closed"
			},
			wantErr: ErrInvalidFederationMode,
		},
		{
			name: "federation port zero",
			mutate: func(c *Config) {
				c.FederationDomain = "a.example"
				c.FederationPort = 0
			},
			wantErr: ErrInvalidFederationPort,
		},
		{
			name: "federation listener equals main listener",
			mutate: func(c *Config) {
				c.FederationDomain = "a.example"
				c.FederationPort = c.Port
			},
			wantErr: ErrFederationListener,
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

// With federation off, bad federation port and insecure-HTTP values are
// ignored.
func TestLoad_BadFederationEnvIgnoredWhenOff(t *testing.T) {
	t.Setenv("CONCLAVE_FEDERATION_PORT", "x")
	t.Setenv("CONCLAVE_FEDERATION_INSECURE_HTTP", "yes")
	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load with federation off: %v", err)
	}
	if c.FederationPort != 7433 || c.FederationInsecureHTTP {
		t.Errorf("got port %d insecure %v, want the defaults", c.FederationPort, c.FederationInsecureHTTP)
	}
}

func TestSameListener(t *testing.T) {
	tests := []struct {
		h1   string
		p1   int
		h2   string
		p2   int
		want bool
	}{
		{"127.0.0.1", 7432, "127.0.0.1", 7433, false},
		{"127.0.0.1", 7432, "127.0.0.1", 7432, true},
		{"127.0.0.1", 7432, "::ffff:127.0.0.1", 7432, true},
		{"::1", 7432, "[::1]", 7432, true},
		{"::1", 7432, "0:0:0:0:0:0:0:1", 7432, true},
		{"::1", 7432, "127.0.0.1", 7432, false},
		{"0.0.0.0", 7432, "127.0.0.1", 7432, true},
		{"127.0.0.1", 7432, "0.0.0.0", 7432, true},
		{"::", 7432, "10.0.0.5", 7432, true},
		{"::", 7432, "::1", 7432, true},
		{"", 7432, "10.0.0.5", 7432, true},
		{"0.0.0.0", 7432, "127.0.0.1", 7433, false},
		{"LocalHost", 7432, "localhost", 7432, true},
		{"a.example", 7432, "b.example", 7432, false},
		{"0.0.0.0", 7432, "localhost", 7432, true},
		{"10.0.0.5", 7432, "localhost", 7432, false},
	}
	for _, tt := range tests {
		if got := sameListener(tt.h1, tt.p1, tt.h2, tt.p2); got != tt.want {
			t.Errorf("sameListener(%q:%d, %q:%d) = %v, want %v", tt.h1, tt.p1, tt.h2, tt.p2, got, tt.want)
		}
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
		{"bad federation port", "CONCLAVE_FEDERATION_PORT", "x"},
		{"bad federation insecure http", "CONCLAVE_FEDERATION_INSECURE_HTTP", "yes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCLAVE_FEDERATION_DOMAIN", "a.example")
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
