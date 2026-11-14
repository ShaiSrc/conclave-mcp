package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
	go func() { doneCh <- Execute(ctx, cfg, "", &buf) }()

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
	go func() { doneCh <- Execute(ctx, cfg, "", &buf) }()
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
	go func() { doneCh <- Execute(ctx, cfg, "", &buf) }()
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

// serve takes the build's version from the root command and logs it at
// boot; it is the version GET /v1/server answers.
func TestNewCmd_Version(t *testing.T) {
	var out bytes.Buffer
	root := &cobra.Command{Use: "conclave-mcp", Version: "9.9.9-test"}
	root.AddCommand(NewCmd())
	root.SetArgs([]string{"serve", "--port", fmt.Sprint(freePort(t)), "--db", t.TempDir() + "/data.db"})
	root.SetOut(&out)
	root.SetErr(&out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // boot, then shut down at once
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("serve: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `"version":"9.9.9-test"`) {
		t.Errorf("boot log lacks the version:\n%s", out.String())
	}
}

// runServe runs `serve args...` with its boot cancelled at once and
// returns the log.
func runServe(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := NewCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // boot, then shut down at once
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

// A deprecated env name in use is logged as a warning at start.
func TestNewCmd_DeprecatedEnvWarns(t *testing.T) {
	t.Setenv("RATE_LIMIT_PER_MIN", "60")
	out, err := runServe(t, "--port", fmt.Sprint(freePort(t)), "--db", t.TempDir()+"/data.db")
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"level":"WARN","msg":"RATE_LIMIT_PER_MIN is deprecated`) || !strings.Contains(out, `"rate_limit_per_min":60`) {
		t.Errorf("no deprecation warning or the old name did not apply:\n%s", out)
	}
}

// Flags win over env, env over the defaults: CONCLAVE_DB_PATH,
// CONCLAVE_HOST and CONCLAVE_PORT apply when the flag is not given.
func TestNewCmd_EnvBelowFlags(t *testing.T) {
	envDB, flagDB := t.TempDir()+"/env.db", t.TempDir()+"/flag.db"
	envPort, flagPort := freePort(t), freePort(t)
	t.Setenv("CONCLAVE_DB_PATH", envDB)
	t.Setenv("CONCLAVE_HOST", "localhost")
	t.Setenv("CONCLAVE_PORT", fmt.Sprint(envPort))

	out, err := runServe(t)
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, out)
	}
	for _, want := range []string{fmt.Sprintf(`"db_path":%q`, envDB), `"host":"localhost"`, fmt.Sprintf(`"port":%d`, envPort)} {
		if !strings.Contains(out, want) {
			t.Errorf("env: log lacks %s:\n%s", want, out)
		}
	}

	out, err = runServe(t, "--db", flagDB, "--host", "127.0.0.1", "--port", fmt.Sprint(flagPort))
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, out)
	}
	for _, want := range []string{fmt.Sprintf(`"db_path":%q`, flagDB), `"host":"127.0.0.1"`, fmt.Sprintf(`"port":%d`, flagPort)} {
		if !strings.Contains(out, want) {
			t.Errorf("flags: log lacks %s:\n%s", want, out)
		}
	}
}

// Before 0.14 serve ignored CONCLAVE_DB_PATH and used the default path.
// When the env names a database that does not exist while the default
// one does, serve refuses rather than start on a new, empty database.
func TestNewCmd_RefusesNewDBBesideOldDefault(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(".conclave-mcp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".conclave-mcp/data.db", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newDB := filepath.Join(dir, "new", "data.db")
	t.Setenv("CONCLAVE_DB_PATH", newDB)
	out, err := runServe(t, "--port", fmt.Sprint(freePort(t)))
	if err == nil || !strings.Contains(err.Error(), newDB) || !strings.Contains(err.Error(), "./.conclave-mcp/data.db") {
		t.Fatalf("serve: %v, want a refusal naming both files\n%s", err, out)
	}
	if _, err := os.Stat(newDB); !os.IsNotExist(err) {
		t.Errorf("new database created: %v", err)
	}
	// With --db the operator chose the file: no check.
	if out, err := runServe(t, "--port", fmt.Sprint(freePort(t)), "--db", newDB); err != nil {
		t.Errorf("serve --db: %v\n%s", err, out)
	}
}

// CONCLAVE_LOG_LEVEL sets the level of the server's log.
func TestExecute_LogLevel(t *testing.T) {
	var buf bytes.Buffer
	cfg := newTestConfig()
	cfg.DBPath = t.TempDir() + "/data.db"
	cfg.Port = freePort(t)
	cfg.LogLevel = "warn"
	cfg.Warnings = []string{"a warning"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(ctx, cfg, "", &buf); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), `"level":"INFO"`) || !strings.Contains(buf.String(), `"msg":"a warning"`) {
		t.Errorf("at warn: %s", buf.String())
	}
}
