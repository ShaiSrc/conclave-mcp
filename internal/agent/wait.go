package agent

import (
	"context"
	"time"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// Any accepts every batch of new messages.
func Any([]Msg) bool { return true }

// Wakes reports whether msgs ask something of the reader or answer
// it: worth waking an idle agent for. Plain status updates are not.
func Wakes(msgs []Msg) bool {
	for _, m := range msgs {
		if Actionable(m) || m.MessageType == "answer" {
			return true
		}
	}
	return false
}

// WaitFor returns the new messages from other keys once want accepts
// them, and only then advances the cursor in statePath; messages it
// does not want stay unread for the next reader. With wait <= 0 it
// reads once. Otherwise it waits up to wait (or until ctx ends) and
// returns nil when the time is up.
//
// The event stream is opened before each read, so nothing sent in
// between is missed, and the state lock is held only for the reads
// (an agent's hooks share the cursor file).
func WaitFor(ctx context.Context, c *client.Client, roomID, statePath string, wait time.Duration, want func([]Msg) bool) ([]Msg, error) {
	deadline := time.Now().Add(wait)
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	for {
		var stream *Stream
		if wait > 0 {
			var err error
			if stream, err = Subscribe(ctx, c, roomID, time.Until(deadline)); err != nil {
				if ctx.Err() != nil {
					return nil, nil // time is up
				}
				return nil, err
			}
		}
		news, self, err := readWanted(ctx, c, roomID, statePath, want)
		if err != nil || len(news) > 0 || wait <= 0 {
			stream.Close()
			return news, err
		}
		_, err = stream.NextFromOther(self)
		stream.Close()
		if ctx.Err() != nil {
			return nil, nil // time is up
		}
		if err != nil {
			return nil, err
		}
		// Another agent wrote, or the server ended the stream at its
		// own wait limit: read again (a hook may have taken it).
	}
}

// readWanted is one locked read; the cursor moves only if want
// accepts the new messages.
func readWanted(ctx context.Context, c *client.Client, roomID, statePath string, want func([]Msg) bool) ([]Msg, int64, error) {
	unlock, err := LockState(statePath)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	st := LoadState(statePath)
	next := st
	news, err := FetchNew(ctx, c, roomID, &next)
	if err != nil {
		return nil, 0, err
	}
	if len(news) == 0 || !want(news) {
		return nil, next.SelfID, nil
	}
	return news, next.SelfID, SaveState(statePath, next)
}
