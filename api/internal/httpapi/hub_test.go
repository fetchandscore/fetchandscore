package httpapi

import (
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, ch <-chan Event, want string) Event {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Name != want {
			t.Fatalf("received %q, want %q", ev.Name, want)
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
		return Event{}
	}
}

func TestHub_DeliversToSubscribersOfTheSameSession(t *testing.T) {
	h := NewHub()

	a := h.Subscribe(1, 0)
	defer h.Unsubscribe(a)
	b := h.Subscribe(1, 0)
	defer h.Unsubscribe(b)

	h.Publish(1, "throw.added", map[string]any{"points": 5})

	waitFor(t, a.Events, "throw.added")
	waitFor(t, b.Events, "throw.added")
}

// Two clubs scoring at once must not see each other's rounds.
func TestHub_DoesNotLeakBetweenSessions(t *testing.T) {
	h := NewHub()

	mine := h.Subscribe(1, 0)
	defer h.Unsubscribe(mine)
	theirs := h.Subscribe(2, 0)
	defer h.Unsubscribe(theirs)

	h.Publish(1, "throw.added", nil)
	waitFor(t, mine.Events, "throw.added")

	select {
	case ev := <-theirs.Events:
		t.Fatalf("session 2 received %q from session 1", ev.Name)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()

	sub := h.Subscribe(1, 0)
	h.Unsubscribe(sub)

	h.Publish(1, "throw.added", nil)

	if _, open := <-sub.Events; open {
		t.Fatal("received an event after unsubscribing")
	}
}

// A reconnecting client sends Last-Event-ID. Anything it missed while the
// connection was down must be replayed, or a scorekeeper's phone waking from
// sleep would show a stale total.
func TestHub_ReplaysMissedEventsOnReconnect(t *testing.T) {
	h := NewHub()

	first := h.Subscribe(1, 0)
	h.Publish(1, "round.started", nil)
	started := waitFor(t, first.Events, "round.started")
	h.Unsubscribe(first)

	// Three events land while nobody is listening.
	h.Publish(1, "throw.added", map[string]any{"n": 1})
	h.Publish(1, "throw.added", map[string]any{"n": 2})
	h.Publish(1, "throw.added", map[string]any{"n": 3})

	resumed := h.Subscribe(1, started.ID)
	defer h.Unsubscribe(resumed)

	for i := 1; i <= 3; i++ {
		ev := waitFor(t, resumed.Events, "throw.added")
		if ev.ID != started.ID+int64(i) {
			t.Errorf("replayed event id %d, want %d", ev.ID, started.ID+int64(i))
		}
	}
}

// Event ids must increase, because the client uses them to resume.
func TestHub_AssignsIncreasingEventIDs(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(1, 0)
	defer h.Unsubscribe(sub)

	var last int64
	for range 5 {
		h.Publish(1, "throw.added", nil)
		ev := waitFor(t, sub.Events, "throw.added")
		if ev.ID <= last {
			t.Fatalf("event id %d did not advance past %d", ev.ID, last)
		}
		last = ev.ID
	}
}

// A phone that stops reading must not wedge the scorekeeper's writes. The hub
// drops events for a subscriber that cannot keep up rather than blocking.
func TestHub_PublishDoesNotBlockOnASlowSubscriber(t *testing.T) {
	h := NewHub()

	slow := h.Subscribe(1, 0)
	defer h.Unsubscribe(slow)

	done := make(chan struct{})
	go func() {
		for range subscriberBuffer * 3 {
			h.Publish(1, "throw.added", nil)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
}

func TestHub_ConcurrentSubscribeAndPublishIsRaceFree(t *testing.T) {
	h := NewHub()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sub := h.Subscribe(int64(n%3), 0)
			defer h.Unsubscribe(sub)
			for range 20 {
				h.Publish(int64(n%3), "throw.added", nil)
				select {
				case <-sub.Events:
				default:
				}
			}
		}(i)
	}
	wg.Wait()
}
