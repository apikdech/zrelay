import { expect, test } from '@playwright/test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';
import { createServer, type ViteDevServer } from 'vite';

// Drives the canvas timeline in a real browser engine (ui-spec §4.6).
const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = fileURLToPath(new URL('../..', import.meta.url));

let server: ViteDevServer;
let cacheDir: string;
let origin: string;

test.beforeAll(async () => {
  // Every Playwright worker runs its own dev server. A private dependency
  // cache keeps them from racing on node_modules/.vite while optimizing.
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

test.describe('interaction', () => {
  test('click selects a clip at its first trigger offset; empty space snaps within 24 px', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(`${origin}/e2e/page/timeline.html?data=small`);
    await page.waitForFunction(() => (window.__tl?.draws ?? 0) > 0);
    const box = (await page.locator('canvas').boundingBox())!;
    const clip = await page.evaluate(() => {
      const l = window.__tl.layout();
      const lane = l.lanes[0]!;
      const c = lane.clips[0]!;
      return { x0: c.x0, x1: c.x1, y: c.y + c.h / 2 };
    });

    await page.mouse.click(box.x + (clip.x0 + clip.x1) / 2, box.y + clip.y);
    expect(await page.evaluate(() => window.__tl.selected)).toEqual([{ clipId: 'c1', offsetMs: 4200 }]);

    // 30 px beyond the end: nothing. 20 px beyond: snaps.
    await page.mouse.click(box.x + clip.x1 + 30, box.y + clip.y);
    expect(await page.evaluate(() => window.__tl.selected)).toHaveLength(1);
    await page.mouse.click(box.x + clip.x1 + 20, box.y + clip.y);
    expect(await page.evaluate(() => window.__tl.selected)).toHaveLength(2);
    expect(errors).toEqual([]);
  });

  test('wheel zooms around the pointer and double-click steps to the next preset', async ({ page }) => {
    await page.goto(`${origin}/e2e/page/timeline.html?data=small`);
    await page.waitForFunction(() => (window.__tl?.draws ?? 0) > 0);
    const box = (await page.locator('canvas').boundingBox())!;
    const span = () => page.evaluate(() => window.__tl.layout().view.spanMs);
    const full = await span();

    await page.mouse.move(box.x + 500, box.y + 40);
    await page.mouse.wheel(0, -300);
    await expect.poll(span).toBeLessThan(full);

    await page.locator('canvas').focus();
    await page.keyboard.press('Home');
    await expect.poll(span).toBe(full);

    await page.mouse.dblclick(box.x + 500, box.y + 40);
    await expect.poll(span).toBe(6 * 3_600_000);
  });

  test('dragging pans the view', async ({ page }) => {
    await page.goto(`${origin}/e2e/page/timeline.html?data=small`);
    await page.waitForFunction(() => (window.__tl?.draws ?? 0) > 0);
    const box = (await page.locator('canvas').boundingBox())!;
    await page.locator('canvas').focus();
    await page.keyboard.press('Equal'); // zoom in one preset so there is room to pan
    const start = () => page.evaluate(() => window.__tl.layout().view.startMs);
    await expect.poll(() => page.evaluate(() => window.__tl.layout().view.spanMs)).toBeLessThan(24 * 3_600_000);
    const before = await start();
    await page.mouse.move(box.x + 600, box.y + 40);
    await page.mouse.down();
    await page.mouse.move(box.x + 300, box.y + 40, { steps: 5 });
    await page.mouse.up();
    await expect.poll(start).toBeGreaterThan(before);
  });
});

test.describe('performance', () => {
  test('4 x 500 clips: average frame time under 16.7 ms while panning and zooming for 3 s', async ({ page, browserName }) => {
    test.skip(browserName !== 'chromium', 'frame budget is asserted on desktop Chromium; phones are verified manually');
    await page.goto(`${origin}/e2e/page/timeline.html?data=perf`);
    await page.waitForFunction(() => (window.__tl?.draws ?? 0) > 0);
    const r = await page.evaluate(() => window.__tl.runPerf(3000));
    test.info().annotations.push({ type: 'perf', description: JSON.stringify(r) });
    // A real frame loop ran, not an idle page.
    expect(r.frames).toBeGreaterThan(30);
    expect(r.avgDrawMs).toBeLessThan(16.7);
  });
});
