import { expect, test, type Page } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { build } from 'vite';

// First run, end to end (Task 41), on Chromium and WebKit: a fresh profile
// pastes the Contract §4.5 vector master key, the day view lists the fixture
// clip, which plays, seeks and exports, and the Live tab shows a live tile.
// The SPA is the production build (with its service worker) served by the real
// zview-server over testdata/clips/complete, and the live upstream is the real
// live server fed by the in-process RTSP fixture, sealed under the same key.
// Every port is ephemeral and every process is private to this spec.
const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = fileURLToPath(new URL('../..', import.meta.url));
const clipsDir = join(repoRoot, 'testdata', 'clips');
const manifest = JSON.parse(readFileSync(join(clipsDir, 'manifest.json'), 'utf8')) as {
  master_key: string;
  master_key_hex: string;
  camera: string;
  complete: { clip_id: string };
};
const vectors = JSON.parse(readFileSync(join(repoRoot, 'testdata', 'zrec', 'v1', 'vectors.json'), 'utf8')) as {
  derived: { k_api_sha256_hex: string };
};
const sidecarPath = join(clipsDir, 'complete', 'zrelay', 'clips', 'front-door', '2026', '11', '06', `171606359_${manifest.complete.clip_id}.json`);
const sidecar = JSON.parse(readFileSync(sidecarPath, 'utf8')) as {
  start_time: string;
  video: { plaintext_size_bytes: number };
};
// The browser runs in UTC, so the day shown is the clip's UTC day.
const CLIP_DAY = sidecar.start_time.slice(0, 10);
const EXPORT_NAME = `front-door_${CLIP_DAY.replaceAll('-', '')}-171606.mp4`;

interface FixtureInfo {
  upstream: string;
  camera: string;
  key: string;
  api_token_sha256: string;
  bearer: string;
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
  dir = mkdtempSync(join(tmpdir(), 'zview-firstrun-'));
  const app = join(dir, 'app');
  const store = join(dir, 'store');
  mkdirSync(store);
  cpSync(join(clipsDir, 'complete'), store, { recursive: true });

  await build({ configFile: webRoot + 'vite.config.ts', root: webRoot, logLevel: 'warn', build: { outDir: app, emptyOutDir: true } });
  goBuild(join(dir, 'livefixture'), './internal/testutil/cmd/livefixture');
  goBuild(join(dir, 'zview-server'), './cmd/zview-server');

