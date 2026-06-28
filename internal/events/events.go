// Package events is the in-process pub/sub for room events.
//
// Broker design (SPEC-0003 §2.2 + ADR-0009):
//
//   - One Broker per server (held by Server.Deps).
//   - One subscriber per active SSE connection. A subscriber
//     owns a buffered channel (size 16) and a "done" signal.
//   - Publish is non-blocking: if a subscriber's channel is
//     full, the event is dropped for that subscriber only —
//     the publisher does not block. This is the right
//     trade-off for a "live stream of new messages" use case:
//     a slow reader should not back-pressure the whole
//     server.
//   - Close cancels every subscriber. Used during graceful
//     shutdown.
//
// The broker is single-node. A v0.3+ scale-up would need a
// pub/sub backplane (Redis, NATS, in-process ring buffer).
package events

import (
	"sync"
	"sync/atomic"
)

// Event is the payload sent to subscribers. Payload is the
// pre-marshaled JSON (e.g. one message in the same shape
// as GET /v1/rooms/{id}/messages returns for one row).
type Event struct {
	Payload []byte
}

// Broker is an in-process pub/sub for room events. It is
// safe for concurrent use.
type Broker struct {
	mu     sync.RWMutex
	subs   map[string]map[*subscriber]struct{}
	closed atomic.Bool
}

type subscriber struct {
	ch     chan Event
	done   chan struct{}
	closed atomic.Bool
}

// NewBroker returns a ready-to-use broker.
func NewBroker() *Broker {
	return &Broker{
		subs: make(map[string]map[*subscriber]struct{}),
	}
}

// Subscribe registers a new subscriber for roomID. The
// returned channel receives events until the caller calls
// the returned cancel func. The channel is buffered (size
// 16); if the caller is too slow, events are dropped for
// this subscriber (not for the others).
func (b *Broker) Subscribe(roomID string) (<-chan Event, func()) {
	s := &subscriber{
		ch:   make(chan Event, 16),
		done: make(chan struct{}),
	}
	b.mu.Lock()
	if _, ok := b.subs[roomID]; !ok {
		b.subs[roomID] = make(map[*subscriber]struct{})
	}
	b.subs[roomID][s] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		if s.closed.Swap(true) {
			return
		}
		close(s.done)
		b.mu.Lock()
		if m, ok := b.subs[roomID]; ok {
			delete(m, s)
			if len(m) == 0 {
				delete(b.subs, roomID)
			}
		}
		b.mu.Unlock()
	}
	return s.ch, cancel
}

// Publish broadcasts payload to every subscriber of roomID.
// Returns the number of subscribers that received the
// event. If a subscriber's channel is full OR the subscriber
// has been cancelled, the event is dropped for that
// subscriber; the rest still get it. Never blocks.
func (b *Broker) Publish(roomID string, payload []byte) int {
	if b.closed.Load() {
		return 0
	}
	b.mu.RLock()
	m, ok := b.subs[roomID]
	if !ok {
		b.mu.RUnlock()
		return 0
	}
	subs := make([]*subscriber, 0, len(m))
	for s := range m {
		subs = append(subs, s)
	}
	b.mu.RUnlock()

	ev := Event{Payload: payload}
	n := 0
	for _, s := range subs {
		// Check done first: a cancelled subscriber has its
		// done channel closed; if we land here, the
		// publisher should skip rather than risk sending to
		// a no-longer-drained channel.
		select {
		case <-s.done:
			continue
		default:
		}
		select {
		case s.ch <- ev:
			n++
		default:
			// full channel, drop for this subscriber
		}
	}
	return n
}

// SubscriberCount returns the number of live subscribers
// for roomID. Used by tests and the /healthz surface.
func (b *Broker) SubscriberCount(roomID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[roomID])
}

// Close cancels all subscribers. After Close, Publish is a
// no-op. Used during graceful shutdown. Idempotent.
func (b *Broker) Close() {
	if b.closed.Swap(true) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.subs {
		for s := range m {
			if !s.closed.Swap(true) {
				close(s.done)
			}
		}
	}
	b.subs = make(map[string]map[*subscriber]struct{})
}
