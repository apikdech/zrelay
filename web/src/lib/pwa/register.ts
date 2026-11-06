// Registers /sw.js and hands back the update state for the prompt. Only the
// production build ships a service worker, so development never has one.
import { anyVideoPlaying, createUpdater, type Updater } from './update';

export function registerServiceWorker(): Updater | null {
  if (!import.meta.env.PROD || !('serviceWorker' in navigator)) return null;
  const container = navigator.serviceWorker;

  const subs = new Set<() => void>();
  let inner: Updater | null = null;
  const updater: Updater = {
    get available() {
      return inner?.available ?? false;
    },
    onChange(cb) {
      subs.add(cb);
      return () => subs.delete(cb);
    },
    apply: () => inner?.apply() ?? false,
  };

  const start = () => {
    container
      .register('/sw.js', { scope: '/' })
      .then((registration) => {
        inner = createUpdater({
          container,
          registration,
          isPlaying: () => anyVideoPlaying(document),
          reload: () => location.reload(),
        });
        inner.onChange(() => subs.forEach((cb) => cb()));
        subs.forEach((cb) => cb()); // a version may already be waiting
        // Look for a new version whenever the app comes back to the foreground.
        document.addEventListener('visibilitychange', () => {
          if (document.visibilityState === 'visible') void registration.update().catch(() => {});
        });
      })
      .catch((e: unknown) => {
        // No service worker: the app still works, just without the shell cache.
        console.warn('service worker registration failed', e instanceof Error ? e.message : '');
      });
  };
  if (document.readyState === 'complete') start();
  else window.addEventListener('load', start, { once: true });
  return updater;
}
