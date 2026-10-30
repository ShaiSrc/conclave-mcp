// Load tests (SPEC-0007 §6.2, Vikunja #36).
//
// These tests are skipped under `go test -short`. They spawn
// real `conclave-mcp serve` subprocesses via the helpers in
// helpers_test.go and put them under realistic load to exercise
// the rate limiter, message-size guard, and concurrent-verify
// cap. The 600 rpm rate limit (SPEC-0003 §6) is only effective
// after the ADR-0006 wiring landed in commit c7d4ad2; without
// that, the hardcoded 60 rpm would 429 every test in this file.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoad_RateLimitEnforced: sends concurrent POSTs to
// /v1/rooms with a single API key. Per SPEC-0007 §6.2 the
// rate limiter is 600 rpm with a burst of 30, so a sustained
// 200 req/s load must produce SOME 429s and at most
// burst+refill successes. The test asserts:
//   - At least 5% of the requests return 429
//   - At least the burst amount (30) succeed
//   - Every request lands in exactly one of four buckets
//     (created, 429, timeout, other) and the buckets sum
//     to the total count, with no silent drops.
//
// The number of in-flight requests is bounded so a single
// HTTP client + a single-threaded server can finish them all
// within the 5s per-request timeout. The previous version of
// this test (700 parallel) hit the per-request timeout for
// some requests on slower CI runners, and the resulting
// "context deadline exceeded" errors were followed by a
// `signal: killed` from the test runner before all goroutines
// had accounted for their result. With 200 requests and the
// same 5ms sleep between launches, the test completes in
// ~1s, well inside the 5s timeout, and every goroutine
// gets to record its bucket.
//
// The count is also low enough that the test stays reliable
// under `go test -race`, where the race detector slows the
// server and SQLite by 5-10x. A 200-request run finishes in
// under 5s even with -race.
func TestLoad_RateLimitEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short mode")
	}
	env, key := bootstrapKeyFor(t)
	auth := key.Plaintext

	const totalRequests = 200

	created := atomic.Int64{}
	tooMany := atomic.Int64{}
	timeouts := atomic.Int64{}
	other := atomic.Int64{}
	deadline := time.Now().Add(60 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if time.Now().After(deadline) {
				other.Add(1) // count as "did not even start"
				return
			}
			name := fmt.Sprintf("load-%d-%d", time.Now().UnixNano(), i)
			resp := env.call("POST", "/v1/rooms", auth, map[string]any{"name": name})
			switch {
			case resp.Err != nil:
				timeouts.Add(1)
			case resp.Status == 200 || resp.Status == 201:
				created.Add(1)
			case resp.Status == 429:
				tooMany.Add(1)
			default:
				other.Add(1)
			}
		}(i)
		time.Sleep(5 * time.Millisecond) // ~200/s, well under 600 rpm refill
	}
	wg.Wait()

	completed := created.Load() + tooMany.Load() + timeouts.Load() + other.Load()
	if completed != totalRequests {
		t.Errorf("internal counter bug: %d requests, only %d accounted for (created=%d, 429=%d, timeout=%d, other=%d)",
			totalRequests, completed, created.Load(), tooMany.Load(), timeouts.Load(), other.Load())
	}
	if tooMany.Load() < totalRequests*5/100 {
		t.Errorf("expected >= 5%% 429s, got %d of %d", tooMany.Load(), completed)
	}
	if created.Load() < 30 {
		t.Errorf("expected at least 30 successes (the burst), got %d", created.Load())
	}
	if created.Load() > totalRequests {
		t.Errorf("more successes than requests: %d > %d", created.Load(), totalRequests)
	}
	t.Logf("load test: %d created, %d rate-limited, %d timeout, %d other, %d total",
		created.Load(), tooMany.Load(), timeouts.Load(), other.Load(), completed)
}

