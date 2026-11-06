import { describe, expect, it } from 'vitest';
import { installServiceWorker, type SwScope } from './sw';

// A service worker scope with just enough surface to drive its handlers.
class FakeCache {
  entries = new Map<string, Response>();
  async addAll(urls: string[]) {
    for (const u of urls) this.entries.set(new URL(u, 'https://zview.test').pathname, new Response('shell:' + u));
  }
  async match(req: Request) {
    return this.entries.get(new URL(req.url).pathname)?.clone();
  }
}
class FakeCaches {
  stores = new Map<string, FakeCache>();
  async open(name: string) {
    let c = this.stores.get(name);
    if (!c) this.stores.set(name, (c = new FakeCache()));
    return c;
  }
  async keys() {
    return [...this.stores.keys()];
  }
  async delete(name: string) {
    return this.stores.delete(name);
  }
}

type Listener = (e: unknown) => void;
function makeScope() {
  const listeners = new Map<string, Listener>();
  const calls = { skipWaiting: 0, claim: 0, fetched: [] as string[] };
  const caches = new FakeCaches();
  const scope: SwScope = {
    addEventListener: (type, l) => listeners.set(type, l as Listener),
    skipWaiting: async () => void calls.skipWaiting++,
    clients: { claim: async () => void calls.claim++ },
    location: { origin: 'https://zview.test' },
    caches: caches as unknown as CacheStorage,
    fetch: async (req) => {
      calls.fetched.push(new URL(req.url).pathname);
      return new Response('network');
    },
  };
  /** Fires a lifecycle event and waits for what it passed to waitUntil. */
  async function lifecycle(type: 'install' | 'activate') {
    const waits: Promise<unknown>[] = [];
    listeners.get(type)!({ waitUntil: (p: Promise<unknown>) => waits.push(p) });
    await Promise.all(waits);
  }
  /** Fires a fetch event; resolves with the response the worker supplied, or null when it left the request alone. */
  async function fetchEvent(path: string, init: { method?: string; origin?: string } = {}) {
    let supplied: Promise<Response> | null = null;
    const url = (init.origin ?? 'https://zview.test') + path;
    listeners.get('fetch')!({
      request: new Request(url, { method: init.method ?? 'GET' }),
      respondWith: (p: Promise<Response>) => (supplied = p),
    });
    return supplied ? await (supplied as Promise<Response>) : null;
  }
  return { scope, listeners, calls, caches, lifecycle, fetchEvent };
}

const SHELL = ['/assets/index-Bx3k9QaZ.js', '/assets/index-Dq1_a-Zx.css'];

describe('service worker', () => {
  it('precaches exactly the hashed shell on install and does not skip waiting by itself', async () => {
    const t = makeScope();
    installServiceWorker(t.scope, SHELL, 'v1');
    await t.lifecycle('install');
    expect([...t.caches.stores.keys()]).toEqual(['zview-shell-v1']);
    expect([...t.caches.stores.get('zview-shell-v1')!.entries.keys()]).toEqual(SHELL);
    expect(t.calls.skipWaiting).toBe(0);
  });

  it('skips waiting only when the page asks, with a SKIP_WAITING message', async () => {
    const t = makeScope();
    installServiceWorker(t.scope, SHELL, 'v1');
    t.listeners.get('message')!({ data: { type: 'something-else' } });
    t.listeners.get('message')!({ data: 'SKIP_WAITING' });
    expect(t.calls.skipWaiting).toBe(0);
    t.listeners.get('message')!({ data: { type: 'SKIP_WAITING' } });
    expect(t.calls.skipWaiting).toBe(1);
  });

  it('serves shell files from the cache and leaves everything else to the network', async () => {
    const t = makeScope();
    installServiceWorker(t.scope, SHELL, 'v1');
    await t.lifecycle('install');
    expect(await (await t.fetchEvent(SHELL[0]!))!.text()).toBe('shell:' + SHELL[0]);
    expect(t.calls.fetched).toEqual([]);
    // Not in the shell, so not intercepted: index.html, the manifest, sw.js itself.
    for (const p of ['/', '/index.html', '/manifest.webmanifest', '/sw.js', '/assets/other-Zz9kQ8aB.js']) {
      expect(await t.fetchEvent(p), p).toBeNull();
    }
  });

  it('never intercepts /api or /live, not even for a file it holds', async () => {
    const t = makeScope();
    // A bad build could list a data path in the shell: it still must not be served from Cache Storage.
    installServiceWorker(t.scope, [...SHELL, '/api/v1/clips', '/live/front-door'], 'v1');
    await t.lifecycle('install');
    const store = t.caches.stores.get('zview-shell-v1')!;
    expect([...store.entries.keys()].some((k) => k.startsWith('/api') || k.startsWith('/live'))).toBe(false);
    for (const p of ['/api/v1/status', '/api/v1/clips/x/video', '/live/front-door', '/live/front-door/sessions/1']) {
      expect(await t.fetchEvent(p), p).toBeNull();
      expect(await t.fetchEvent(p, { method: 'POST' }), p + ' POST').toBeNull();
      expect(await t.fetchEvent(p, { method: 'DELETE' }), p + ' DELETE').toBeNull();
    }
  });

  it('ignores non-GET and cross-origin requests even for shell paths', async () => {
    const t = makeScope();
    installServiceWorker(t.scope, SHELL, 'v1');
    await t.lifecycle('install');
    expect(await t.fetchEvent(SHELL[0]!, { method: 'POST' })).toBeNull();
    expect(await t.fetchEvent(SHELL[0]!, { origin: 'https://evil.test' })).toBeNull();
  });

  it('falls back to the network for a shell file that is not cached yet', async () => {
    const t = makeScope();
    installServiceWorker(t.scope, SHELL, 'v1');
    // No install ran, so the cache is empty.
    expect(await (await t.fetchEvent(SHELL[0]!))!.text()).toBe('network');
    expect(t.calls.fetched).toEqual([SHELL[0]]);
  });

  it('drops shells of older versions on activate and takes control', async () => {
    const t = makeScope();
    await t.caches.open('zview-shell-v0');
    await t.caches.open('zview-shell-v1');
    installServiceWorker(t.scope, SHELL, 'v1');
    await t.lifecycle('activate');
    expect(await t.caches.keys()).toEqual(['zview-shell-v1']);
    expect(t.calls.claim).toBe(1);
  });

  it('never touches a cache that is not its own shell cache', async () => {
    const t = makeScope();
    await t.caches.open('someone-else');
    installServiceWorker(t.scope, SHELL, 'v1');
    await t.lifecycle('activate');
    expect(await t.caches.keys()).toContain('someone-else');
  });
});
