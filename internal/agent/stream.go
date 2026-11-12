package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// Stream is a live subscription to a room's message events (SSE).
type Stream struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

// Subscribe opens the room's event stream and returns once the server
// says the subscription is live (": ok"). Reading the backlog after
// that cannot miss a message. The server ends the stream after wait
// (it caps wait, 300s by default); ctx ends it earlier.
func Subscribe(ctx context.Context, c *client.Client, roomID string, wait time.Duration) (*Stream, error) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/rooms/%s/events?wait=%d", c.BaseURL, roomID, secs), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")
	// No client timeout: the server's wait and ctx bound the stream.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, &client.NetError{Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("subscribe: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	sc := bufio.NewScanner(resp.Body)
	// 256 KiB of content can JSON-escape to ~1.5 MiB in one data line.
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		if bytes.HasPrefix(sc.Bytes(), []byte(":")) {
			return &Stream{body: resp.Body, sc: sc}, nil
		}
	}
	resp.Body.Close()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

// NextFromOther blocks until a message from a key other than self
// arrives (true), or the stream ends (false, with ctx's error if ctx
// ended it).
func (s *Stream) NextFromOther(self int64) (bool, error) {
	for {
		m, ok, err := s.Next()
		if !ok {
			return false, err
		}
		if m.Author != self {
			return true, nil
		}
	}
}

// Next blocks until the next message event and returns the message
// (the server sends it whole), or false when the stream ends.
func (s *Stream) Next() (Msg, bool, error) {
	var data []byte
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data[:0], bytes.TrimPrefix(after, []byte(" "))...)
			continue
		}
		if len(line) != 0 || len(data) == 0 {
			continue
		}
		var m Msg
		// Pings carry no id.
		if json.Unmarshal(data, &m) == nil && m.ID != 0 {
			return m, true, nil
		}
		data = data[:0]
	}
	return Msg{}, false, s.sc.Err()
}

// Close ends the subscription.
func (s *Stream) Close() {
	if s != nil {
		s.body.Close()
	}
}
