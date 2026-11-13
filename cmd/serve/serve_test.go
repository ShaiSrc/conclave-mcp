package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/config"
)

func newTestConfig() *config.Config {
	c := config.Defaults()
	return c
}

func TestExecute_LogsConfigAndBind(t *testing.T) {
	var buf bytes.Buffer
	cfg := newTestConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 9000
	cfg.DBPath = t.TempDir() + "/data.db"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run Execute in a goroutine; cancel after the boot log
	// lines have been emitted. The previous stub returned
	// immediately; the production code blocks on srv.Start until
	// ctx is cancelled.
	doneCh := make(chan error, 1)
	go func() { doneCh <- Execute(ctx, cfg, &buf) }()

	// Wait briefly for boot logs to flush, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return within 5s after cancel")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 log lines, got %d: %s", len(lines), buf.String())
	}

	// Every line must be valid JSON.
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d not valid JSON: %v\nline: %s", i, err, line)
			continue
		}
		if rec["msg"] == nil {
			t.Errorf("line %d missing msg: %s", i, line)
		}
	}

	// First line should be config-loaded. The "server starting"
	// line comes after migrations, which now produce their own
	// log lines; we search for it rather than hard-coding an index.
	var first map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)

	if first["msg"] != "config loaded" {
		t.Errorf("first msg: got %v, want 'config loaded'", first["msg"])
	}
	if first["port"] != float64(9000) { // JSON numbers decode to float64
		t.Errorf("first port: got %v, want 9000", first["port"])
	}

	var serverStart map[string]any
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "server starting" {
			serverStart = rec
			break
		}
	}
	if serverStart == nil {
		t.Fatalf("no 'server starting' line in output: %s", buf.String())
	}
	if serverStart["bind"] != "127.0.0.1:9000" {
		t.Errorf("bind: got %v, want 127.0.0.1:9000", serverStart["bind"])
	}
}

func TestExecute_NilContext(t *testing.T) {
	var buf bytes.Buffer
	cfg := newTestConfig()
	cfg.DBPath = t.TempDir() + "/data.db"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan error, 1)
	go func() { doneCh <- Execute(ctx, cfg, &buf) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-doneCh

	if !strings.Contains(buf.String(), "config loaded") {
		t.Errorf("expected log output, got %s", buf.String())
	}
}

func TestExecute_DefaultsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	cfg := newTestConfig()
	cfg.DBPath = t.TempDir() + "/data.db"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan error, 1)
	go func() { doneCh <- Execute(ctx, cfg, &buf) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Defaults: 256 KiB, 32 concurrent, 600/min, 730d
	for _, want := range []string{
		`"max_message_bytes":262144`,
		`"max_concurrent_verify":32`,
		`"rate_limit_per_min":600`,
		`"audit_retention":"17520h0m0s"`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log output missing %s, got %s", want, buf.String())
		}
	}
}

func TestNewCmd_RegistersFlags(t *testing.T) {
	cmd := NewCmd()
	if cmd.Use != "serve" {
		t.Errorf("Use: got %q, want serve", cmd.Use)
	}
	for _, name := range []string{"host", "port", "db", "federation-domain"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag, got nil", name)
		}
	}
}

func TestNewCmd_RejectsArgs(t *testing.T) {
	cmd := NewCmd()
	cmd.SetArgs([]string{"extra-positional"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error for extra positional arg, got nil")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The federation domain reaches the server from the env var, and the
// --federation-domain flag wins over it only when given.
func TestNewCmd_FederationDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"env":  {nil, "env.example"},
		"flag": {[]string{"--federation-domain", "flag.example"}, "flag.example"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CONCLAVE_FEDERATION_DOMAIN", "env.example")
			t.Setenv("CONCLAVE_FEDERATION_PORT", fmt.Sprint(freePort(t)))
			var out bytes.Buffer
			cmd := NewCmd()
			cmd.SetArgs(append([]string{"--port", fmt.Sprint(freePort(t)), "--db", t.TempDir() + "/data.db"}, tc.args...))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // boot, then shut down at once
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatalf("serve: %v\n%s", err, out.String())
			}
			for _, want := range []string{fmt.Sprintf(`"federation_domain":%q`, tc.want), `"msg":"federation listener listening"`} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("log output missing %s:\n%s", want, out.String())
				}
			}
		})
	}
}
