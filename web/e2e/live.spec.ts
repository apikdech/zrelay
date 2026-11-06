import { expect, test } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { build } from 'vite';

// Live view end to end (design §7.2): the SPA built by Vite, served by the real
// zview-server, whose live upstream is the real internal/relay/live server fed
// by the in-process RTSP fixture (helper binary internal/testutil/cmd/livefixture).
// Every port is ephemeral and every process is private to this spec.
const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = fileURLToPath(new URL('../..', import.meta.url));

interface FixtureInfo {
  upstream: string;
  camera: string;
  key: string;
  api_token_sha256: string;
}

let dir: string;
let origin: string;
let fixture: FixtureInfo;
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
  dir = mkdtempSync(join(tmpdir(), 'zview-live-'));
  const app = join(dir, 'app');
  const store = join(dir, 'store');
  mkdirSync(store);

  await build({ configFile: webRoot + 'vite.config.ts', root: webRoot, logLevel: 'warn', build: { outDir: app, emptyOutDir: true } });
  goBuild(join(dir, 'livefixture'), './internal/testutil/cmd/livefixture');
  goBuild(join(dir, 'zview-server'), './cmd/zview-server');

  const fx = spawn(join(dir, 'livefixture'), ['--camera', 'front'], { cwd: repoRoot, stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(fx);
  fixture = JSON.parse(await firstMatch(fx, 'stdout', (l) => (l.startsWith('{') ? l : null))) as FixtureInfo;

  writeFileSync(
    join(dir, 'zview.yaml'),
    [
      'version: 2',
      'server:',
      '  listen: 127.0.0.1:0',
      `  api_token_sha256: ["${fixture.api_token_sha256}"]`,
      `  app_dir: ${app}`,
      'storage:',
      '  driver: local',
      '  local:',
      `    base_dir: ${store}`,
      'index:',
      '  cameras_fallback: [front]',
      'live:',
      `  upstream: ${fixture.upstream}`,
      '',
    ].join('\n'),
  );
  const zs = spawn(join(dir, 'zview-server'), ['--config', join(dir, 'zview.yaml')], { stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(zs);
  origin = 'http://' + (await firstMatch(zs, 'stderr', (l) => /addr=(\S+)/.exec(l)?.[1] ?? null));
});

test.afterAll(() => {
  for (const c of children) c.kill('SIGTERM');
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test('a live tile reaches the live state and plays 640 px wide video', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'loopback WebRTC is asserted on Chromium; phones are verified manually');
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));

  // Import the throwaway key through the real onboarding form, then open Live.
  await page.goto(`${origin}/#/key`);
  await page.getByLabel('Master key').fill(fixture.key);
  await page.getByRole('button', { name: 'Import key' }).click();
  await page.getByRole('link', { name: 'Live' }).click();

  const tile = page.getByTestId('live-tile');
  await expect(tile).toHaveAttribute('data-camera', 'front');
  await expect(tile).toHaveAttribute('data-state', 'live', { timeout: 30_000 });
  const video = tile.locator('video');
  await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.videoWidth), { timeout: 30_000 }).toBe(640);
  expect(await video.evaluate((v: HTMLVideoElement) => v.muted)).toBe(true);
  expect(errors).toEqual([]);
});
