package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

// heartbeatInterval is how often a comment is written to an idle stream.
//
// Cloudflare Tunnel and most proxies close a connection that says nothing for
// long enough. A round is 60 to 90 seconds; 20 seconds keeps the pipe warm
// through the gaps between rounds without being chatty.
const heartbeatInterval = 20 * time.Second

// handleStream serves a play session's events over Server-Sent Events.
//
// SSE rather than WebSockets: the traffic is one-way, the browser reconnects
// on its own, and it is plain HTTP, so it passes through the tunnel and
// carries the session cookie without ceremony.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	sessionID, err := pathInt(r, "sessionID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid session.")
		return
	}

	session, err := s.store.PlaySessionByID(r.Context(), sessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Watching a round as it happens is for the people at the field.
	if !s.requireRole(w, r, session.ClubID, user.ID, store.RoleMember) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "internal", "Streaming unavailable.")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Tells nginx and friends not to buffer, which would defeat the point.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.hub.Subscribe(sessionID, lastEventID(r))
	defer s.hub.Unsubscribe(sub)

	// Tell the browser how long to wait before reconnecting, then prove the
	// stream is open so the client can drop any "connecting" state.
	if _, err := fmt.Fprint(w, "retry: 3000\n\n: connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev, open := <-sub.Events:
			if !open {
				return
			}
			if err := writeSSE(w, ev); err != nil {
				s.log.Debug("stream write failed", "session", sessionID, "error", err)
				return
			}
			flusher.Flush()

		case <-ticker.C:
			// A comment is a no-op to the EventSource API but real bytes to
			// every proxy between here and the phone.
			//
			// A failed write here is the point of the heartbeat: it is how a
			// client that vanished without closing cleanly gets noticed, so
			// the goroutine and its subscription can be released.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				s.log.Debug("stream heartbeat failed", "session", sessionID, "error", err)
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev Event) error {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("marshalling %s: %w", ev.Name, err)
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, payload)
	return err
}

// lastEventID reads the resume point a reconnecting browser sends.
//
// EventSource replays it in a header automatically; the query parameter is
// there for clients that cannot set headers.
func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}