  // Live video under the vector key, so one pasted key serves clips and live.
  const fx = spawn(join(dir, 'livefixture'), ['--camera', manifest.camera, '--key', 'ENV:ZVIEW_E2E_KEY'], {
    cwd: repoRoot,
    env: { ...process.env, ZVIEW_E2E_KEY: manifest.master_key },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  children.push(fx);
  fixture = JSON.parse(await firstMatch(fx, 'stdout', (l) => (l.startsWith('{') ? l : null))) as FixtureInfo;
  expect(fixture.api_token_sha256).toBe(vectors.derived.k_api_sha256_hex);

  writeFileSync(
    join(dir, 'zview.yaml'),
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

test.use({ timezoneId: 'UTC', viewport: { width: 1280, height: 800 } });

interface Probe {
  csp: { directive: string; blocked: string; disposition: string }[];
  /** Every message across the main thread / worker boundary, as inspectable text. */
  messages: { dir: 'out' | 'in'; type: string; text: string; bytes: number }[];
  /** Sizes and head bytes of what the stubbed file picker was given. */
  saved: { name: string; total: number; head: number[]; closed: boolean; aborted: boolean }[];
}

/**
 * Installed before any page script: collects CSP violation events (the policy
 * is Report-Only, which fires them too), wraps the Worker so every message in
 * either direction is recorded, and, on Chromium, stands in for the save picker.
 */
async function instrument(page: Page, stubPicker: boolean, rawKey: number[]) {
  await page.addInitScript(
    ({ stubPicker, rawKey }) => {
      const probe: Probe = { csp: [], messages: [], saved: [] };
      (window as unknown as { __probe: Probe }).__probe = probe;
      document.addEventListener('securitypolicyviolation', (e) => {
        probe.csp.push({ directive: e.violatedDirective, blocked: e.blockedURI, disposition: e.disposition });
      });

      const needle = new Uint8Array(rawKey);
      const hasRaw = (b: Uint8Array): boolean => {
        outer: for (let i = 0; i + needle.length <= b.length; i++) {
          for (let j = 0; j < needle.length; j++) if (b[i + j] !== needle[j]) continue outer;
          return true;
        }
        return false;
      };
      const describe = (v: unknown, depth = 0): { text: string; raw: boolean; bytes: number } => {
        if (v instanceof ArrayBuffer) return { text: '', raw: hasRaw(new Uint8Array(v)), bytes: v.byteLength };
        if (ArrayBuffer.isView(v)) {
          const u = new Uint8Array(v.buffer, v.byteOffset, v.byteLength);
          return { text: '', raw: hasRaw(u), bytes: u.length };
        }
        if (v === null || typeof v !== 'object') return { text: JSON.stringify(v) ?? '', raw: false, bytes: 0 };
        if (depth > 6) return { text: '[deep]', raw: false, bytes: 0 };
        let text = '';
        let raw = false;
        let bytes = 0;
        for (const [k, x] of Object.entries(v)) {
          const d = describe(x, depth + 1);
          text += `${JSON.stringify(k)}:${d.text},`;
          raw ||= d.raw;
          bytes += d.bytes;
        }
        return { text: `{${text}}`, raw, bytes };
      };
      const record = (dir: 'out' | 'in', data: unknown) => {
        const d = describe(data);
        const env = data as { msg?: { type?: string }; result?: unknown };
        probe.messages.push({
          dir,
          type: env?.msg?.type ?? (dir === 'in' ? 'reply' : 'unknown'),
          text: d.raw ? d.text + '<<RAW-KEY-BYTES>>' : d.text,
          bytes: d.bytes,
        });
      };

      const Orig = window.Worker;
      class Spy extends Orig {
        constructor(url: string | URL, opts?: WorkerOptions) {
          super(url, opts);
          super.addEventListener('message', (e: MessageEvent) => record('in', e.data));
        }
        override postMessage(msg: unknown, transfer?: Transferable[] | StructuredSerializeOptions) {
          record('out', msg);
          super.postMessage(msg, transfer as Transferable[]);
        }
      }
      window.Worker = Spy as unknown as typeof Worker;

      if (stubPicker) {
        (window as unknown as { showSaveFilePicker: unknown }).showSaveFilePicker = async (o: { suggestedName: string }) => ({
          createWritable: async () => {
            const rec = { name: o.suggestedName, total: 0, head: [] as number[], closed: false, aborted: false };
            probe.saved.push(rec);
            return {
              write: async (chunk: Uint8Array) => {
                if (rec.head.length < 16) rec.head.push(...chunk.subarray(0, 16 - rec.head.length));
                rec.total += chunk.byteLength;
              },
              close: async () => void (rec.closed = true),
              abort: async () => void (rec.aborted = true),
            };
          },
        });
      }
    },
    { stubPicker, rawKey },
  );
}

const probeOf = (page: Page) => page.evaluate(() => (window as unknown as { __probe: Probe }).__probe);

/** The fresh-profile start: paste the vector key into the onboarding form, then open the day view. */
async function importKeyAndOpenDay(page: Page) {
  await page.goto(origin);
  // No key yet: the timeline says so and points to the key page.
  await expect(page.getByRole('alert')).toContainText('Import your key first');
  await page.getByRole('link', { name: 'Open keys' }).click();
  await page.getByLabel('Master key').fill(manifest.master_key);
  await page.getByRole('button', { name: 'Import key' }).click();
  await expect(page.getByLabel('Master key')).toHaveValue(''); // cleared once the worker holds it
  await page.getByRole('link', { name: 'Timeline' }).click();
  await page.getByLabel('Date').fill(CLIP_DAY);
}

const rawKey = Array.from({ length: 32 }, (_, i) => i);
const currentTime = (page: Page) => page.getByTestId('player').evaluate((v: HTMLVideoElement) => v.currentTime);

async function playFirstClip(page: Page) {
  const list = page.getByRole('listbox', { name: /recordings/i });
  const option = list.getByRole('option');
  await expect(option).toHaveCount(1);
  await expect(option.first()).toHaveAttribute('data-clip', manifest.complete.clip_id);
  await option.first().click();
  await expect(page.getByTestId('integrity')).toHaveAttribute('data-kind', 'verified', { timeout: 30_000 });
  await expect.poll(() => currentTime(page), { timeout: 20_000 }).toBeGreaterThan(1);
}

test('a fresh profile imports the key, plays, seeks, exports and sees live video', async ({ page, browserName }) => {
  test.setTimeout(180_000);
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await instrument(page, browserName === 'chromium', rawKey);

  await importKeyAndOpenDay(page);
  await playFirstClip(page);

  // Seek: the native seek bar sets currentTime; the +10 s button goes through the engine.
  await page.getByTestId('player').evaluate((v: HTMLVideoElement) => {
    v.currentTime = 20;
  });
  await expect.poll(() => currentTime(page), { timeout: 5000, intervals: [50] }).toBeGreaterThanOrEqual(20);
  const at = await currentTime(page);
  await expect.poll(() => currentTime(page), { timeout: 8000 }).toBeGreaterThan(at + 0.5);
  await page.getByRole('button', { name: '-10 s' }).click();
  await expect.poll(() => currentTime(page), { timeout: 5000 }).toBeLessThan(at);
  await expect(page.getByTestId('integrity')).toHaveAttribute('data-kind', 'verified');

  // Export: the confirmation says the copy is unencrypted; the file is the whole plaintext clip.
  await page.getByRole('button', { name: 'Export' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('This saves an unencrypted copy on this device.');
  await expect(dialog).toContainText(EXPORT_NAME);
  if (browserName === 'chromium') {
    await dialog.getByRole('button', { name: 'Save' }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    const saved = (await probeOf(page)).saved;
    expect(saved).toHaveLength(1);
    expect(saved[0]).toMatchObject({ name: EXPORT_NAME, total: sidecar.video.plaintext_size_bytes, closed: true, aborted: false });
    expect(String.fromCharCode(...saved[0]!.head.slice(4, 8))).toBe('ftyp');
  } else {
    // No picker: the Blob fallback hands the browser a download.
    const download = page.waitForEvent('download', { timeout: 30_000 });
    await dialog.getByRole('button', { name: 'Save' }).click();
    expect((await download).suggestedFilename()).toBe(EXPORT_NAME);
  }

  // Live: the tab's tile reaches the live state and plays video.
  await page.getByRole('link', { name: 'Live' }).click();
  const tile = page.getByTestId('live-tile');
  await expect(tile).toHaveAttribute('data-camera', manifest.camera);
  if (browserName === 'chromium') {
    await expect(tile).toHaveAttribute('data-state', 'live', { timeout: 30_000 });
    await expect.poll(() => tile.locator('video').evaluate((v: HTMLVideoElement) => v.videoWidth), { timeout: 30_000 }).toBe(640);
  } else {
    // Playwright's WebKit build cannot complete loopback ICE (as in live.spec.ts): the tile must still be
    // there and never claim the viewer limit; live video on Safari is a manual check (docs/LIVE-CHECKS.md).
    await expect(tile).not.toHaveAttribute('data-state', 'limit');
  }

  // The policy is Report-Only: not one violation was reported along the way.
  const probe = await probeOf(page);
  expect(probe.csp).toEqual([]);
  expect(errors).toEqual([]);

  // The main thread never saw key material. Only the paste itself crosses to the worker (key.import);
  // the master key, its hex, K_api and the raw key bytes appear in no other message, in either direction.
  const secrets = [manifest.master_key, manifest.master_key_hex, fixture.bearer, fixture.bearer.slice(0, 20)];
  expect(probe.messages.filter((m) => m.type === 'key.import' && m.dir === 'out')).toHaveLength(1);
  expect(probe.messages.length).toBeGreaterThan(10); // the wrapper really saw the traffic
  for (const m of probe.messages) {
    if (m.type === 'key.import' && m.dir === 'out') continue;
    for (const s of secrets) expect(m.text, `${m.dir} ${m.type}`).not.toContain(s);
    expect(m.text, `${m.dir} ${m.type}`).not.toContain('<<RAW-KEY-BYTES>>');
  }
  // Nor did it reach the address bar or the page text.
  expect(page.url()).not.toContain(manifest.master_key_hex);
  expect(await page.evaluate(() => document.body.innerText)).not.toContain(manifest.master_key);
});

test('after playback Cache Storage holds the hashed shell and no data', async ({ page }) => {
  test.setTimeout(180_000);
  await instrument(page, false, rawKey);
  const apiResponses: { url: string; fromSW: boolean }[] = [];
  page.on('response', (r) => {
    const p = new URL(r.url()).pathname;
    if (p.startsWith('/api/') || p.startsWith('/live/')) apiResponses.push({ url: p, fromSW: r.fromServiceWorker() });
  });

  await importKeyAndOpenDay(page);
  await playFirstClip(page);

  // The worker registers once the page has loaded and takes control on activation.
  await page.evaluate(() => navigator.serviceWorker.ready.then(() => undefined));
  await expect.poll(() => page.evaluate(() => navigator.serviceWorker.controller !== null), { timeout: 30_000 }).toBe(true);

  const stored = await page.evaluate(async () => {
    const out: Record<string, string[]> = {};
    for (const name of await caches.keys()) out[name] = (await (await caches.open(name)).keys()).map((r) => new URL(r.url).pathname);
    return out;
  });
  const urls = Object.values(stored).flat();
  expect(Object.keys(stored).length).toBeGreaterThan(0);
  expect(urls.length).toBeGreaterThan(0);
  expect(urls.filter((u) => u.includes('/api') || u.includes('/live'))).toEqual([]);
  // Only the hashed shell: every entry sits under /assets/, no clip, sidecar or page.
  for (const u of urls) expect(u).toMatch(/^\/assets\/.+-[\w-]{8}\.\w+$/);

  expect(apiResponses.length).toBeGreaterThan(0);
  expect(apiResponses.filter((r) => r.fromSW)).toEqual([]);
  expect((await probeOf(page)).csp).toEqual([]);
});

test('the app is installable: manifest, icons and theme are served', async ({ page, request }) => {
  await page.goto(origin);
  const href = await page.locator('link[rel="manifest"]').getAttribute('href');
  expect(href).toBe('/manifest.webmanifest');
  const res = await request.get(origin + href!);
  expect(res.ok()).toBe(true);
  expect(res.headers()['content-type']).toContain('application/manifest+json');
  const m = (await res.json()) as {
    name: string;
    short_name: string;
    start_url: string;
    scope: string;
    display: string;
    background_color: string;
    theme_color: string;
    icons: { src: string; sizes: string; type: string; purpose?: string }[];
  };
  expect(m).toMatchObject({ name: 'zview', display: 'standalone', start_url: '/', scope: '/' });
  expect(m.short_name).toBeTruthy();
  expect(m.theme_color).toMatch(/^#[0-9a-f]{6}$/i);
  expect(m.background_color).toMatch(/^#[0-9a-f]{6}$/i);
  const sizes = m.icons.map((i) => `${i.sizes}/${i.purpose ?? 'any'}`);
  expect(sizes).toEqual(expect.arrayContaining(['192x192/any', '512x512/any', '512x512/maskable']));
  for (const icon of m.icons) {
    const r = await request.get(origin + icon.src);
    expect(r.ok(), icon.src).toBe(true);
    expect(r.headers()['content-type']).toBe(icon.type);
  }
  await expect(page.locator('link[rel="apple-touch-icon"]')).toHaveCount(1);
  await expect(page.locator('meta[name="theme-color"]')).toHaveAttribute('content', m.theme_color);

  // sw.js is revalidated on every load and carries the precache list of the build.
  const sw = await request.get(origin + '/sw.js');
  expect(sw.ok()).toBe(true);
  expect(sw.headers()['cache-control']).toBe('no-cache');
  const body = await sw.text();
  expect(body).toMatch(/\/assets\/index-[\w-]{8}\.js/);
  expect(body).not.toContain('/api/v1');
});
