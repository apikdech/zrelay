// Service worker (ui-spec §4.9, design §7.2).
//
// It precaches the content-hashed app shell and serves exactly those files from
// Cache Storage. It never intercepts `/api` or `/live`, never stores data, and
// never takes over from the running version by itself: a new version waits
// until the page, on the viewer's click, sends SKIP_WAITING.
import { isDataPath } from './lib/pwa/shell';

/** The slice of ServiceWorkerGlobalScope this worker uses, so tests can drive it. */
export interface SwScope {
  addEventListener(type: 'install' | 'activate' | 'fetch' | 'message', listener: (event: never) => void): void;
  skipWaiting(): Promise<void>;
  clients: { claim(): Promise<void> };
  location: { origin: string };
  caches: CacheStorage;
  fetch(request: Request): Promise<Response>;
}

const CACHE_PREFIX = 'zview-shell-';

interface Extendable {
  waitUntil(p: Promise<unknown>): void;
}
interface FetchLike {
  request: Request;
  respondWith(r: Promise<Response>): void;
}

export function installServiceWorker(scope: SwScope, shell: readonly string[], version: string): void {
  const name = CACHE_PREFIX + version;
  // Defence in depth: a data path never enters the shell, even if a build listed one.
  const files = shell.filter((p) => !isDataPath(p));
  const held = new Set(files);
  const on = <E>(type: Parameters<SwScope['addEventListener']>[0], listener: (e: E) => void): void =>
    scope.addEventListener(type, listener as (event: never) => void);

  on<Extendable>('install', (e) => {
    e.waitUntil(scope.caches.open(name).then((c) => c.addAll(files)));
  });

  on<Extendable>('activate', (e) => {
    e.waitUntil(
      scope.caches
        .keys()
        .then((keys) => Promise.all(keys.filter((k) => k.startsWith(CACHE_PREFIX) && k !== name).map((k) => scope.caches.delete(k))))
        .then(() => scope.clients.claim()),
    );
  });

  on<FetchLike>('fetch', (e) => {
    const req = e.request;
    if (req.method !== 'GET') return;
    const url = new URL(req.url);
    // Anything not named here is left to the browser: pages, the manifest, sw.js itself, /api, /live.
    if (url.origin !== scope.location.origin || isDataPath(url.pathname) || !held.has(url.pathname)) return;
    e.respondWith(
      scope.caches
        .open(name)
        .then((c) => c.match(req))
        .then((hit) => hit ?? scope.fetch(req)),
    );
  });

  on<{ data?: unknown }>('message', (e) => {
    const d = e.data as { type?: unknown } | null;
    if (d && typeof d === 'object' && d.type === 'SKIP_WAITING') void scope.skipWaiting();
  });
}

// The build plugin (vite-sw-plugin.ts) defines these when it compiles this file to /sw.js.
declare const __SHELL__: readonly string[] | undefined;
declare const __SHELL_VERSION__: string | undefined;

// ServiceWorkerGlobalScope is not in the DOM lib this project compiles against; it exists only inside a worker.
const workerScope = (globalThis as { ServiceWorkerGlobalScope?: abstract new () => object }).ServiceWorkerGlobalScope;
if (workerScope && self instanceof workerScope) {
  installServiceWorker(self as unknown as SwScope, typeof __SHELL__ === 'undefined' ? [] : __SHELL__, __SHELL_VERSION__ ?? 'dev');
}
