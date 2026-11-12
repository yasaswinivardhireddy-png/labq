import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Locator, type Page } from '@playwright/test';

const WCAG_22_AA = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];
const SHOTS = process.env.SCREENSHOT_DIR;

async function expectAccessible(page: Page) {
  const { violations } = await new AxeBuilder({ page }).withTags(WCAG_22_AA).analyze();
  expect(violations.map((v) => `${v.id} (${v.nodes.length}): ${v.help} at ${v.nodes[0]?.target.join(' ')}`)).toEqual([]);
}

/** Waits until the seeded demo jobs have all finished, so pages show their final state. */
async function demoSettled(page: Page) {
  await expect
    .poll(async () => {
      const res = await page.request.get('/api/v1/jobs');
      const { jobs } = (await res.json()) as { jobs: { state: string }[] };
      return jobs.length >= 6 && jobs.every((j) => !['queued', 'running'].includes(j.state));
    }, { timeout: 45_000 })
    .toBe(true);
}

const focusedName = (page: Page) =>
  page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null;
    return el?.getAttribute('aria-label') ?? el?.textContent?.trim() ?? '';
  });

/** Presses Tab until focus reaches the target, proving it is reachable from the keyboard. */
async function tabTo(page: Page, target: string | Locator, max = 40) {
  for (let i = 0; i < max; i++) {
    await page.keyboard.press('Tab');
    const reached =
      typeof target === 'string'
        ? (await focusedName(page)) === target
        : await target.evaluate((el) => el === document.activeElement);
    if (reached) return;
  }
  throw new Error(`Tab never reached ${typeof target === 'string' ? target : 'the target element'}`);
}

test('every page and form state passes axe WCAG 2.2 AA checks, including contrast', async ({ page }) => {
  test.setTimeout(120_000);
  await page.goto('/');
  await demoSettled(page);
  for (const path of ['/', '/jobs/j-6', '/devices/edge-4', '/devices/edge-1', '/jobs/new', '/reservations', '/no-such-page']) {
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await expectAccessible(page);
  }
  // A quarantined device, retried jobs and a passed failover are on the pages that were checked.
  await page.goto('/devices/edge-4');
  await expect(page.locator('.badge-bad')).toContainText('Quarantined');
  await page.goto('/jobs/j-6');
  await expect(page.getByRole('row', { name: /link-failover/ })).toContainText('Passed');
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/job.png`, fullPage: true });

  await page.goto('/reservations');
  await page.getByLabel('Length in minutes').fill('abc');
  await page.getByRole('button', { name: 'Reserve' }).click();
  await expect(page).toHaveTitle(/^Error: Reservations/);
  await expect(page.getByRole('alert')).toContainText('There is a problem');
  await expectAccessible(page);
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/reservation-errors.png`, fullPage: true });

  await page.goto('/');
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/overview.png`, fullPage: true });
});

test('keyboard only: skip link, then submit a job through the form', async ({ page }) => {
  await page.goto('/');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: 'Skip to main content' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('main')).toBeFocused();

  await page.goto('/');
  await tabTo(page, 'Submit a job');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: 'Submit a job' })).toBeVisible();

  const name = `kbd-${Date.now() % 100000}`;
  await tabTo(page, page.getByRole('radio', { name: 'edge-pair' }));
  await page.keyboard.press('Space');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('textbox', { name: 'Job name' })).toBeFocused();
  await page.keyboard.type(name);
  await page.keyboard.press('Tab');
  await expect(page.getByRole('combobox', { name: 'Team' })).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('radio', { name: 'Normal' })).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('combobox', { name: 'Reservation' })).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(page.getByRole('button', { name: 'Submit job' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: new RegExp(name) })).toBeVisible();
  await expect(page).toHaveURL(/\/jobs\/j-\d+$/);
});

test('pages reflow at 320 CSS pixels without horizontal scrolling (WCAG 1.4.10)', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 720 });
  for (const path of ['/', '/jobs/j-1', '/devices/edge-1', '/jobs/new', '/reservations']) {
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `${path} scrolls sideways by ${overflow}px`).toBeLessThanOrEqual(0);
  }
});

test('honors reduced motion and forced colors (Windows high contrast)', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce', forcedColors: 'active' });
  await page.goto('/jobs/new');
  expect(await page.getByRole('button', { name: 'Submit job' }).evaluate((el) => getComputedStyle(el).transitionDuration)).toBe('0s');
  await page.keyboard.press('Tab');
  await page.keyboard.press('Tab');
  expect(await page.evaluate(() => getComputedStyle(document.activeElement!).outlineStyle)).not.toBe('none');
  await expectAccessible(page);
});
