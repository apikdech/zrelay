import { expect, test } from '@playwright/test';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';
import { createServer, type ViteDevServer } from 'vite';
import { fsAllow } from '../vite-fs-allow';
import type { CaseResult } from '../test-support/vectors-runner';

// Runs the same cross-language vector loop as Vitest, but in a real browser
// engine (Chromium and WebKit) through a test page (Contract §4.5, ui-spec §7).
const webRoot = fileURLToPath(new URL('..', import.meta.url));

let server: ViteDevServer;
let origin: string;

test.beforeAll(async () => {
  server = await createServer({
    configFile: false,
    root: webRoot,
    logLevel: 'warn',
    // Ephemeral port: never a fixed one, many test runs share this machine.
    server: { host: '127.0.0.1', port: 0, strictPort: false, fs: { allow: fsAllow(webRoot) } },
  });
  await server.listen();
  origin = `http://127.0.0.1:${(server.httpServer!.address() as AddressInfo).port}`;
});

test.afterAll(async () => {
  await server.close();
});

test('every zrec vector passes in the browser engine', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(`${origin}/e2e/page/vectors.html`);
  await page.waitForFunction(() => window.__vectorResults !== undefined, undefined, { timeout: 30_000 });
  const results = (await page.evaluate(() => window.__vectorResults)) as CaseResult[];

  expect(errors).toEqual([]);
  // derived + 4 positive + 11 negative
  expect(results).toHaveLength(16);
  expect(results.filter((r) => !r.ok)).toEqual([]);
});
