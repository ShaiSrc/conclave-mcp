package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hermes/conclave-mcp/internal/agent"
)

func TestSessionStart_OperatorMessageIsNotedBeforeTheBody(t *testing.T) {
	s := newSetup(t)
	s.f.msgs = []agent.Msg{{ID: 2, Author: 9, TS: "t1", Operator: true, Content: "stop"}}
	out := s.run(map[string]any{"hook_event_name": "SessionStart"})
	var o struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatal(err)
	}
	ctx := o.HookSpecificOutput.AdditionalContext
	if strings.Count(ctx, agent.OperatorNote) != 1 || strings.Index(ctx, agent.OperatorNote) > strings.Index(ctx, "- OPERATOR (confirmed with passkey)") {
		t.Errorf("want OperatorNote once, before the body:\n%s", ctx)
	}
}
