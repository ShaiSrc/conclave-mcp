// Package events — Broker unit tests.
package events

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBroker_PublishSubscribe exercises the basic
// publish-to-subscriber path: one subscriber receives
// the published payload.
func TestBroker_PublishSubscribe(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	ch, cancel := b.Subscribe("room1")
	defer cancel()

	const want = `{"id":1}`
	if n := b.Publish("room1", []byte(want)); n != 1 {
		t.Fatalf("Publish returned %d, want 1", n)
	}
	select {
	case ev := <-ch:
		if string(ev.Payload) != want {
			t.Errorf("payload=%q, want %q", ev.Payload, want)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the event within 1s")
	}
}

// TestBroker_PublishToUnknownRoom is a no-op (no subscribers).
func TestBroker_PublishToUnknownRoom(t *testing.T) {
	b := NewBroker()
	defer b.Close()
	if n := b.Publish("nobody", []byte("x")); n != 0 {
		t.Errorf("Publish returned %d, want 0", n)
	}
}

// TestBroker_MultipleSubscribers: every subscriber gets
// every event in the same room.
func TestBroker_MultipleSubscribers(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	c1, cancel1 := b.Subscribe("room1")
	defer cancel1()
	c2, cancel2 := b.Subscribe("room1")
	defer cancel2()

	if n := b.Publish("room1", []byte(`"hi"`)); n != 2 {
		t.Fatalf("Publish returned %d, want 2", n)
	}
	for i, ch := range []<-chan Event{c1, c2} {
		select {
		case ev := <-ch:
			if string(ev.Payload) != `"hi"` {
				t.Errorf("sub %d payload=%q, want %q", i, ev.Payload, `"hi"`)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d did not receive the event", i)
		}
	}
}

// TestBroker_RoomIsolation: events for room A do not leak
// to subscribers on room B.
func TestBroker_RoomIsolation(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	chA, cancelA := b.Subscribe("A")
	defer cancelA()
	chB, cancelB := b.Subscribe("B")
	defer cancelB()

	if n := b.Publish("A", []byte(`"a1"`)); n != 1 {
		t.Fatalf("Publish A returned %d, want 1", n)
	}
	select {
	case ev := <-chA:
		if string(ev.Payload) != `"a1"` {
			t.Errorf("A payload=%q, want a1", ev.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("A did not receive")
	}
	select {
	case ev := <-chB:
		t.Errorf("B received unexpected event: %s", ev.Payload)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestBroker_SlowSubscriberDrop: a subscriber whose channel
// is full does not back-pressure the publisher. Events for
// that subscriber are dropped; other subscribers still
// receive.
func TestBroker_SlowSubscriberDrop(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	fast, cancelFast := b.Subscribe("r")
	defer cancelFast()
	slow, cancelSlow := b.Subscribe("r")
	defer cancelSlow()
	// Reference slow so the lint check does not flag it;
	// the test below relies on the channel being drained
	// to zero during the assertion phase.
	_ = slow

	// Fill both subscribers' channels (size 16) without
	// draining. After 16 publishes, slow's channel is
	// full; the 17th publish drops for slow only.
	for i := 0; i < 16; i++ {
		if n := b.Publish("r", []byte(`"filler"`)); n != 2 {
			t.Fatalf("Publish %d returned %d, want 2", i, n)
		}
	}
	// Drain fast.
	for i := 0; i < 16; i++ {
		select {
		case <-fast:
		case <-time.After(time.Second):
			t.Fatalf("fast did not receive event %d", i)
		}
	}
	// One more publish. slow is full; the event is dropped
	// for slow only. fast receives it.
	if n := b.Publish("r", []byte(`"after"`)); n != 1 {
		t.Errorf("Publish returned %d, want 1 (slow is full)", n)
	}
	select {
	case ev := <-fast:
		if string(ev.Payload) != `"after"` {
			t.Errorf("fast payload=%q, want 'after'", ev.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("fast did not receive 'after'")
	}
}

// TestBroker_CancelStopsDelivery: after cancel, the
// subscriber's done channel is closed and no further
// events are delivered.
func TestBroker_CancelStopsDelivery(t *testing.T) {
	b := NewBroker()
	defer b.Close()
	ch, cancel := b.Subscribe("r")
	cancel()
	// Subsequent Publish should not block. We can't
	// assert that directly, but we can assert that
	// SubscriberCount drops to 0 and no event arrives.
	if got := b.SubscriberCount("r"); got != 0 {
		t.Errorf("SubscriberCount=%d, want 0 after cancel", got)
	}
	b.Publish("r", []byte(`"x"`))
	select {
	case ev, open := <-ch:
		if open {
			t.Errorf("received event after cancel: %s", ev.Payload)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestBroker_CloseIdempotent: close twice is fine.
func TestBroker_CloseIdempotent(t *testing.T) {
	b := NewBroker()
	b.Close()
	b.Close() // must not panic
}

// TestBroker_ConcurrentPublishSubscribe exercises the
// publish path under concurrent writers. Race detector
// catches any unsynchronized state.
func TestBroker_ConcurrentPublishSubscribe(t *testing.T) {
	b := NewBroker()
	defer b.Close()
	var wg sync.WaitGroup
	var published atomic.Int64
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe("r")
			defer cancel()
			time.Sleep(5 * time.Millisecond) // let publishers fire
			// Drain whatever arrives; we only assert
			// "no panic, no deadlock, no use-after-close".
			for {
				select {
				case _, open := <-ch:
					if !open {
						return
					}
					published.Add(1)
				case <-time.After(20 * time.Millisecond):
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			b.Publish("r", []byte(`"x"`))
		}()
	}
	wg.Wait()
}
