import test from 'node:test';
import assert from 'node:assert/strict';

import { WriteQueue, newClientId } from './queue.js';

/** An in-memory stand-in for localStorage. */
function fakeStorage() {
  const data = new Map();
  return {
    getItem: (k) => (data.has(k) ? data.get(k) : null),
    setItem: (k, v) => data.set(k, v),
    removeItem: (k) => data.delete(k),
    get size() {
      return data.size;
    },
  };
}

/** An error shaped like ApiError, for controlling retry behaviour. */
function apiError(status, retryable) {
  const err = new Error(`HTTP ${status}`);
  err.status = status;
  err.isRetryable = retryable;
  return err;
}

const noSleep = () => Promise.resolve();

// Parks a drain at its first backoff. Used where the test wants writes sitting
// in the queue unsent; a resolving sleep would spin the retry loop forever,
// which is exactly what the real exponential backoff exists to prevent.
const neverWake = () => new Promise(() => {});

test('client ids are unique', () => {
  const ids = new Set();
  for (let i = 0; i < 1000; i++) ids.add(newClientId());
  assert.equal(ids.size, 1000);
});

test('drains queued writes in the order they were tapped', async () => {
  const sent = [];
  const q = new WriteQueue('r1', async (item) => void sent.push(item.zone), {
    storage: fakeStorage(),
    sleep: noSleep,
  });

  q.enqueue({ zone: '40-50' });
  q.enqueue({ zone: 'miss' });
  q.enqueue({ zone: '10-20' });
  await q.drain();

  assert.deepEqual(sent, ['40-50', 'miss', '10-20']);
  assert.equal(q.pending, 0);
});

test('retries a failed write until it succeeds, without reordering', async () => {
  let attempts = 0;
  const sent = [];
  const q = new WriteQueue(
    'r1',
    async (item) => {
      attempts++;
      // The first write fails twice before going through.
      if (item.zone === '40-50' && attempts < 3) throw apiError(0, true);
      sent.push(item.zone);
    },
    { storage: fakeStorage(), sleep: noSleep },
  );

  q.enqueue({ zone: '40-50' });
  q.enqueue({ zone: '30-40' });
  await q.drain();

  assert.deepEqual(sent, ['40-50', '30-40'], 'a retry must not let a later write overtake');
  assert.equal(q.pending, 0);
});

test('drops a write the server will never accept, rather than blocking behind it', async () => {
  const sent = [];
  const q = new WriteQueue(
    'r1',
    async (item) => {
      if (item.zone === 'bogus') throw apiError(400, false);
      sent.push(item.zone);
    },
    { storage: fakeStorage(), sleep: noSleep },
  );

  q.enqueue({ zone: 'bogus' });
  q.enqueue({ zone: '40-50' });
  await q.drain();

  assert.deepEqual(sent, ['40-50'], 'the valid write behind a rejected one must still land');
  assert.equal(q.pending, 0);

  // A refused write is a lost tap. It must stay visible even though a later
  // write succeeded, so the UI can tell someone their score is incomplete.
  assert.equal(q.dropped.length, 1);
  assert.equal(q.dropped[0].item.zone, 'bogus');
  assert.equal(q.dropped[0].error.status, 400);
  assert.equal(q.lastError, null, 'a transient error should clear once writes flow again');
});

test('pending writes survive a reload', async () => {
  const storage = fakeStorage();

  const first = new WriteQueue('r1', async () => { throw apiError(0, true); }, {
    storage,
    sleep: neverWake,
  });
  first.enqueue({ zone: '40-50', client_id: 'a' });
  first.enqueue({ zone: '30-40', client_id: 'b' });

  // The tab is closed mid-outage and reopened.
  const sent = [];
  const second = new WriteQueue('r1', async (item) => void sent.push(item.client_id), {
    storage,
    sleep: noSleep,
  });

  assert.equal(second.pending, 2, 'queued writes were lost across a reload');
  await second.drain();
  assert.deepEqual(sent, ['a', 'b']);
});

test('clearing a queue empties its storage too', () => {
  const storage = fakeStorage();
  const q = new WriteQueue('r1', async () => { throw apiError(0, true); }, {
    storage,
    sleep: neverWake,
  });

  q.enqueue({ zone: '40-50' });
  assert.equal(storage.size, 1);

  q.clear();
  assert.equal(q.pending, 0);
  assert.equal(storage.size, 0, 'a false start must not leave writes to replay');
});

test('survives unusable storage', async () => {
  const broken = {
    getItem() { throw new Error('denied'); },
    setItem() { throw new Error('denied'); },
    removeItem() { throw new Error('denied'); },
  };

  const sent = [];
  const q = new WriteQueue('r1', async (item) => void sent.push(item.zone), {
    storage: broken,
    sleep: noSleep,
  });

  q.enqueue({ zone: '40-50' });
  await q.drain();
  assert.deepEqual(sent, ['40-50'], 'private mode must not stop someone scoring');
});

test('reports pending count as it changes', async () => {
  const seen = [];
  const q = new WriteQueue('r1', async () => {}, { storage: fakeStorage(), sleep: noSleep });
  q.onChange = (queue) => seen.push(queue.pending);

  q.enqueue({ zone: '40-50' });
  await q.drain();

  assert.ok(seen.includes(1), 'never reported a pending write');
  assert.equal(seen.at(-1), 0, 'did not report the queue draining');
});

test('a concurrent drain does not double-send', async () => {
  const sent = [];
  const q = new WriteQueue(
    'r1',
    async (item) => {
      await new Promise((r) => setTimeout(r, 5));
      sent.push(item.zone);
    },
    { storage: fakeStorage(), sleep: noSleep },
  );

  q.enqueue({ zone: 'a' });
  q.enqueue({ zone: 'b' });
  await Promise.all([q.drain(), q.drain(), q.drain()]);

  assert.deepEqual(sent, ['a', 'b']);
});
