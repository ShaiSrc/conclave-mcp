// Package hook implements `conclave-mcp hook`, one Claude Code hook
// command for every event. It keeps agents on one repository in step:
// new messages go into the context, edits of paths another agent holds
// are denied, open questions are answered before stopping, and claims
// are freed at session end. See docs/agent-integration.md.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hermes/conclave-mcp/internal/agent"
	"github.com/hermes/conclave-mcp/internal/claims"
	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// Error is a hook failure. main exits 1 for it, never 2: Claude Code
// treats exit 2 as "block" (drop the prompt, refuse the tool call).
type Error struct{ Err error }

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// NewCmd builds `conclave-mcp hook [--wake DURATION]`.
func NewCmd() *cobra.Command {
	var flagWake time.Duration
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Claude Code hook for every event (inbox, claims, Stop, SessionEnd)",
		Long: `Reads Claude Code hook JSON on stdin and acts on hook_event_name:

  SessionStart, UserPromptSubmit, PostToolUse
      new messages from other agents as additionalContext; keeps this
      agent's claims alive
  PreToolUse (Edit, Write, MultiEdit, NotebookEdit)
      claims the file, or denies the edit if another agent holds it
  Stop        has the agent handle open questions first (once)
  SessionEnd  releases this agent's claims and posts a handoff

With --wake DURATION (an asyncRewake hook on SessionStart and Stop) it
waits in the background for a question, task, handoff, alert, decision
or answer from another agent, prints the new messages on stderr and
exits 2, which wakes an idle Claude session. The newest waiter of an
agent wins; SessionEnd stops it. Status updates alone do not wake.

The room is the repository's (.conclave.json, see conclave-mcp init).
The key comes from --token, --as, CONCLAVE_API_KEY or CONCLAVE_AGENT
(a key stored by agent add --store). Without any it does nothing, so a committed
.claude/settings.json does not bother teammates who do not use
Conclave. Failures exit 1 (non-blocking); edits are allowed while
the server is unreachable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flagToken, _ := cmd.Flags().GetString("token")
			flagAs, _ := cmd.Flags().GetString("as")
			token, err := client.ResolveToken(flagToken, flagAs)
			if err != nil {
				return &Error{err}
			}
			if token == "" {
				return nil
			}
			server, _ := cmd.Flags().GetString("server")
			c, err := client.New(client.Options{Server: server, Token: token})
			if err != nil {
				return &Error{err}
			}
			h := &Hook{C: c, Token: token, Now: time.Now, ReleaseOnStop: os.Getenv("CONCLAVE_RELEASE_ON_STOP") == "1"}
			in := agent.ReadHookInput(cmd.InOrStdin())
			if flagWake > 0 {
				err := h.Wake(cmd.Context(), in, flagWake, cmd.ErrOrStderr())
				if err != nil && !errors.Is(err, ErrWake) {
					return &Error{err}
				}
				return err
			}
			if err := h.Run(cmd.Context(), in, cmd.OutOrStdout()); err != nil {
				return &Error{err}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&flagWake, "wake", 0, "wait up to this long for a message that needs this agent, then exit 2 (asyncRewake hook)")
	return cmd
}

// Hook handles one hook invocation.
type Hook struct {
	C        *client.Client
	Token    string
	Now      func() time.Time
	StateDir string // "" = user cache dir; tests use a temp dir

	// ReleaseOnStop (CONCLAVE_RELEASE_ON_STOP=1) releases the claims
	// when the turn ends. For headless runs (`claude -p`), which never
	// fire SessionEnd; interactive sessions must not set it, Stop
	// fires after every turn there.
	ReleaseOnStop bool
}

// statePath is this agent's cursor and claim state for repo.
func (h *Hook) statePath(repo agent.Repo) string {
	sum := sha256.Sum256([]byte(repo.Root))
	return agent.StatePath(h.StateDir, h.Token, "repo-"+hex.EncodeToString(sum[:6]))
}

// ErrWake means `hook --wake` printed messages for the agent; main
// exits 2, which makes Claude Code wake the (possibly idle) session.
var ErrWake = errors.New("conclave: new messages for the agent")

const wakeSuffix = ".wake"

// wakePoll is how often a waiter checks that it is still the newest.
var wakePoll = 2 * time.Second

// Wake runs as an asyncRewake hook (SessionStart, Stop). It waits up
// to wait for a message that asks something of this agent or answers
// it, writes the new messages to w and returns ErrWake. Status
// updates alone do not wake it and stay unread. The newest waiter of
// an agent wins: starting one, or SessionEnd, makes older ones exit
// quietly, so each Stop can re-arm without piling up processes.
func (h *Hook) Wake(ctx context.Context, in agent.HookInput, wait time.Duration, w io.Writer) error {
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	repo, err := agent.FindRepo(cwd)
	if err != nil {
		return nil
	}
	statePath := h.statePath(repo)
	st := agent.LoadState(statePath)
	if st.RoomID == "" {
		if st.RoomID, err = agent.ResolveRoom(ctx, h.C, repo.RoomName()); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release, err := newestWaiter(statePath+wakeSuffix, in.SessionID, cancel)
	if err != nil {
		return err
	}
	defer release()
	news, err := agent.WaitFor(ctx, h.C, st.RoomID, statePath, wait, agent.Wakes)
	if err != nil || len(news) == 0 {
		if ctx.Err() != nil {
			return nil // superseded or stopped
		}
		return err
	}
	fmt.Fprint(w, agent.Format(h.roomLabel(repo, &st), news, agent.Labels(ctx, h.C, st.RoomID))+
		"Handle what is addressed to you: answer with messages.send and reply_to = the message's #id.\n")
	return ErrWake
}

// newestWaiter records this process as the agent's waiter in p and
// cancels when another one takes over (or p is removed). The marker
// starts with the session id, so SessionEnd stops only its own
// session's waiter. release removes p if it is still ours. Writes and
// removals of p hold p's lock, so an exiting waiter cannot delete a
// newer one's marker.
func newestWaiter(p, session string, cancel func()) (release func(), err error) {
	id := session + " " + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// LockState also creates the state dir: the wake hook can be the
	// first hook ever run for this agent.
	unlock, err := agent.LockState(p)
	if err != nil {
		return nil, err
	}
	err = os.WriteFile(p, []byte(id), 0o600)
	unlock()
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(wakePoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if b, _ := os.ReadFile(p); string(b) != id {
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		unlock, err := agent.LockState(p)
		if err != nil {
			return
		}
		defer unlock()
		if b, _ := os.ReadFile(p); string(b) == id {
			_ = os.Remove(p)
		}
	}, nil
}

// stopWaiter makes session's waiter exit; a newer session's waiter
// (same agent, same repository) keeps running.
func stopWaiter(p, session string) {
	unlock, err := agent.LockState(p)
	if err != nil {
		return
	}
	defer unlock()
	if b, _ := os.ReadFile(p); strings.HasPrefix(string(b), session+" ") {
		_ = os.Remove(p)
	}
}

// Run dispatches on in.Event and writes the hook's JSON answer to out.
func (h *Hook) Run(ctx context.Context, in agent.HookInput, out io.Writer) error {
	if in.Event == "" {
		return errors.New("no hook JSON on stdin; run this from a Claude Code hook")
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	repo, err := agent.FindRepo(cwd)
	if err != nil {
		return nil // not in a git repository: nothing to coordinate
	}
	statePath := h.statePath(repo)
	unlock, err := agent.LockState(statePath)
	if err != nil {
		return err
	}
	defer unlock()
	loaded := agent.LoadState(statePath)
	st := loaded
	if st.RoomID == "" {
		if st.RoomID, err = agent.ResolveRoom(ctx, h.C, repo.RoomName()); err != nil {
			return err
		}
	}
	switch in.Event {
	case "SessionStart":
		err = h.sessionStart(ctx, repo, &st, out)
	case "UserPromptSubmit", "PostToolUse":
		err = h.inbox(ctx, in.Event, repo, &st, out)
	case "PreToolUse":
		err = h.preToolUse(ctx, in, cwd, repo, &st, out)
	case "Stop":
		err = h.stop(ctx, in, repo, &st, out)
	case "SessionEnd":
		err = h.sessionEnd(ctx, &st)
		stopWaiter(statePath+wakeSuffix, in.SessionID)
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		st.RoomID = "" // room gone or re-created: resolve it again next time
	}
	if st != loaded {
		if serr := agent.SaveState(statePath, st); err == nil {
			err = serr
		}
	}
	return err
}

func (h *Hook) roomLabel(repo agent.Repo, st *agent.State) string {
	return repo.RoomName() + " (room_id " + st.RoomID + ")"
}

// sessionStart always tells the agent who it is, which room to use
// and what is claimed, so it never has to look any of it up.
func (h *Hook) sessionStart(ctx context.Context, repo agent.Repo, st *agent.State, out io.Writer) error {
	h.heartbeat(ctx, repo, st)
	var me struct {
		ID    int64  `json:"id"`
		Label string `json:"label"`
	}
	if _, err := h.C.Do(ctx, client.Request{Method: "GET", Path: "/v1/whoami", Out: &me}); err != nil {
		return err
	}
	st.SelfID = me.ID
	var held struct {
		Claims []claims.Claim `json:"claims"`
	}
	if _, err := h.C.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms/" + st.RoomID + "/claims", Out: &held}); err != nil {
		return err
	}
	msgs, err := agent.FetchNew(ctx, h.C, st.RoomID, st)
	if err != nil {
		return err
	}
	var b strings.Builder
	if agent.HasExternal(msgs) || slices.ContainsFunc(held.Claims, func(c claims.Claim) bool { return c.TrustOrigin == "external" }) {
		b.WriteString(agent.ExternalNote + "\n")
	}
	if agent.HasOperator(msgs) {
		b.WriteString(agent.OperatorNote + "\n")
	}
	fmt.Fprintf(&b, "Conclave: you are %s in room %s (room_id %s); use this room_id with the conclave tools.\n",
		me.Label, repo.RoomName(), st.RoomID)
	if len(held.Claims) == 0 {
		b.WriteString("No active claims.\n")
	} else {
		b.WriteString("Active claims (do not edit paths others hold):\n")
		for _, c := range held.Claims {
			fmt.Fprintf(&b, "- %s: %s", agent.OneLine(c.Path), agent.OneLine(c.Label))
			if c.TrustOrigin == "external" {
				b.WriteString(" (EXTERNAL, untrusted)")
			}
			fmt.Fprintf(&b, " until %s", c.ExpiresAt)
			if c.Note != "" {
				fmt.Fprintf(&b, " (%s)", agent.OneLine(c.Note))
			}
			b.WriteString("\n")
		}
	}
	if len(msgs) > 0 {
		b.WriteString(agent.FormatBody(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C, st.RoomID)))
	}
	return json.NewEncoder(out).Encode(agent.ContextOutput("SessionStart", b.String()))
}

func (h *Hook) inbox(ctx context.Context, event string, repo agent.Repo, st *agent.State, out io.Writer) error {
	h.heartbeat(ctx, repo, st)
	msgs, err := agent.FetchNew(ctx, h.C, st.RoomID, st)
	if err != nil || len(msgs) == 0 {
		return err
	}
	return json.NewEncoder(out).Encode(agent.ContextOutput(event,
		agent.Format(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C, st.RoomID))))
}

// heartbeat keeps this agent's claims alive while it works; best
// effort, at most every min(5 min, ttl/3).
func (h *Hook) heartbeat(ctx context.Context, repo agent.Repo, st *agent.State) {
	now := h.Now()
	if now.Sub(time.Unix(st.Heartbeat, 0)) < min(5*time.Minute, repo.TTL()/3) {
		return
	}
	if _, err := h.C.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + st.RoomID + "/claims/heartbeat",
		Body: map[string]any{"ttl_seconds": int(repo.TTL().Seconds())}}); err == nil {
		st.Heartbeat = now.Unix()
	}
}

func (h *Hook) preToolUse(ctx context.Context, in agent.HookInput, cwd string, repo agent.Repo, st *agent.State, out io.Writer) error {
	p := in.ToolInput.FilePath
	if p == "" {
		p = in.ToolInput.NotebookPath
	}
	rel, ok := repo.Rel(cwd, p)
	if p == "" || !ok {
		return nil
	}
	ask := "Ask the holder in the Conclave room (messages.send, message_type question, room_id " + st.RoomID +
		") or work on something else. Do not edit it through Bash."
	if repo.IsShared(rel) {
		// Shared files are never auto-claimed, but an explicit claim
		// by another agent still holds.
		if err := agent.EnsureSelf(ctx, h.C, st); err != nil {
			return err
		}
		var resp struct {
			Claims []claims.Claim `json:"claims"`
		}
		if _, err := h.C.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms/" + st.RoomID + "/claims", Out: &resp}); err != nil {
			return err
		}
		for _, c := range resp.Claims {
			if c.KeyID != st.SelfID && !c.Auto && claims.Overlaps(c.Path, rel) {
				who := agent.OneLine(c.Label)
				if c.TrustOrigin == "external" {
					who += " (EXTERNAL, untrusted)"
				}
				return deny(out, fmt.Sprintf("%s is claimed by %s (claim %s, until %s). %s", rel, who, agent.OneLine(c.Path), c.ExpiresAt, ask))
			}
		}
		return nil
	}
	_, err := h.C.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + st.RoomID + "/claims",
		Body: map[string]any{"paths": []string{rel}, "auto": true, "ttl_seconds": int(repo.TTL().Seconds())}})
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return deny(out, agent.OneLine(apiErr.Message)+". "+ask)
	}
	if err == nil {
		st.Heartbeat = h.Now().Unix() // a claim refreshes all own claims
	}
	return err // other errors fail open: main exits 1 and the edit proceeds
}

func deny(out io.Writer, reason string) error {
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason,
	}})
}

// stop blocks the end of the turn once when other agents asked
// something; plain updates wait for the next inbox read.
func (h *Hook) stop(ctx context.Context, in agent.HookInput, repo agent.Repo, st *agent.State, out io.Writer) error {
	if in.StopHookActive {
		return h.releaseOnStop(ctx, st)
	}
	next := *st
	msgs, err := agent.FetchNew(ctx, h.C, st.RoomID, &next)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(msgs, agent.Actionable) {
		return h.releaseOnStop(ctx, st) // cursor untouched: the messages stay for the next inbox read
	}
	*st = next
	return json.NewEncoder(out).Encode(map[string]any{
		"decision": "block",
		"reason": agent.Format(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C, st.RoomID)) +
			"Before you stop: answer what is addressed to you in the Conclave room (messages.send with reply_to = the message's #id), or say briefly why no answer is needed. Then stop.",
	})
}

func (h *Hook) releaseOnStop(ctx context.Context, st *agent.State) error {
	if !h.ReleaseOnStop {
		return nil
	}
	return h.sessionEnd(ctx, st)
}

func (h *Hook) sessionEnd(ctx context.Context, st *agent.State) error {
	var resp struct {
		Released []string `json:"released"`
	}
	if _, err := h.C.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + st.RoomID + "/claims/release",
		Body: map[string]any{}, Out: &resp}); err != nil {
		return err
	}
	st.Heartbeat = 0
	if len(resp.Released) == 0 {
		return nil
	}
	_, err := h.C.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + st.RoomID + "/messages", Body: map[string]any{
		"content":      "Session ended; released claims: " + strings.Join(resp.Released, ", "),
		"message_type": "handoff",
		"metadata":     map[string]any{"released": resp.Released},
	}})
	return err
}
