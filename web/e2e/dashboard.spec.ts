import { expect, test } from '@playwright/test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';
import { createServer, type ViteDevServer } from 'vite';

// The timeline page in a real browser engine: layout (desktop and phone),
// the virtualized list, keyboard shortcuts and the relay status dot.
const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = fileURLToPath(new URL('../..', import.meta.url));

let server: ViteDevServer;
let cacheDir: string;
let origin: string;

test.beforeAll(async () => {
  cacheDir = mkdtempSync(join(tmpdir(), 'zview-vite-'));
  server = await createServer({
    configFile: webRoot + 'vite.config.ts',
    root: webRoot,
    logLevel: 'warn',
    cacheDir,
    // Ephemeral port: never a fixed one, many test runs share this machine.
    server: { host: '127.0.0.1', port: 0, strictPort: false, fs: { allow: [repoRoot] } },
  });
  await server.listen();
  origin = `http://127.0.0.1:${(server.httpServer!.address() as AddressInfo).port}`;
});

test.afterAll(async () => {
  await server.close();
  rmSync(cacheDir, { recursive: true, force: true });
});

const url = () => `${origin}/e2e/page/dashboard.html`;

test.describe('desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('shows a short window of a long list, newest first, beside the player', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(url());
    const list = page.getByRole('listbox', { name: /recordings/i });
    await expect(list.getByRole('option').first()).toBeVisible();
    expect(await list.getByRole('option').count()).toBeLessThan(40);
    // Newest first: the last clip of the day (index 399) leads.
    await expect(list.getByRole('option').first()).toHaveAttribute('data-clip', /399$/);
    const [p, l] = await Promise.all([page.getByTestId('player').boundingBox(), list.boundingBox()]);
    expect(l!.x).toBeGreaterThan(p!.x + p!.width - 1); // the sidebar sits to the right of the player
    expect(errors).toEqual([]);
  });

  test('J and K walk the clips and the status dot is red for a 4-minute-old status', async ({ page }) => {
    await page.goto(url());
    const list = page.getByRole('listbox', { name: /recordings/i });
    await expect(list.getByRole('option').first()).toBeVisible();
    await page.keyboard.press('k');
    const sel = list.getByRole('option', { selected: true });
    await expect(sel).toHaveAttribute('data-clip', /399$/);
    await page.keyboard.press('j');
    await expect(sel).toHaveAttribute('data-clip', /398$/);
    await expect(page.getByTestId('relay-dot')).toHaveAttribute('data-level', 'bad');
  });

  test('a selection far down the list scrolls into view', async ({ page }) => {
    await page.goto(url());
    const list = page.getByRole('listbox', { name: /recordings/i });
    await expect(list.getByRole('option').first()).toBeVisible();
    await page.keyboard.press('k');
    for (let i = 0; i < 30; i++) await page.keyboard.press('j');
    await expect(list.getByRole('option', { selected: true })).toBeInViewport();
  });
});

test.describe('phone', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test('pins the player, folds the header into a menu and opens the events as a sheet', async ({ page }) => {
    await page.goto(url());
    const list = page.getByRole('listbox', { name: /recordings/i });
    await expect(list).toBeHidden();
    await expect(page.getByLabel('Camera')).toBeHidden();
    await page.getByRole('button', { name: 'Menu' }).click();
    await expect(page.getByLabel('Camera')).toBeVisible();

    await page.getByRole('button', { name: /^Events/ }).click();
    await expect(list.getByRole('option').first()).toBeVisible();
    const box = (await list.boundingBox())!;
    expect(box.y + box.height).toBeLessThanOrEqual(844 + 1);

    // The player's block is pinned to the top of the viewport while the page scrolls.
    const pin = await page.getByTestId('player').evaluate((el) => {
      const cs = getComputedStyle(el.closest('.sticky')!);
      return { position: cs.position, top: cs.top };
    });
    expect(pin).toEqual({ position: 'sticky', top: '0px' });
    // No horizontal page scroll.
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
});
