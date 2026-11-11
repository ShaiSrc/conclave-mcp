package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func reply(id, to int64) Msg {
	m := Msg{ID: id}
	if to != 0 {
		m.Metadata = json.RawMessage(fmt.Sprintf(`{"reply_to":%d,"x":1}`, to))
	}
	return m
}

func ids(ms []Msg) []int64 {
	var out []int64
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestThread(t *testing.T) {
	// 1 <- 2 <- 4, and 3 <- 5; 6 replies to a deleted message.
	msgs := []Msg{reply(1, 0), reply(2, 1), reply(3, 0), reply(4, 2), reply(5, 3), reply(6, 99)}
	for _, tc := range []struct {
		id   int64
		want []int64
	}{
		{1, []int64{1, 2, 4}},
		{4, []int64{1, 2, 4}}, // from a reply: walks up to the root
		{3, []int64{3, 5}},
		{6, []int64{6}},
		{99, nil}, // not in the room
	} {
		if got := ids(Thread(msgs, tc.id)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Thread(%d) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestReplyTo(t *testing.T) {
	if got := ReplyTo(reply(2, 1)); got != 1 {
		t.Errorf("ReplyTo = %d, want 1", got)
	}
	for _, meta := range []string{``, `null`, `{}`, `{"reply_to":"7"}`, `[1]`} {
		if got := ReplyTo(Msg{Metadata: json.RawMessage(meta)}); got != 0 {
			t.Errorf("ReplyTo(%q) = %d, want 0", meta, got)
		}
	}
}

func TestFormatThread_DropsControlCharsKeepsLines(t *testing.T) {
	out := FormatThread("r", []Msg{{ID: 1, Author: 2, Content: "a\x1b[31mb\x07\nc\td"}}, nil, 0)
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("control characters in output: %q", out)
	}
	if !strings.Contains(out, ": a[31mb\n    c\td\n") {
		t.Errorf("want line break and tab kept: %q", out)
	}
}

func TestMsgJSONKeepsZeroSize(t *testing.T) {
	b, _ := json.Marshal(Msg{ID: 1})
	if !strings.Contains(string(b), `"size":0`) {
		t.Errorf("size dropped: %s", b)
	}
}
