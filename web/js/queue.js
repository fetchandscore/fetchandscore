// The write queue.
//
// A round is 60 seconds and a throw lands every few of them. If the network
// hiccups mid-round the scorekeeper cannot stop and wait, so taps go into a
// durable queue and drain in the background.
//
// Correctness rests on one thing: every write carries a client-generated id,
// and the server upserts on it. That makes a retry after an ambiguous timeout
// harmless, which is what lets this retry freely.

const STORAGE_PREFIX = 'fns.queue.';

/** Generates an id for a write. */
export function newClientId() {
  if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
  // Older Safari. Random enough for a per-round uniqueness constraint.
  return 'c-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

/**
 * A durable FIFO of pending writes for one round.
 *
 * Entries survive a reload, because a phone that locks, backgrounds the tab
 * and gets reopened is an ordinary event at a dog field, not an edge case.
 */
export class WriteQueue {
  /**
   * @param {string} key            storage key, usually the round id
   * @param {(item: object) => Promise<void>} send  performs one write
   * @param {object} [options]
   * @param {Storage} [options.storage]
   * @param {() => number} [options.now]
   * @param {(ms: number) => Promise<void>} [options.sleep]
   */
  constructor(key, send, { storage = globalThis.localStorage, now = Date.now, sleep } = {}) {
    this.key = STORAGE_PREFIX + key;
    this.send = send;
    this.storage = storage;
    this.now = now;
    this.sleep = sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));

    this.items = this.#load();
    /** The in-flight drain, or null. Held so callers can await one already running. */
    this.draining = null;
    /** Called whenever the pending count or error state changes. */
    this.onChange = () => {};
    /** The most recent transient failure, cleared by the next success. */
    this.lastError = null;
    /**
     * Writes the server refused outright.
     *
     * Kept separate from lastError, which a later success clears. A refused
     * write is a tap that will never be recorded, and in a scoring app that
     * has to be surfaced rather than quietly forgotten.
     */
    this.dropped = [];
  }

  get pending() {
    return this.items.length;
  }

  /** Adds a write and starts draining. Resolves once queued, not once sent. */
  enqueue(item) {
    this.items.push({ ...item, queuedAt: this.now() });
    this.#persist();
    this.onChange(this);
    void this.drain();
  }

  /**
   * Sends queued writes in order, one at a time.
   *
   * Strictly sequential: throws are ordered, and a parallel drain could land
   * them out of order or interleave a void with the throw it voids.
   */
  drain() {
    // Returning the in-flight drain rather than undefined means awaiting
    // drain() always means "wait until the queue is empty", whether or not one
    // was already running. Tests depend on that, and so does confirming a
    // round: it must not lock until every queued throw has landed.
    this.draining ??= this.#drainLoop().finally(() => {
      this.draining = null;
    });
    return this.draining;
  }

  async #drainLoop() {
    let backoff = 500;

    while (this.items.length > 0) {
      const item = this.items[0];

      try {
        await this.send(item);
        this.items.shift();
        this.#persist();
        this.lastError = null;
        this.onChange(this);
        backoff = 500;
      } catch (error) {
        // A rejected write will never be accepted however many times it is
        // resent. Dropping it is the only way to stop it blocking the queue
        // behind it forever.
        if (error?.isRetryable === false) {
          this.items.shift();
          this.#persist();
          this.dropped.push({ item, error });
          this.onChange(this);
          continue;
        }

        this.lastError = error;
        this.onChange(this);

        // Capped exponential backoff. Ten seconds is short enough that
        // reconnecting feels immediate to someone watching the pending count.
        await this.sleep(backoff);
        backoff = Math.min(backoff * 2, 10_000);
      }
    }
  }

  /** Discards everything pending. Used on a false start, when the round is void. */
  clear() {
    this.items = [];
    this.lastError = null;
    this.dropped = [];
    this.#persist();
    this.onChange(this);
  }

  #load() {
    try {
      const raw = this.storage?.getItem(this.key);
      const parsed = raw ? JSON.parse(raw) : [];
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      // Corrupt or unavailable storage must not stop someone scoring; the
      // queue simply starts empty and lives in memory.
      return [];
    }
  }

  #persist() {
    try {
      if (this.items.length === 0) this.storage?.removeItem(this.key);
      else this.storage?.setItem(this.key, JSON.stringify(this.items));
    } catch {
      // Private mode, or a full disk. In-memory queueing still works.
    }
  }
}
