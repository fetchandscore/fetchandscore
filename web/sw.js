// A deliberately small service worker.
//
// It caches the app shell and the audio cues, and nothing else. It never
// touches API responses: a cached score is a wrong score, and the write queue
// already handles being offline far better than a cache could.
//
// The classic service-worker failure is users pinned to a stale build. Two
// things prevent it here: the cache name carries the build version, so a
// deploy evicts everything; and navigations are network-first, so a reachable
// server always wins.

const VERSION = 'v1';
const CACHE = `fns-shell-${VERSION}`;

const SHELL = [
  'index.html',
  'auth.html',
  'session.html',
  'round.html',
  'results.html',
  'css/app.build.css',
  'js/alpine.min.js',
  'js/api.js',
  'js/queue.js',
  'js/timer.js',
  'js/stream.js',
  'js/round.js',
  'js/scoring-table.js',
  'audio/offsets.json',
  'audio/cues.webm',
  'audio/cues.m4a',
  'img/icon.svg',
  'manifest.webmanifest',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      // A single missing file would reject addAll and leave the worker
      // uninstalled, so each is added independently.
      .then((cache) => Promise.allSettled(SHELL.map((path) => cache.add(path))))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((names) => Promise.all(names.filter((n) => n !== CACHE).map((n) => caches.delete(n))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (event) => {
  const { request } = event;

  if (request.method !== 'GET') return;

  const url = new URL(request.url);

  // Anything not on this origin is the API. Never cache it, never serve it
  // from a cache: scores must be live or absent, not stale.
  if (url.origin !== self.location.origin) return;

  // Navigations go to the network first so a deployed change is picked up
  // immediately, falling back to the cached shell only when offline.
  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(() => caches.match(request).then((r) => r || caches.match('index.html'))),
    );
    return;
  }

  // Static assets are cache-first: they are versioned by the cache name, so a
  // hit is always from the current build.
  event.respondWith(
    caches.match(request).then((cached) => {
      if (cached) return cached;
      return fetch(request).then((response) => {
        if (response.ok && response.type === 'basic') {
          const copy = response.clone();
          void caches.open(CACHE).then((cache) => cache.put(request, copy));
        }
        return response;
      });
    }),
  );
});
