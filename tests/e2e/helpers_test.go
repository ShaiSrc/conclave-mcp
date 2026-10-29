// Package e2e contains end-to-end tests that exercise the real
// HTTP server, the real SQLite database, and the real HTTP auth
// pipeline against an in-process Server instance. The tests do not
// use the test server's httptest.NewServer wrapper: they bind a
// real loopback port, real net.Listener, real timeouts.
//
// For the binary-spawn variant (real cmd/conclave-mcp process,
// real config file, real env), see e2e_subprocess_test.go.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, same as the server

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/server"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// testEnv is the in-process server harness.
//
// One testEnv == one Server == one SQLite file == one port.
// All resources are torn down in t.Cleanup().
type testEnv struct {
	t       *testing.T
	dir     string
	dbPath  string
	cfg     server.Config
	server  *server.Server
	addr    string
	baseURL string
	cancel  context.CancelFunc

	// Two distinct API keys, owned by two distinct "agents". Used
	// by tests that simulate two callers without involving a real
	// LLM (see e2e_two_clients_test.go).
	keyA apiKey
	keyB apiKey

	httpClient *http.Client
}

// apiKey holds the plaintext token, the row id, and the prefix that
// the server indexes. The plaintext is the only way to authenticate;
// we keep it in the test env for the lifetime of the test only.
type apiKey struct {
	ID        int64
	Prefix    string
	Plaintext string
}

// newTestEnv spins up an in-process server on a random loopback port,
// runs migrations, creates two API keys, and returns the harness.
//
// The server runs in a goroutine and is stopped via the returned
// cleanup. The DB file is created with mode 0600 in a 0700 dir.
// waitReady polls /healthz until it returns 200 or the timeout
// expires. The server logs "server starting" before Listen, and
// Listen is followed by the first Accept — a small delay is
// expected on slow CI machines.
func (e *testEnv) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := e.httpClient.Get(e.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not become healthy within %s", e.addr, timeout)
}

// createKey inserts a key directly. We bypass the HTTP layer
// because tests need both keys to exist BEFORE the test body runs;
// otherwise the first test action (e.g. "create room with A") is
// racing against the bootstrap. The hashing uses the same argon2id
// parameters the production POST /v1/keys uses (auth.DefaultParams).
//
// The label includes a per-process counter so two test envs against
// the same database file (TestLifecycle_MigrationIdempotent) do
// not collide on the api_keys.label UNIQUE constraint.
var createKeyCounter int64

