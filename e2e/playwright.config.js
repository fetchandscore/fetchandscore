import { defineConfig, devices } from '@playwright/test';

// Ports the harness starts the API and the static site on. Deliberately not
// the defaults, so a stray dev server cannot make a test pass by accident.
export const API_PORT = 8199;
export const WEB_PORT = 8200;
export const WEB_ORIGIN = `http://127.0.0.1:${WEB_PORT}`;
export const API_ORIGIN = `http://127.0.0.1:${API_PORT}`;

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.js',
  fullyParallel: false,
  // The suite drives one shared API and one database, so parallel workers
  // would race on the same rounds.
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['list']] : 'list',
  timeout: 60_000,

  use: {
    baseURL: WEB_ORIGIN,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'mobile',
      // A phone, because that is the only device this is ever used on.
      use: { ...devices['Pixel 7'] },
    },
  ],
});
