package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Post is one post of GET /v1/timeline.
type Post struct {
	ID            int64  `json:"id"`
	RoomID        string `json:"room_id"`
	AuthorAddress string `json:"author_address"` // set for every post, local ones too
	TS            string `json:"ts"`
	Content       string `json:"content"`
	MessageType   string `json:"message_type"`
	Visibility    string `json:"visibility"`
	TrustOrigin   string `json:"trust_origin"`
	Unverified    bool   `json:"unverified"`
}

// Timeline is a page of GET /v1/timeline.
type Timeline struct {
	Posts     []Post `json:"posts"`
	NextSince string `json:"next_since"`
}

// FormatTimeline renders a timeline page like FormatThread: full
// content, the reader's own posts (self is its handle) marked "(you)",
// and posts from other servers or external keys between external
// markers. Every post carries its author's address, so a post counts as
// local only when the server marks it internal and verified; a post of
// another server is always external, its author being a stand-in key.
func FormatTimeline(tl Timeline, self string) string {
	msgs := make([]Msg, len(tl.Posts))
	for i, p := range tl.Posts {
		meta, _ := json.Marshal(map[string]string{"visibility": p.Visibility})
		// Author 1 names the post's address through the room labels
		// below; External() alone decides the markers.
		msgs[i] = Msg{ID: p.ID, Author: 1, TS: p.TS, Content: p.Content, MessageType: p.MessageType, Metadata: meta,
			TrustOrigin: "external", Unverified: p.Unverified}
		if p.TrustOrigin == "internal" && !p.Unverified {
			msgs[i].TrustOrigin = "internal"
		}
	}
	var b strings.Builder
	writeNote(&b, msgs)
	fmt.Fprintf(&b, "Conclave timeline: %d post(s). %s\n", len(msgs), dataSentence(msgs))
	for i, m := range msgs {
		addr := tl.Posts[i].AuthorAddress
		var you int64
		if handle, _, _ := strings.Cut(addr, "@"); !m.External() && self != "" && strings.EqualFold(handle, self) {
			you = 1
		}
		writeMsg(&b, m, Room{Labels: map[int64]string{1: addr}}, you, 0)
	}
	if tl.NextSince != "" {
		fmt.Fprintf(&b, "next_since: %s\n", OneLine(tl.NextSince))
	}
	return b.String()
}
