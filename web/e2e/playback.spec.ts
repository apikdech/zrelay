import { expect, test, type Page } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { build } from 'vite';

// Playback engine end to end (ui-spec §4.3, §4.4): the SPA built by Vite and
// served by the real zview-server over the fixtures of testdata/clips/ (a real
// ~33 s clip recorded by the relay pipeline, plus a tampered and a truncated
// variant). Every port is ephemeral and every process is private to this spec.
const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = fileURLToPath(new URL('../..', import.meta.url));
const clipsDir = join(repoRoot, 'testdata', 'clips');
const manifest = JSON.parse(readFileSync(join(clipsDir, 'manifest.json'), 'utf8')) as {
  master_key: string;
  camera: string;
  duration_ms: number;
  complete: { clip_id: string };
  tamper: { clip_id: string };
  incomplete: { clip_id: string };
  tamper_fail_at_ms: number;
};
// k_api_sha256 of the Contract §4.5 vector key, which the fixtures are sealed with.
const vectors = JSON.parse(readFileSync(join(repoRoot, 'testdata', 'zrec', 'v1', 'vectors.json'), 'utf8')) as {
  derived: { k_api_sha256_hex: string };
};

test.describe.configure({ mode: 'serial' });

let dir: string;
const origins: Record<'complete' | 'tamper' | 'incomplete', string> = { complete: '', tamper: '', incomplete: '' };
const children: ChildProcess[] = [];

function goBuild(out: string, pkg: string) {
  const r = spawnSync('go', ['build', '-o', out, pkg], { cwd: repoRoot, encoding: 'utf8' });
  if (r.status !== 0) throw new Error(`go build ${pkg} failed:\n${r.stderr}`);
}

/** Resolves with the first line of `child`'s `stream` that `pick` accepts. */
function firstMatch(child: ChildProcess, stream: 'stdout' | 'stderr', pick: (line: string) => string | null): Promise<string> {
  return new Promise((resolve, reject) => {
    const rl = createInterface({ input: child[stream]! });
    const seen: string[] = [];
    const timer = setTimeout(() => reject(new Error(`timeout waiting for output; saw:\n${seen.join('\n')}`)), 60_000);
    rl.on('line', (line) => {
      seen.push(line);
      const v = pick(line);
      if (v !== null) {
        clearTimeout(timer);
        resolve(v);
      }
    });
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`exited ${code} before ready; saw:\n${seen.join('\n')}`));
    });
  });
}

test.beforeAll(async () => {
  test.setTimeout(300_000);
  dir = mkdtempSync(join(tmpdir(), 'zview-playback-'));
  const app = join(dir, 'app');
  await build({ configFile: webRoot + 'vite.config.ts', root: webRoot, logLevel: 'warn', build: { outDir: app, emptyOutDir: true } });
  goBuild(join(dir, 'zview-server'), './cmd/zview-server');

  for (const variant of ['complete', 'tamper', 'incomplete'] as const) {
    const store = join(dir, 'store-' + variant);
    mkdirSync(store);
    cpSync(join(clipsDir, variant), store, { recursive: true });
    const cfg = join(dir, `zview-${variant}.yaml`);
    writeFileSync(
      cfg,
      [
        'version: 2',
        'server:',
        '  listen: 127.0.0.1:0',
        `  api_token_sha256: ["${vectors.derived.k_api_sha256_hex}"]`,
        `  app_dir: ${app}`,
        'storage:',
        '  driver: local',
        '  root: zrelay',
        '  local:',
        `    base_dir: ${store}`,
        'index:',
        `  cameras_fallback: [${manifest.camera}]`,
        'live:',
        '  upstream: http://127.0.0.1:9',
        '',
      ].join('\n'),
    );
    const zs = spawn(join(dir, 'zview-server'), ['--config', cfg], { stdio: ['ignore', 'pipe', 'pipe'] });
    children.push(zs);
    origins[variant] = 'http://' + (await firstMatch(zs, 'stderr', (l) => /addr=(\S+)/.exec(l)?.[1] ?? null));
  }
});

test.afterAll(() => {
  for (const c of children) c.kill('SIGTERM');
  if (dir) rmSync(dir, { recursive: true, force: true });
});

/** Imports the vector key through the real onboarding form, then opens the demo page on `clip`. */
async function openDemo(page: Page, variant: keyof typeof origins, clip: string) {
  const origin = origins[variant];
  await page.goto(`${origin}/#/key`);
  await page.getByLabel('Master key').fill(manifest.master_key);
  await page.getByRole('button', { name: 'Import key' }).click();
  // The form clears itself once the worker holds the key; opening a clip earlier would race the import.
  await expect(page.getByLabel('Master key')).toHaveValue('');
  await page.evaluate((c) => {
    location.hash = `#/demo?clip=${c}`;
  }, clip);
  const demo = page.getByTestId('demo');
  await expect(demo).toHaveAttribute('data-clip', clip);
  return demo;
}

