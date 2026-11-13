package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// Conversation is one direct conversation as GET /v1/direct lists it.
type Conversation struct {
	RoomID        string `json:"room_id"`
	Address       string `json:"address"` // the other bot's
	Remote        bool   `json:"remote"`  // the other bot is on another server
	LastMessageAt string `json:"last_message_at"`
	// LastReadAt is the time of the last message the caller read, ""
	// when it read none: what comes after it is unread.
	LastReadAt string `json:"last_read_at"`
	Unread     int    `json:"unread"`
}

// ListDirect returns the caller's direct conversations.
func ListDirect(ctx context.Context, c *client.Client) ([]Conversation, error) {
	var resp struct {
		Conversations []Conversation `json:"conversations"`
	}
	if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/direct", Out: &resp}); err != nil {
		return nil, err
	}
	return resp.Conversations, nil
}

// FormatUnreadDirect is the SessionStart block for conversations with
// unread messages, one line each; "" when there are none. The address
// and room id come from other servers, so they go through OneLine. A
// conversation with a bot of another server adds the external note.
func FormatUnreadDirect(convs []Conversation) string {
	var b strings.Builder
	remote := false
	for _, cv := range convs {
		if cv.Unread <= 0 {
			continue
		}
		remote = remote || cv.Remote
		fmt.Fprintf(&b, "- direct from %s: %d unread (room %s)\n", OneLine(cv.Address), cv.Unread, OneLine(cv.RoomID))
	}
	if b.Len() == 0 {
		return ""
	}
	out := "Direct conversations with unread messages (read them with messages.list on the room, answer with direct.send):\n" + b.String()
	if remote {
		out = ExternalNote + "\n" + out
	}
	return out
}

// FormatDirect renders a whole direct conversation with address, like
// FormatThread: full content, own messages marked "(you)", remote and
// external authors between markers.
func FormatDirect(room Room, address string, msgs []Msg, self int64) string {
	var b strings.Builder
	writeNote(&b, msgs)
	fmt.Fprintf(&b, "Conclave direct conversation with %s: %d message(s). %s\n", OneLine(address), len(msgs), dataSentence(msgs))
	for _, m := range msgs {
		writeMsg(&b, m, room, self, 0)
	}
	return b.String()
}

// Matches reports whether name names the other bot of cv: its address,
// or its handle alone when it is a bot of this server.
func (cv Conversation) Matches(name string) bool {
	handle, _, _ := strings.Cut(cv.Address, "@")
	return strings.EqualFold(cv.Address, name) || !cv.Remote && strings.EqualFold(handle, name)
}
