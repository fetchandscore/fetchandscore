// Starts a real API against a throwaway database and a static server for the
// site, so the end-to-end tests exercise the same code that ships.

import { execFileSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { API_ORIGIN, API_PORT, WEB_ORIGIN, WEB_PORT } from './playwright.config.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, '..');

const apiBinary = path.join(repo, 'bin', 'fetchandscore');
const ctlBinary = path.join(repo, 'bin', 'fnsctl');

/** Waits for a URL to answer, or throws. */
async function waitForServer(url, label, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // Not up yet.
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`${label} did not come up at ${url}`);
}

export class Harness {
  constructor() {
    this.dir = mkdtempSync(path.join(tmpdir(), 'fns-e2e-'));
    this.dbPath = path.join(this.dir, 'e2e.db');
    this.logPath = path.join(this.dir, 'api.log');
    this.api = null;
    this.web = null;
  }

  async start() {
    if (!existsSync(apiBinary) || !existsSync(ctlBinary)) {
      throw new Error(`Binaries missing. Run: make build (looked in ${path.join(repo, 'bin')})`);
    }

    execFileSync(ctlBinary, ['seed', '-db', this.dbPath], { stdio: 'pipe' });

    const logStream = await import('node:fs').then((fs) => fs.openSync(this.logPath, 'a'));

    this.api = spawn(apiBinary, [], {
      env: {
        ...process.env,
        // Development mode writes the sign-in link to the log instead of
        // sending mail, which is how a test signs in without an inbox.
        FNS_DEV: '1',
        FNS_ADDR: `127.0.0.1:${API_PORT}`,
        FNS_BASE_URL: WEB_ORIGIN,
        FNS_ALLOWED_ORIGIN: WEB_ORIGIN,
        FNS_SECURE_COOKIES: 'false',
        FNS_DB_PATH: this.dbPath,
      },
      stdio: ['ignore', logStream, logStream],
    });

    this.web = spawn(
      process.execPath,
      [path.join(here, 'static-server.js'), String(WEB_PORT), path.join(repo, 'web'), API_ORIGIN],
      { stdio: 'ignore' },
    );

    await waitForServer(`${API_ORIGIN}/healthz`, 'API');
    await waitForServer(`${WEB_ORIGIN}/index.html`, 'static site');
  }

  /**
   * Signs an address in by requesting a link and reading the token out of the
   * development log, which is the same path a real user takes minus the inbox.
   */
  async signIn(page, email) {
    const res = await fetch(`${API_ORIGIN}/api/auth/request`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Origin: WEB_ORIGIN },
      body: JSON.stringify({ email }),
    });
    if (!res.ok) throw new Error(`requesting a link failed: ${res.status}`);

    const token = await this.latestToken();
    await page.goto(`/auth.html?token=${token}`);
    await page.waitForURL(/index\.html|\/$/, { timeout: 15_000 });
  }

  /** The most recent sign-in token the server logged. */
  async latestToken() {
    for (let attempt = 0; attempt < 40; attempt++) {
      const log = existsSync(this.logPath) ? readFileSync(this.logPath, 'utf8') : '';
      const matches = [...log.matchAll(/token=([A-Za-z0-9_-]+)/g)];
      if (matches.length > 0) return matches.at(-1)[1];
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error('no sign-in token appeared in the API log');
  }

  async stop() {
    this.api?.kill('SIGTERM');
    this.web?.kill('SIGTERM');
    // Give the API a moment to close the database cleanly.
    await new Promise((r) => setTimeout(r, 300));
    rmSync(this.dir, { recursive: true, force: true });
  }
}
