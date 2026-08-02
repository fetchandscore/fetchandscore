import { expect, test } from '@playwright/test';
import { Harness } from './harness.js';

let harness;

test.beforeAll(async () => {
  harness = new Harness();
  await harness.start();
});

test.afterAll(async () => {
  await harness?.stop();
});

/** Opens the first round of the seeded session, signed in as the captain. */
async function openFirstRound(page) {
  await harness.signIn(page, 'brad@example.com');

  await expect(page.getByRole('heading', { name: 'Fetch and Score' })).toBeVisible();
  await page.getByText('Week 1').first().click();

  await expect(page.getByRole('heading', { name: 'Week 1' })).toBeVisible();
  await page
    .getByRole('link', { name: /Round 1/ })
    .first()
    .click();

  await expect(page.getByRole('button', { name: 'START' })).toBeVisible();
}

/** The big score readout in the header. */
function scoreText(page) {
  return page.locator('header p.clock').first();
}

test('a signed-out visitor is sent to sign in', async ({ page }) => {
  await page.goto('/index.html');
  await expect(page).toHaveURL(/auth\.html/);
  await expect(page.getByText('Email me a link')).toBeVisible();
});

test('the dashboard lists the seeded session', async ({ page }) => {
  await harness.signIn(page, 'brad@example.com');

  await expect(page.getByText('Happening now')).toBeVisible();
  await expect(page.getByText('Demo Disc Dogs')).toBeVisible();
  await expect(page.getByText('3 teams')).toBeVisible();
});

test('scoring a round produces the score the rules require', async ({ page }) => {
  await openFirstRound(page);

  await page.getByRole('button', { name: 'START' }).click();

  // The preroll runs for three seconds before the clock and the grid appear.
  await expect(page.getByRole('button', { name: /40-50 yd \+ AIR/ })).toBeVisible({
    timeout: 10_000,
  });

  await page.getByRole('button', { name: /40-50 yd \+ AIR/ }).click(); // 5.5
  await expect(scoreText(page)).toHaveText('5.5');

  await page.getByRole('button', { name: /^30-40 yd 3$/ }).click(); // 3
  await expect(scoreText(page)).toHaveText('8.5');

  await page.getByRole('button', { name: /MISS/ }).click(); // 0
  await expect(scoreText(page)).toHaveText('8.5');

  await page.getByRole('button', { name: /^20-30 yd 2$/ }).click(); // 2
  await expect(scoreText(page)).toHaveText('10.5');

  // Undo removes the most recent throw, and only that one.
  await page.getByRole('button', { name: /Undo/ }).click();
  await expect(scoreText(page)).toHaveText('8.5');

  await page.getByRole('button', { name: 'Confirm round' }).click();
  await expect(page.getByText('Final')).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('p.clock').first()).toHaveText('8.5');
});

test('the tiny-dog bonus is applied to the right team', async ({ page }) => {
  await harness.signIn(page, 'brad@example.com');
  await page.getByText('Week 1').first().click();

  // Pip is the tiny dog in the seeded club.
  const pip = page.locator('li').filter({ hasText: 'Pip' });
  await pip.getByRole('link', { name: /Round 1/ }).click();

  await expect(page.getByText('tiny')).toBeVisible();

  await page.getByRole('button', { name: 'START' }).click();
  // 3 for the zone plus 1 for the tiny dog.
  await page.getByRole('button', { name: /^30-40 yd 4$/ }).click({ timeout: 10_000 });
  await expect(scoreText(page)).toHaveText('4');
});

// The reason the write queue exists. A scorekeeper in a dead spot has to be
// able to keep tapping, and nothing may be lost or double-counted.
test('throws tapped while offline are saved once the connection returns', async ({
  page,
  context,
}) => {
  await harness.signIn(page, 'sam@example.com');
  await page.getByText('Week 1').first().click();

  const moose = page.locator('li').filter({ hasText: 'Moose' });
  await moose.getByRole('link', { name: /Round 1/ }).click();

  await page.getByRole('button', { name: 'START' }).click();
  await page.getByRole('button', { name: /^40-50 yd 5$/ }).click({ timeout: 10_000 });
  await expect(scoreText(page)).toHaveText('5');

  // The signal drops.
  await context.setOffline(true);

  await page.getByRole('button', { name: /^30-40 yd 3$/ }).click();
  await page.getByRole('button', { name: /^20-30 yd 2$/ }).click();
  await page.getByRole('button', { name: /^10-20 yd 1$/ }).click();

  // The score keeps up regardless: the tally is local and the writes queue.
  await expect(scoreText(page)).toHaveText('11');
  await expect(page.getByText(/saving/)).toBeVisible();

  // The signal comes back.
  await context.setOffline(false);
  await page.evaluate(() => dispatchEvent(new Event('online')));

  await expect(page.getByText(/saving/)).toBeHidden({ timeout: 20_000 });

  // Confirm, then reload from the server. The total must match exactly, with
  // no throw lost and none counted twice.
  await page.getByRole('button', { name: 'Confirm round' }).click();
  await expect(page.getByText('Final')).toBeVisible({ timeout: 20_000 });

  await page.reload();
  await expect(page.getByText('Final')).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('p.clock').first()).toHaveText('11');
});

test('a confirmed round appears on the public results page without an account', async ({
  page,
  browser,
}) => {
  await harness.signIn(page, 'brad@example.com');
  await page.getByText('Week 1').first().click();
  const sessionUrl = new URL(page.url());
  const sessionId = sessionUrl.searchParams.get('session');

  // A completely separate browser context: no cookies, no account.
  const anon = await browser.newContext();
  const anonPage = await anon.newPage();
  await anonPage.goto(`/results.html?session=${sessionId}`);

  await expect(anonPage.getByText('Demo Disc Dogs')).toBeVisible();
  await expect(anonPage.getByText(/confirmed rounds only/)).toBeVisible();
  await anon.close();
});

test('a false start clears the round', async ({ page }) => {
  await harness.signIn(page, 'alex@example.com');
  await page.getByText('Week 1').first().click();

  const pip = page.locator('li').filter({ hasText: 'Pip' });
  await pip.getByRole('link', { name: /Round 2/ }).click();

  await page.getByRole('button', { name: 'START' }).click();
  await page.getByRole('button', { name: /^40-50 yd 6$/ }).click({ timeout: 10_000 });
  await expect(scoreText(page)).toHaveText('6');

  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', { name: 'False start' }).click();

  await expect(page.getByRole('button', { name: 'START' })).toBeVisible();
  await expect(scoreText(page)).toHaveText('0');
});
