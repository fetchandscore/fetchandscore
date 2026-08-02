// The live feed.
//
// EventSource already reconnects on its own and replays Last-Event-ID, which
// is most of what this needs. The wrapper exists to expose connection state to
// the UI and to stop cleanly when a page navigates away.

import { API_BASE } from './api.js';

/** Connection states, for the indicator on screen. */
export const Connection = Object.freeze({
  CONNECTING: 'connecting',
  LIVE: 'live',
  OFFLINE: 'offline',
});

/**
 * Subscribes to a play session's events.
 *
 * @param {number|string} sessionId
 * @param {object} handlers            event name -> callback
 * @param {(state: string) => void} [onState]
 */
export function watchSession(sessionId, handlers, onState = () => {}) {
  let source = null;
  let closed = false;

  const connect = () => {
    if (closed) return;
    onState(Connection.CONNECTING);

    // withCredentials is what carries the session cookie cross-origin; the
    // stream is club-members-only, so without it every connection is a 401.
    source = new EventSource(`${API_BASE}/api/sessions/${sessionId}/stream`, {
      withCredentials: true,
    });

    source.onopen = () => onState(Connection.LIVE);

    source.onerror = () => {
      // EventSource retries by itself using the server's `retry:` hint, so
      // this only reports the state. Reconnecting here as well would produce
      // two competing connections.
      onState(Connection.OFFLINE);
    };

    for (const [name, handler] of Object.entries(handlers)) {
      source.addEventListener(name, (event) => {
        let payload = null;
        try {
          payload = JSON.parse(event.data);
        } catch {
          return;
        }
        handler(payload, event);
      });
    }
  };

  connect();

  return {
    close() {
      closed = true;
      source?.close();
      source = null;
    },
  };
}
