package httpapi

import "sync"

// Event is one thing that happened in a play session, fanned out to everyone
// watching it.
type Event struct {
	// ID increases within a session. Clients send it back as Last-Event-ID to
	// resume after a dropped connection.
	ID int64
	// Name is the SSE event type, e.g. "throw.added".
	Name string
	// Data is marshalled to JSON on the way out.
	Data any
}

const (
	// subscriberBuffer is how far behind a client may fall before the hub
	// starts dropping its events. A round is at most a few dozen throws, so a
	// client this far behind is not merely slow, it is gone.
	subscriberBuffer = 64

	// replayWindow is how many recent events per session are retained for
	// resuming. A phone waking from sleep needs to catch up on a round, not a
	// season.
	replayWindow = 256
)

// Subscriber is one open connection watching a play session.
type Subscriber struct {
	// Events is closed when the subscriber is removed.
	Events chan Event

	sessionID int64
}

// Hub fans session events out to connected clients.
//
// It is in-process on purpose. A single container serves one league; a message
// broker here would be infrastructure bought with no one to pay for it. If the
// service ever runs more than one replica, this is the seam to replace.
type Hub struct {
	mu      sync.Mutex
	subs    map[int64]map[*Subscriber]struct{}
	history map[int64][]Event
	nextID  int64
}

func NewHub() *Hub {
	return &Hub{
		subs:    make(map[int64]map[*Subscriber]struct{}),
		history: make(map[int64][]Event),
	}
}

// Subscribe registers for a session's events.
//
// Anything that happened after lastEventID is delivered immediately, so a
// client that reconnects does not miss the throws it slept through. Pass 0 for
// a fresh connection.
func (h *Hub) Subscribe(sessionID, lastEventID int64) *Subscriber {
	sub := &Subscriber{
		Events:    make(chan Event, subscriberBuffer),
		sessionID: sessionID,
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[*Subscriber]struct{})
	}
	h.subs[sessionID][sub] = struct{}{}

	if lastEventID > 0 {
		for _, ev := range h.history[sessionID] {
			if ev.ID > lastEventID {
				// The buffer is sized well above the replay window's practical
				// use, but a non-blocking send keeps a pathological case from
				// deadlocking Subscribe under the lock.
				select {
				case sub.Events <- ev:
				default:
				}
			}
		}
	}
	return sub
}

// Unsubscribe removes a subscriber and closes its channel. Calling it twice is
// safe, which matters because handlers unsubscribe from a defer.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subs, ok := h.subs[sub.sessionID]
	if !ok {
		return
	}
	if _, present := subs[sub]; !present {
		return
	}

	delete(subs, sub)
	if len(subs) == 0 {
		delete(h.subs, sub.sessionID)
	}
	close(sub.Events)
}

// Publish records an event and delivers it to everyone watching the session.
//
// Delivery is non-blocking. A scorekeeper's write must never wait on a
// spectator's phone, so a subscriber that has stopped reading simply misses
// events; its next reconnect replays them from history.
func (h *Hub) Publish(sessionID int64, name string, data any) Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	ev := Event{ID: h.nextID, Name: name, Data: data}

	hist := append(h.history[sessionID], ev)
	if len(hist) > replayWindow {
		hist = hist[len(hist)-replayWindow:]
	}
	h.history[sessionID] = hist

	for sub := range h.subs[sessionID] {
		select {
		case sub.Events <- ev:
		default:
		}
	}
	return ev
}

// Forget drops a session's retained history, called when a session closes so
// the map does not grow for the life of the process.
func (h *Hub) Forget(sessionID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.history, sessionID)
}
