package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/claims"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// fake is a minimal Conclave server: we are key 1, agent-b is key 2,
// the repo's room is R ("acme/widgets").
type fake struct {
	mu        sync.Mutex
	claims    []claims.Claim
	msgs      []agent.Msg
	claimReqs []map[string]any
	posted    []map[string]any
	released  bool
}

// add appends a message from another agent; open event streams see it.
func (f *fake) add(m agent.Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
}

// events streams the messages added after the subscription (SSE).
func (f *fake) events(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := len(f.msgs)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, ": ok\n\n")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		f.mu.Lock()
		var next []agent.Msg
		if len(f.msgs) > n {
			next, n = append(next, f.msgs[n:]...), len(f.msgs)
		}
		f.mu.Unlock()
		for _, m := range next {
			b, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: message.created\ndata: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
	}
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/rooms/R/events" {
		f.events(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/whoami":
		write(map[string]any{"id": 1, "label": "agent-a"})
	case "GET /v1/rooms":
		write(map[string]any{"rooms": []any{map[string]any{"id": "R", "name": "acme/widgets"}}})
	case "GET /v1/rooms/R/members":
		write(map[string]any{"members": []any{map[string]any{"key_id": 2, "label": "agent-b"}}})
	case "GET /v1/rooms/R/messages":
		since := r.URL.Query().Get("since")
		out := []agent.Msg{}
		for _, m := range f.msgs {
			if m.TS > since {
				out = append(out, m)
			}
		}
		write(map[string]any{"messages": out})
	case "GET /v1/rooms/R/claims":
		write(map[string]any{"claims": f.claims})
	case "POST /v1/rooms/R/claims":
		f.claimReqs = append(f.claimReqs, body)
		for _, p := range body["paths"].([]any) {
			for _, c := range f.claims {
				if claims.Overlaps(p.(string), c.Path) {
					w.WriteHeader(http.StatusConflict)
					write(map[string]any{"error": map[string]any{"code": "conflict", "message": p.(string) + " is claimed by " + c.Label}})
					return
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
		write(map[string]any{"claims": []any{}})
	case "POST /v1/rooms/R/claims/heartbeat":
		write(map[string]any{"refreshed": 0})
	case "POST /v1/rooms/R/claims/release":
		f.released = true
		write(map[string]any{"released": []string{"src/a.go"}})
	case "POST /v1/rooms/R/messages":
		f.posted = append(f.posted, body)
		w.WriteHeader(http.StatusCreated)
		write(map[string]any{"id": 9})
	default:
		http.NotFound(w, r)
	}
}

type setup struct {
	t    *testing.T
	f    *fake
	repo string
	h    *Hook
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "remote", "add", "origin", "git@github.com:acme/widgets.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	f := &fake{}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	token := "tk_testtest0000000000000000000000000000000000"
	c, err := client.New(client.Options{Server: api.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return &setup{t: t, f: f, repo: repo, h: &Hook{C: c, Token: token, Now: time.Now, StateDir: t.TempDir()}}
}

// run feeds one hook event and returns the hook's stdout.
func (s *setup) run(input map[string]any) string {
	s.t.Helper()
	input["cwd"] = s.repo
	b, _ := json.Marshal(input)
	var out bytes.Buffer
	if err := s.h.Run(context.Background(), agent.ReadHookInput(bytes.NewReader(b)), &out); err != nil {
		s.t.Fatalf("Run(%s): %v", b, err)
	}
	return out.String()
}

func edit(path string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Edit", "tool_input": map[string]any{"file_path": path}}
}

func TestPreToolUse_DeniesPathHeldByOther(t *testing.T) {
	s := newSetup(t)
	s.f.claims = []claims.Claim{{Path: "src/", KeyID: 2, Label: "agent-b"}}
	out := s.run(edit(filepath.Join(s.repo, "src", "a.go")))
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "agent-b") {
		t.Errorf("want deny naming agent-b, got %s", out)
	}
}

func TestPreToolUse_AutoClaimsFreePath(t *testing.T) {
	s := newSetup(t)
	if out := s.run(edit(filepath.Join(s.repo, "lib", "x.go"))); out != "" {
		t.Errorf("free path must be allowed silently, got %s", out)
	}
	if len(s.f.claimReqs) != 1 || s.f.claimReqs[0]["auto"] != true || s.f.claimReqs[0]["paths"].([]any)[0] != "lib/x.go" {
		t.Errorf("want one auto claim of lib/x.go, got %v", s.f.claimReqs)
	}
}

func TestPreToolUse_SharedAndOutside(t *testing.T) {
	s := newSetup(t)
	if out := s.run(edit(filepath.Join(s.repo, "go.mod"))); out != "" || len(s.f.claimReqs) != 0 {
		t.Errorf("shared file: must not auto-claim (out %q, claims %v)", out, s.f.claimReqs)
	}
	if out := s.run(edit("/etc/hosts")); out != "" || len(s.f.claimReqs) != 0 {
		t.Errorf("outside the repo: must do nothing (out %q)", out)
	}
	s.f.claims = []claims.Claim{{Path: "go.mod", KeyID: 2, Label: "agent-b"}} // explicit claim
	if out := s.run(edit(filepath.Join(s.repo, "go.mod"))); !strings.Contains(out, "deny") {
		t.Errorf("explicitly claimed shared file must be denied, got %q", out)
	}
}

func TestInbox_InjectsOnceThenQuiet(t *testing.T) {
	s := newSetup(t)
	s.f.msgs = []agent.Msg{{ID: 1, Author: 2, TS: "t1", Content: "I take src/auth"}}
	out := s.run(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"})
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "I take src/auth") || !strings.Contains(out, "room_id R") {
		t.Errorf("want the message as additionalContext with the room id, got %s", out)
	}
	if out := s.run(map[string]any{"hook_event_name": "UserPromptSubmit"}); out != "" {
		t.Errorf("nothing new: got %s", out)
	}
}

func TestStop_BlocksOnceForActionable(t *testing.T) {
	s := newSetup(t)
	s.f.msgs = []agent.Msg{{ID: 1, Author: 2, TS: "t1", MessageType: "status_update", Content: "fyi"}}
	if out := s.run(map[string]any{"hook_event_name": "Stop"}); out != "" {
		t.Errorf("status_update alone must not block, got %s", out)
	}
	s.f.msgs = append(s.f.msgs, agent.Msg{ID: 2, Author: 2, TS: "t2", MessageType: "question", Content: "which port?"})
	out := s.run(map[string]any{"hook_event_name": "Stop"})
	if !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "which port?") || !strings.Contains(out, "fyi") {
		t.Errorf("want a block with both messages, got %s", out)
	}
	s.f.msgs = append(s.f.msgs, agent.Msg{ID: 3, Author: 2, TS: "t3", MessageType: "question", Content: "again?"})
	if out := s.run(map[string]any{"hook_event_name": "Stop", "stop_hook_active": true}); out != "" {
		t.Errorf("stop_hook_active must never block again, got %s", out)
	}
}

func TestSessionEnd_ReleasesAndPostsHandoff(t *testing.T) {
	s := newSetup(t)
	s.run(map[string]any{"hook_event_name": "SessionEnd"})
	if !s.f.released || len(s.f.posted) != 1 || s.f.posted[0]["message_type"] != "handoff" ||
		!strings.Contains(s.f.posted[0]["content"].(string), "src/a.go") {
		t.Errorf("want release + handoff naming src/a.go, got released=%v posted=%v", s.f.released, s.f.posted)
	}
}

func TestCmd_SilentWithoutKey(t *testing.T) {
	t.Setenv("CONCLAVE_API_KEY", "")
	t.Setenv("CONCLAVE_AGENT", "")
	cmd := NewCmd()
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("server", "", "")
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PreToolUse"}`))
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil || out.Len() != 0 {
		t.Errorf("no key: want silent success, got err=%v out=%q", err, out.String())
	}
}

func TestCmd_UnknownAgentIsANonBlockingError(t *testing.T) {
	// CONCLAVE_AGENT names an agent but no key is stored for it: say
	// so (exit 1, shown to the user) instead of silently doing nothing.
	t.Setenv("CONCLAVE_API_KEY", "")
	t.Setenv("CONCLAVE_AGENT", "no-such-agent-for-tests")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := NewCmd()
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("server", "", "")
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PreToolUse"}`))
	var hookErr *Error
	if err := cmd.RunE(cmd, nil); !errors.As(err, &hookErr) || !strings.Contains(err.Error(), "no-such-agent-for-tests") {
		t.Errorf("want a hook error naming the agent, got %v", err)
	}
}

func TestSessionStart_AlwaysTellsIdentityRoomAndClaims(t *testing.T) {
	s := newSetup(t)
	s.f.claims = []claims.Claim{{Path: "src/", KeyID: 2, Label: "agent-b", Note: "refactor", ExpiresAt: "2026-11-07T12:00:00Z"}}
	out := s.run(map[string]any{"hook_event_name": "SessionStart"})
	for _, want := range []string{"additionalContext", "you are agent-a", "room_id R", "src/: agent-b", "refactor"} {
		if !strings.Contains(out, want) {
			t.Errorf("SessionStart context lacks %q: %s", want, out)
		}
	}
}

func TestStop_ReleaseOnStopForHeadlessRuns(t *testing.T) {
	s := newSetup(t)
	s.run(map[string]any{"hook_event_name": "Stop"})
	if s.f.released {
		t.Fatal("without ReleaseOnStop, Stop must not release claims")
	}
	s.h.ReleaseOnStop = true
	if out := s.run(map[string]any{"hook_event_name": "Stop"}); out != "" {
		t.Errorf("nothing actionable: Stop must not block, got %s", out)
	}
	if !s.f.released || len(s.f.posted) != 1 || s.f.posted[0]["message_type"] != "handoff" {
		t.Errorf("ReleaseOnStop: want release + handoff, got released=%v posted=%v", s.f.released, s.f.posted)
	}
}

func TestInbox_ParallelHooksInjectOnce(t *testing.T) {
	s := newSetup(t)
	s.f.msgs = []agent.Msg{{ID: 1, Author: 2, TS: "t1", Content: "only once"}}
	in, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "cwd": s.repo})
	var (
		wg sync.WaitGroup
		mu sync.Mutex
		n  int
	)
	for range 8 { // parallel tool calls each fire PostToolUse
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if err := s.h.Run(context.Background(), agent.ReadHookInput(bytes.NewReader(in)), &out); err != nil {
				t.Error(err)
			}
			if strings.Contains(out.String(), "only once") {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n != 1 {
		t.Errorf("message injected %d times, want exactly 1", n)
	}
}

func (s *setup) wake(ctx context.Context, d time.Duration) (string, error) {
	return s.wakeIn(ctx, "s1", d)
}

func (s *setup) wakeIn(ctx context.Context, session string, d time.Duration) (string, error) {
	var errOut bytes.Buffer
	err := s.h.Wake(ctx, agent.HookInput{Event: "Stop", Cwd: s.repo, SessionID: session}, d, &errOut)
	return errOut.String(), err
}

func fastWakePoll(t *testing.T) {
	old := wakePoll
	wakePoll = 20 * time.Millisecond
	t.Cleanup(func() { wakePoll = old })
}

func TestWake_QuestionWakesStatusDoesNot(t *testing.T) {
	fastWakePoll(t)
	s := newSetup(t)
	s.run(map[string]any{"hook_event_name": "SessionStart"})
	s.f.add(agent.Msg{ID: 5, Author: 2, TS: "t5", MessageType: "status_update", Content: "fyi: tests green"})

	if out, err := s.wake(context.Background(), 300*time.Millisecond); err != nil || out != "" {
		t.Fatalf("status update only: want no wake, got %q, %v", out, err)
	}

	time.AfterFunc(200*time.Millisecond, func() {
		s.f.add(agent.Msg{ID: 6, Author: 2, TS: "t6", MessageType: "question", Content: "which file?"})
	})
	start := time.Now()
	out, err := s.wake(context.Background(), 10*time.Second)
	if !errors.Is(err, ErrWake) {
		t.Fatalf("question: want ErrWake, got %v (out %q)", err, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("woke after %v, want right after the question", time.Since(start))
	}
	for _, want := range []string{"fyi: tests green", "#6 agent-b <question>", "which file?", "reply_to"} {
		if !strings.Contains(out, want) {
			t.Errorf("wake text lacks %q:\n%s", want, out)
		}
	}
	if got := s.run(map[string]any{"hook_event_name": "UserPromptSubmit"}); got != "" {
		t.Errorf("delivered messages came again on the next prompt: %s", got)
	}
}

func TestWake_NewestWaiterAndSessionEndStopOlderOnes(t *testing.T) {
	fastWakePoll(t)
	s := newSetup(t)
	s.run(map[string]any{"hook_event_name": "SessionStart"})

	first := make(chan error, 1)
	go func() { _, err := s.wake(context.Background(), 30*time.Second); first <- err }()
	time.Sleep(200 * time.Millisecond)
	second := make(chan error, 1)
	go func() { _, err := s.wake(context.Background(), 30*time.Second); second <- err }()

	select {
	case err := <-first:
		if err != nil {
			t.Errorf("superseded waiter: want a quiet exit, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the older waiter kept running after a newer one started")
	}

	time.Sleep(200 * time.Millisecond)
	s.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": "s1"})
	select {
	case err := <-second:
		if err != nil {
			t.Errorf("waiter after SessionEnd: want a quiet exit, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SessionEnd did not stop the waiter")
	}
}

func TestWake_CreatesMissingStateDir(t *testing.T) {
	// The SessionStart wake hook can run before any other hook created
	// the cache dir.
	fastWakePoll(t)
	s := newSetup(t)
	s.h.StateDir = filepath.Join(t.TempDir(), "not", "yet")
	if out, err := s.wake(context.Background(), 200*time.Millisecond); err != nil || out != "" {
		t.Fatalf("fresh state dir: want a quiet timeout, got %q, %v", out, err)
	}
}

func TestWake_SessionEndStopsOnlyItsOwnSessionsWaiter(t *testing.T) {
	// Same agent, two sessions in one repo: the older session ending
	// must not stop the newer session's waiter.
	fastWakePoll(t)
	s := newSetup(t)
	s.run(map[string]any{"hook_event_name": "SessionStart"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.wakeIn(ctx, "new", 30*time.Second); done <- err }()
	time.Sleep(200 * time.Millisecond)

	s.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": "old"})
	select {
	case err := <-done:
		t.Fatalf("the old session's end stopped the new session's waiter (err %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	s.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": "new"})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the new session's end did not stop its waiter")
	}
}