func (e *testEnv) createKey(label string) apiKey {
	e.t.Helper()

	plaintext, hash, err := auth.GenerateToken()
	if err != nil {
		e.t.Fatalf("GenerateToken: %v", err)
	}
	if len(plaintext) < 11 || plaintext[:3] != "tk_" {
		e.t.Fatalf("unexpected plaintext shape: %q", plaintext)
	}
	prefix := plaintext[3:11]

	// Combine the caller's label with a monotonic counter and
	// a short timestamp. The counter is package-global so two
	// envs on the same DB still get distinct labels.
	n := atomicAddInt64(&createKeyCounter, 1)
	unique := fmt.Sprintf("%s-%d-%d", label, time.Now().UnixNano(), n)

	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO api_keys(prefix, hash, label, created_at) VALUES (?, ?, ?, ?)`,
		prefix, hash, unique, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		e.t.Fatalf("insert key: %v", err)
	}
	id, _ := res.LastInsertId()
	return apiKey{ID: id, Prefix: prefix, Plaintext: plaintext}
}

// atomicAddInt64 wraps sync/atomic to avoid the import noise at
// the top of the file. Used only by createKey for the counter.
func atomicAddInt64(p *int64, delta int64) int64 {
	return syncAddInt64(p, delta)
}

// call issues an authenticated HTTP request against the server.
// The body, if non-nil, is JSON-encoded. The response body is read
// fully and returned alongside the status.
type httpResp struct {
	Status int
	Header http.Header
	Body   []byte
	// Err is set when the HTTP request itself failed at the
	// transport layer (timeout, connection refused, DNS, etc.).
	// In that case Status is 0 and Body is nil. Tests that need
	// to distinguish transport failures from application
	// responses (e.g. the load test, which counts timeouts as
	// their own bucket) should check Err first.
	Err error
}

func (e *testEnv) call(method, path, token string, body any) httpResp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return httpResp{Status: 0, Err: fmt.Errorf("marshal: %w", err)}
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.baseURL+path, rdr)
	if err != nil {
		return httpResp{Status: 0, Err: fmt.Errorf("NewRequest: %w", err)}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		// Return a structured failure instead of t.Fatalf so
		// that load tests can count timeouts as a separate
		// bucket rather than killing the whole test process.
		return httpResp{Status: 0, Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// callAs is shorthand for env.call with a known key.
func (e *testEnv) callAs(method, path string, k apiKey, body any) httpResp {
	return e.call(method, path, k.Plaintext, body)
}

// portFromAddr parses "host:port" into int port.
func portFromAddr(addr string) int {
	_, ps, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscanf(ps, "%d", &p)
	return p
}

// isShutdownErr reports whether err is the expected
// "use of closed network connection" or http.ErrServerClosed.
func isShutdownErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Server closed") ||
		strings.Contains(s, "use of closed network connection") ||
		err == http.ErrServerClosed
}

// randomToken is a small helper for tests that want a syntactically
// valid but server-unknown bearer. Length matches production keys.
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "tk_" + hex.EncodeToString(b)
}

// -------- subprocess harness (real binary) --------

// subprocessEnv runs the real `conclave-mcp serve` binary as a child
// process. Use this when the test needs to verify binary behaviour
// (flag parsing, config file, real exit codes). The Go in-process
// env is faster and more flexible, but does not catch binary-only
// regressions.
type subprocessEnv struct {
	t          *testing.T
	cmd        *exec.Cmd
	addr       string
	cancel     context.CancelFunc
	dataDir    string
	binaryPath string
}

// startSubprocess builds, then spawns the binary on a random port.
// Returns when /healthz responds 200, or fails the test.
func startSubprocess(t *testing.T) *subprocessEnv {
	t.Helper()

	// Locate the binary. `go test` puts it next to the test binary
	// via `go build -o`, but `go test` itself does not produce a
	// binary we can spawn. Build a throwaway one in t.TempDir.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "conclave-mcp")

	// We use `go build` from the repo root to keep this portable
	// across go versions and avoid the cmd/...->cmd/conclave-mcp
	// aliasing issue.
	repoRoot := repoRoot(t)
	build := exec.Command("go", "build", "-o", binPath, "./cmd/conclave-mcp")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	// Pick a port. Same race-prone pattern as the in-process env.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	port := portFromAddr(addr)
	cmd := exec.CommandContext(ctx, binPath, "serve",
		"--host", "127.0.0.1",
		fmt.Sprintf("--port=%d", port),
		"--db", filepath.Join(dataDir, "data.db"),
	)
	cmd.Dir = dataDir
	// The serve subcommand reads its config from flags, not env
	// vars. Strip the inherited env so a stray CONCLAVE_PORT
	// from the test runner cannot override our --port flag.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	// Capture both stdout and stderr for diagnostics. The serve
	// stub logs to stderr; a missing port-bind error would also
	// show up there. Stderr alone was empty in early runs and
	// the test failed with "did not become healthy", which is
	// what we are trying to root-cause.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary: %v", err)
	}

	env := &subprocessEnv{
		t:          t,
		cmd:        cmd,
		addr:       addr,
		cancel:     cancel,
		dataDir:    dataDir,
		binaryPath: binPath,
	}

	t.Cleanup(func() {
		// SIGTERM via cancel; if the process is stuck, the test
		// binary will report it as a hang and abort. We do NOT
		// call cmd.Process.Kill() because that masks shutdown
		// bugs we want to see.
		cancel()
		doneCh := make(chan struct{})
		var waitErr error
		go func() {
			waitErr = cmd.Wait()
			close(doneCh)
		}()
		select {
		case <-doneCh:
			if waitErr != nil && !strings.Contains(waitErr.Error(), "exit status") {
				t.Logf("subprocess wait error: %v", waitErr)
			}
		case <-time.After(3 * time.Second):
			t.Logf("subprocess did not exit within 3s; captured stderr: %s", stderr.String())
			_ = cmd.Process.Kill()
		}
	})

	// Wait for ready.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return env
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("subprocess did not become healthy; stderr: %s", stderr.String())
	return env
}

func (s *subprocessEnv) URL() string { return "http://" + s.addr }

// binaryPath is exported so tests can spawn CLI subcommands
// against the same binary that the server is running.
func (s *subprocessEnv) BinaryPath() string { return s.binaryPath }

// DataDir returns the temp data dir used by this server. Tests
// that exercise admin vacuum (SPEC-0004 §5.1) need it because
// the vacuum command opens the DB file directly.
func (s *subprocessEnv) DataDir() string { return s.dataDir }

// call issues an authenticated HTTP request against the
// subprocess server. Mirrors the (testEnv).call signature so
// surface tests can be written against either env type.
func (s *subprocessEnv) call(method, path, token string, body any) httpResp {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, s.URL()+path, rdr)
	if err != nil {
		s.t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// apiKey is re-declared on subprocessEnv so the same test code
// works against either env type. (The in-process testEnv also
// has its own apiKey; we use the same struct shape.)
type subprocessAPIKey struct {
	ID        int64
	Prefix    string
	Plaintext string
}

// createBootstrapKey creates one key directly in the
// subprocess server's SQLite file. We bypass the HTTP layer
// because the subprocess server has no bootstrap key on first
// run — POST /v1/keys requires an existing caller (SPEC-0003
// §3.3). Inserting the key directly is acceptable here: the
// test only needs a valid bearer; the keys service's HTTP path
// is covered by TestE2E_Surface_KeysCRUD.
//
// Returns the plaintext token (only known to the test), the
// row id (for the actor_key_id in audit assertions), and the
// prefix (for completeness).
func (s *subprocessEnv) createBootstrapKey(label string) subprocessAPIKey {
	s.t.Helper()

	plaintext, hash, err := auth.GenerateToken()
	if err != nil {
		s.t.Fatalf("GenerateToken: %v", err)
	}
	if len(plaintext) < 11 || plaintext[:3] != "tk_" {
		s.t.Fatalf("unexpected plaintext shape: %q", plaintext)
	}
	prefix := plaintext[3:11]

	// Combine with a monotonic counter so two test envs against
	// the same DB (TestLifecycle_MigrationIdempotent) do not
	// collide on api_keys.label UNIQUE.
	n := atomicAddInt64(&createKeyCounter, 1)
	unique := fmt.Sprintf("%s-%d-%d", label, time.Now().UnixNano(), n)

	dbPath := filepath.Join(s.dataDir, "data.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		s.t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO api_keys(prefix, hash, label, created_at, is_admin) VALUES (?, ?, ?, ?, 1)`,
		prefix, hash, unique, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		s.t.Fatalf("insert key: %v", err)
	}
	id, _ := res.LastInsertId()
	return subprocessAPIKey{ID: id, Prefix: prefix, Plaintext: plaintext}
}

