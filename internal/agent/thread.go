package agent

import (
	"fmt"
	"strings"
)

// Thread returns the conversation id belongs to, oldest first: the root
// (found by following reply_to up) and every message that replies to
// it, directly or through other replies. nil if id is not in msgs.
func Thread(msgs []Msg, id int64) []Msg {
	parent := make(map[int64]int64, len(msgs))
	for _, m := range msgs {
		parent[m.ID] = ReplyTo(m)
	}
	if _, ok := parent[id]; !ok {
		return nil
	}
	root := func(x int64) int64 {
		// reply_to always points at an older message, so this ends;
		// the bound guards against a corrupt chain.
		for range msgs {
			p := parent[x]
			if _, ok := parent[p]; !ok { // no parent, or it was deleted
				return x
			}
			x = p
		}
		return x
	}
	top := root(id)
	var out []Msg
	for _, m := range msgs {
		if root(m.ID) == top {
			out = append(out, m)
		}
	}
	return out
}

// FormatThread renders a thread for an agent, with full content and
// the reader's own messages marked "(you)".
func FormatThread(room string, msgs []Msg, labels map[int64]string, self int64) string {
	var b strings.Builder
	writeNote(&b, msgs)
	fmt.Fprintf(&b, "Conclave room %s, thread #%d: %d message(s). %s\n", room, msgs[0].ID, len(msgs), dataSentence(msgs))
	for _, m := range msgs {
		writeMsg(&b, m, labels, self, 0)
	}
	return b.String()
}

// Unanswered returns the requests in msgs (questions, tasks, handoffs,
// alerts, decisions) that no other key has replied to with reply_to.
func Unanswered(msgs []Msg) []Msg {
	answered := map[int64]bool{}
	author := make(map[int64]int64, len(msgs))
	for _, m := range msgs {
		author[m.ID] = m.Author
	}
	for _, m := range msgs {
		if to := ReplyTo(m); to > 0 && author[to] != m.Author {
			answered[to] = true
		}
	}
	var out []Msg
	for _, m := range msgs {
		if Actionable(m) && !answered[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// Line renders one message like the inbox does, with full content and
// the reader's own messages marked "(you)".
func Line(m Msg, labels map[int64]string, self int64) string {
	var b strings.Builder
	writeMsg(&b, m, labels, self, 0)
	return b.String()
}