// TestLoad_MessageSizeGuard: 50 sends alternating 256 KiB
// (within limit) and 257 KiB (over). Expect 25 within-limit
// sends to succeed and 25 over-limit sends to return 413.
func TestLoad_MessageSizeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short mode")
	}
	env, key := bootstrapKeyFor(t)
	auth := key.Plaintext

	create := env.call("POST", "/v1/rooms", auth, map[string]any{
		"name": "size-guard-" + fmt.Sprint(time.Now().UnixNano()),
	})
	if create.Status != 200 && create.Status != 201 {
		t.Fatalf("create: %d %s", create.Status, create.Body)
	}
	var room struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(create.Body, &room)

	within := bytes.Repeat([]byte("a"), 256*1024)
	over := bytes.Repeat([]byte("b"), 257*1024)

	var withinOK, over413 int
	for i := 0; i < 50; i++ {
		var content []byte
		if i%2 == 0 {
			content = within
		} else {
			content = over
		}
		resp := env.call("POST", "/v1/rooms/"+room.ID+"/messages", auth, map[string]any{
			"content": string(content),
		})
		if i%2 == 0 {
			if resp.Status == 200 || resp.Status == 201 {
				withinOK++
			}
		} else {
			if resp.Status == 413 {
				over413++
			}
		}
		// Pace to keep the test under 600 rpm refill (10/s). 50ms
		// only worked while every request paid a full argon2id.
		time.Sleep(110 * time.Millisecond)
	}

	if withinOK != 25 {
		t.Errorf("expected 25 within-limit sends to succeed, got %d", withinOK)
	}
	if over413 != 25 {
		t.Errorf("expected 25 over-limit sends to return 413, got %d", over413)
	}
}

// TestLoad_ConcurrentVerify: parallel POST /v1/rooms/...
// /messages with many distinct keys. Expect the test to not
// hang (the argon2id verify pool is sound) and at least 90%
// of sends to complete.
//
// We use 50 keys, not 200, because 200 concurrent key-creates
// from one master key exhaust the master's 600-rpm budget
// before the second phase even starts. 50 keys exercise the
// same code path (parallel argon2id verify) and complete
// within the test's wall-clock budget.
func TestLoad_ConcurrentVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short mode")
	}
	env, master := bootstrapKeyFor(t)
	const n = 50

	// Create n distinct keys via the HTTP path. We pace the
	// creates to stay within the master's rate limit.
	tokens := make([]string, n)
	var keyOK atomic.Int64
	for i := 0; i < n; i++ {
		resp := env.call("POST", "/v1/keys", master.Plaintext, map[string]any{
			"label": fmt.Sprintf("load-conc-%d-%d", time.Now().UnixNano(), i),
		})
		if resp.Status == 200 || resp.Status == 201 {
			var k struct {
				Key string `json:"key"`
			}
			_ = json.Unmarshal(resp.Body, &k)
			tokens[i] = k.Key
			keyOK.Add(1)
		}
		// Below the 10/s refill, so the master still has budget
		// for the room create below. 20ms only worked while every
		// request paid a full argon2id.
		time.Sleep(110 * time.Millisecond)
	}
	if keyOK.Load() < n*90/100 {
		t.Fatalf("only %d of %d key creates succeeded", keyOK.Load(), n)
	}

	// Create one room with the master.
	create := env.call("POST", "/v1/rooms", master.Plaintext, map[string]any{
		"name": "conc-room-" + fmt.Sprint(time.Now().UnixNano()),
	})
	if create.Status != 200 && create.Status != 201 {
		t.Fatalf("create room: %d %s", create.Status, create.Body)
	}
	var room struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(create.Body, &room)

	// n concurrent sends, one per token. Most will be 403
	// (non-members) but the verify-pool property is what we
	// care about: the test must not hang and >= 90% of sends
	// must complete (any status).
	var completed atomic.Int64
	deadline := time.Now().Add(30 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if time.Now().After(deadline) {
				return
			}
			resp := env.call("POST", "/v1/rooms/"+room.ID+"/messages", tokens[i], map[string]any{
				"content": fmt.Sprintf("concurrent-msg-%d", i),
			})
			if resp.Status > 0 {
				completed.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if completed.Load() < n*90/100 {
		t.Errorf("only %d of %d concurrent sends completed (argon2id verify pool may be broken)",
			completed.Load(), n)
	}
	t.Logf("concurrent verify: %d completed of %d", completed.Load(), n)
}