// repoRoot walks up from the test file's directory until it finds
// go.mod. Used so that tests work regardless of `go test` working
// directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found above %s", wd)
	return ""
}

// close releases the resources held by the env without deleting the
// underlying directory. Tests that need to re-open the same files
// (e.g. migration-idempotency check) call this and then construct
// a second env on the same dir.
func (e *testEnv) close() {
	// We use a synchronous cancel by replacing ctx with one we
	// control. Simpler: the cleanup will run on test end, but
	// we can mimic its effect by calling our own cancel.
	// Because the cancel is captured in the cleanup closure
	// defined in newTestEnv/newTestEnvWithDir, we need access
	// to it. Store it on the env.
	e.cancel()
	// Best-effort wait for the goroutine that called Start to
	// return. We do not have a handle to it, so a tiny sleep
	// is the best we can do without exposing internals.
	time.Sleep(50 * time.Millisecond)
}

// newTestEnvWithDir is like newTestEnv but uses an explicit directory
// instead of t.TempDir(). The caller owns the directory's lifetime;
// the test must not call t.TempDir cleanup on it.
func newTestEnvWithDir(t *testing.T, dir string) *testEnv {
	t.Helper()
	return newTestEnvAt(t, dir, true /* ownsDir=false; t.TempDir manages cleanup */)
}

// newTestEnv creates the env in a fresh t.TempDir().
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	return newTestEnvAt(t, dir, true)
}

// newTestEnvAt is the inner constructor. ownsDir is a hint left over
// from an earlier refactor; the new model is "the caller always
// owns the directory, and the test framework handles cleanup".
func newTestEnvAt(t *testing.T, dir string, _ bool) *testEnv {
	t.Helper()
	dbPath := filepath.Join(dir, "data.db")

	// Pick a free port eagerly. We bind, get the port, then close;
	// the small race window between close and server.Listen is
	// acceptable in tests, and avoids cross-test port collisions
	// when the OS reuses ports.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := server.Config{
		Host:        "127.0.0.1",
		Port:        portFromAddr(addr),
		ReadHeader:  5 * time.Second,
		IdleTimeout: 120 * time.Second,
		Logger:      logger,
	}

	db, cleanup, err := storage.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	migrations := os.DirFS(filepath.Join(repoRoot(t), "migrations"))
	if _, err := storage.Migrate(context.Background(), db, migrations, nil); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}

	srv, err := server.New(cfg, server.Deps{
		DB:     db,
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})

	go func() {
		if err := srv.Start(ctx); err != nil && !isShutdownErr(err) {
			t.Logf("server.Start returned: %v", err)
		}
	}()

	env := &testEnv{
		t:          t,
		dir:        dir,
		dbPath:     dbPath,
		cfg:        cfg,
		server:     srv,
		addr:       addr,
		baseURL:    "http://" + addr,
		cancel:     cancel,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	if err := env.waitReady(5 * time.Second); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	env.keyA = env.createKey("key-a")
	env.keyB = env.createKey("key-b")

	return env
}

// spawnServe returns an exec.Cmd for `conclave-mcp serve` bound to
// the given address. The caller is responsible for Start, Wait, and
// process cleanup.
//
// The serve subcommand reads its config from flags, not from env
// vars (see internal/config/config.go: only --host/--port/--db are
// parsed). We pass those flags directly.
func spawnServe(ctx context.Context, bin, addr, dataDir string) (*exec.Cmd, func()) {
	port := portFromAddr(addr)
	cmd := exec.CommandContext(ctx, bin, "serve",
		"--host", "127.0.0.1",
		fmt.Sprintf("--port=%d", port),
		"--db", filepath.Join(dataDir, "data.db"),
	)
	cmd.Dir = dataDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}
	}
	return cmd, cleanup
}

// _ keeps the import of sync in case future helpers need a WaitGroup.
var _ = sync.WaitGroup{}

// syncAddInt64 is the indirection that makes the import stable
// without depending on a private function in the standard library.
func syncAddInt64(p *int64, delta int64) int64 {
	old := *p
	*p = old + delta
	return old
}
