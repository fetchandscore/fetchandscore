// Talking to the API.
//
// The site is static on GitHub Pages and the API lives on another host, so
// every call is cross-origin and must opt into sending the session cookie.

/**
 * Where the API lives. Overridable at runtime so a local dev server, a staging
 * host and production can share one build.
 */
export const API_BASE = (() => {
  const meta = document.querySelector('meta[name="fns-api-base"]');
  if (meta?.content) return meta.content.replace(/\/$/, '');
  if (globalThis.FNS_API_BASE) return String(globalThis.FNS_API_BASE).replace(/\/$/, '');
  return 'https://api.fetchandscore.com';
})();

/** Thrown for any non-2xx response, carrying enough to act on. */
export class ApiError extends Error {
  constructor(status, code, message) {
    super(message || code || `HTTP ${status}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }

  /** True when the caller should be sent back to sign in. */
  get isAuth() {
    return this.status === 401;
  }

  /**
   * True when retrying could plausibly work: a network blip or a server that
   * is briefly unwell. A 400 will never succeed on retry, so the write queue
   * must not spin on one.
   */
  get isRetryable() {
    return this.status === 0 || this.status === 429 || this.status >= 500;
  }
}

/**
 * Makes a JSON request.
 *
 * @param {string} path       API path beginning with /
 * @param {object} [options]
 * @param {string} [options.method]
 * @param {object} [options.body]
 * @param {AbortSignal} [options.signal]
 */
export async function request(path, { method = 'GET', body, signal } = {}) {
  let response;
  try {
    response = await fetch(API_BASE + path, {
      method,
      // Without this the session cookie is not sent cross-origin at all.
      credentials: 'include',
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
      signal,
    });
  } catch (cause) {
    // fetch rejects only on a network-level failure. Status 0 marks it as
    // retryable, which is what the write queue keys off.
    if (cause?.name === 'AbortError') throw cause;
    throw new ApiError(0, 'network', 'Could not reach the server.');
  }

  const payload = await response.json().catch(() => null);

  if (!response.ok) {
    throw new ApiError(response.status, payload?.error, payload?.message);
  }
  return payload;
}

export const api = {
  me: () => request('/api/me'),
  requestLink: (email) => request('/api/auth/request', { method: 'POST', body: { email } }),
  verify: (token) => request('/api/auth/verify', { method: 'POST', body: { token } }),
  logout: () => request('/api/auth/logout', { method: 'POST' }),

  dashboard: () => request('/api/dashboard'),
  session: (id) => request(`/api/sessions/${id}`),
  publicSession: (id) => request(`/api/public/sessions/${id}`),

  startRound: (id) => request(`/api/rounds/${id}/start`, { method: 'POST' }),
  graceRound: (id) => request(`/api/rounds/${id}/grace`, { method: 'POST' }),
  resetRound: (id) => request(`/api/rounds/${id}/reset`, { method: 'POST' }),
  confirmRound: (id) => request(`/api/rounds/${id}/confirm`, { method: 'POST' }),

  addThrow: (roundId, payload) =>
    request(`/api/rounds/${roundId}/throws`, { method: 'POST', body: payload }),
  voidThrow: (roundId, throwId) =>
    request(`/api/rounds/${roundId}/throws/${throwId}/void`, { method: 'POST' }),
};
