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

func NewCmd() *cobra.Command {
	return &cobra.Command{
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

The room is the repository's (.conclave.json, see conclave-mcp init).
Without CONCLAVE_API_KEY it does nothing, so a committed
.claude/settings.json does not bother teammates who do not use
Conclave. Failures exit 1 (non-blocking); edits are allowed while
the server is unreachable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, _ := cmd.Flags().GetString("token")
			if token == "" {
				token = os.Getenv("CONCLAVE_API_KEY")
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
			if err := h.Run(cmd.Context(), agent.ReadHookInput(cmd.InOrStdin()), cmd.OutOrStdout()); err != nil {
				return &Error{err}
			}
			return nil
		},
	}
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
	sum := sha256.Sum256([]byte(repo.Root))
	statePath := agent.StatePath(h.StateDir, h.Token, "repo-"+hex.EncodeToString(sum[:6]))
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
	var b strings.Builder
	fmt.Fprintf(&b, "Conclave: you are %s in room %s (room_id %s); use this room_id with the conclave tools.\n",
		me.Label, repo.RoomName(), st.RoomID)
	if len(held.Claims) == 0 {
		b.WriteString("No active claims.\n")
	} else {
		b.WriteString("Active claims (do not edit paths others hold):\n")
		for _, c := range held.Claims {
			fmt.Fprintf(&b, "- %s: %s until %s", c.Path, c.Label, c.ExpiresAt)
			if c.Note != "" {
				fmt.Fprintf(&b, " (%s)", c.Note)
			}
			b.WriteString("\n")
		}
	}
	msgs, err := agent.FetchNew(ctx, h.C, st.RoomID, st)
	if err != nil {
		return err
	}
	if len(msgs) > 0 {
		b.WriteString(agent.Format(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C)))
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
		agent.Format(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C))))
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
				return deny(out, fmt.Sprintf("%s is claimed by %s (claim %s, until %s). %s", rel, c.Label, c.Path, c.ExpiresAt, ask))
			}
		}
		return nil
	}
	_, err := h.C.Do(ctx, client.Request{Method: "POST", Path: "/v1/rooms/" + st.RoomID + "/claims",
		Body: map[string]any{"paths": []string{rel}, "auto": true, "ttl_seconds": int(repo.TTL().Seconds())}})
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return deny(out, apiErr.Message+". "+ask)
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
		"reason": agent.Format(h.roomLabel(repo, st), msgs, agent.Labels(ctx, h.C)) +
			"Before you stop: answer what is addressed to you in the Conclave room (messages.send), or say briefly why no answer is needed. Then stop.",
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