const currentTime = (page: Page) => page.getByTestId('player').evaluate((v: HTMLVideoElement) => v.currentTime);

test('the complete clip plays, stays verified and seeks to 20 s within 2 s', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const demo = await openDemo(page, 'complete', manifest.complete.clip_id);

  await expect(demo).toHaveAttribute('data-integrity', 'verified', { timeout: 30_000 });
  await expect(demo).not.toHaveAttribute('data-engine', '');
  await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(1);

  const t0 = Date.now();
  await page.getByTestId('seek-input').fill('20');
  await page.getByTestId('seek-go').click();
  await expect.poll(() => currentTime(page), { timeout: 2000, intervals: [50] }).toBeGreaterThanOrEqual(20);
  expect(Date.now() - t0).toBeLessThan(2500);
  // ... and keeps playing from there.
  const at = await currentTime(page);
  await expect.poll(() => currentTime(page), { timeout: 5000 }).toBeGreaterThan(at + 0.5);

  await expect(demo).toHaveAttribute('data-integrity', 'verified');
  expect(errors).toEqual([]);
});

test('a flipped ciphertext bit shows tampered at the right time and playback stops there', async ({ page }) => {
  const demo = await openDemo(page, 'tamper', manifest.tamper.clip_id);
  await expect(demo).toHaveAttribute('data-integrity', 'tampered', { timeout: 30_000 });
  const failedAt = Number(await demo.getAttribute('data-failed-at-ms'));
  expect(Math.abs(failedAt - manifest.tamper_fail_at_ms)).toBeLessThanOrEqual(2000);
  await expect(page.getByTestId('integrity')).toContainText('This recording failed verification at ~');

  // The verified part plays, nothing past the failure does.
  await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(1);
  await page.waitForTimeout(1500);
  expect(await currentTime(page)).toBeLessThanOrEqual(failedAt / 1000 + 0.5);
  await expect(page.getByTestId('export')).toHaveCount(0);
});

test('a truncated clip plays its verified part and shows incomplete', async ({ page }) => {
  const demo = await openDemo(page, 'incomplete', manifest.incomplete.clip_id);
  await expect(demo).toHaveAttribute('data-integrity', 'incomplete', { timeout: 30_000 });
  await expect(page.getByTestId('integrity')).toContainText('Recording was interrupted');
  await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(2);
  // It never turns into tampered: the cut is a truncation, not a forgery.
  await expect(demo).toHaveAttribute('data-integrity', 'incomplete');
});

test('a phone profile seeks outside its 30 s buffer by fetching the indexed fragment', async ({ browser }) => {
  const ctx = await browser.newContext({ hasTouch: true, viewport: { width: 390, height: 844 } });
  try {
    const page = await ctx.newPage();
    const demo = await openDemo(page, 'complete', manifest.complete.clip_id);
    await expect(demo).toHaveAttribute('data-profile', 'mobile');
    await expect(demo).toHaveAttribute('data-integrity', 'verified', { timeout: 30_000 });
    await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(1);

    // The mobile window buffers about 30 s ahead of the playhead, so near the end of the clip
    // the target is usually outside the buffer (always on WebKit; the unit tests pin the path).
    const target = Math.floor(manifest.duration_ms / 1000) - 1;
    expect(target).toBeGreaterThan(30);
    await page.getByTestId('seek-input').fill(String(target));
    await page.getByTestId('seek-go').click();
    await expect.poll(() => currentTime(page), { timeout: 3000, intervals: [50] }).toBeGreaterThanOrEqual(target);
    await expect(demo).toHaveAttribute('data-integrity', 'verified');
    // Playback reaches the end of the clip from the fetched fragment.
    await expect.poll(() => page.getByTestId('player').evaluate((v: HTMLVideoElement) => v.ended), { timeout: 10_000 }).toBe(true);
  } finally {
    await ctx.close();
  }
});

test('without MSE the Blob fallback plays and seeks the complete clip', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'MediaSource', { value: undefined, configurable: true });
    Object.defineProperty(window, 'ManagedMediaSource', { value: undefined, configurable: true });
  });
  const demo = await openDemo(page, 'complete', manifest.complete.clip_id);
  await expect(demo).toHaveAttribute('data-engine', 'blob', { timeout: 30_000 });
  await expect(demo).toHaveAttribute('data-integrity', 'verified');
  await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(1);
  await page.getByTestId('seek-input').fill('20');
  await page.getByTestId('seek-go').click();
  await expect.poll(() => currentTime(page), { timeout: 2000, intervals: [50] }).toBeGreaterThanOrEqual(20);
});
