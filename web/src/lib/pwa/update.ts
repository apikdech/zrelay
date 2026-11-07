// Page side of the service worker update flow (ui-spec §4.9): a new version
// installs and waits; the viewer is told, and only their click lets it take
// over. Waiting is never skipped while a clip plays, and the page reloads only
// for a takeover it asked for, never because another tab updated.

export interface WorkerLike extends EventTarget {
  state: string;
  postMessage(msg: unknown): void;
}
export interface RegistrationLike extends EventTarget {
  waiting: WorkerLike | null;
  installing: WorkerLike | null;
}
export interface ContainerLike extends EventTarget {
  controller: object | null;
}

export interface Updater {
  /** A new version is installed and waiting. */
  readonly available: boolean;
  onChange(cb: () => void): () => void;
  /** Lets the waiting version take over and reloads once it has. False when nothing waits or a clip is playing. */
  apply(): boolean;
}

export interface UpdaterOptions {
  container: ContainerLike;
  registration: RegistrationLike;
  isPlaying: () => boolean;
  reload: () => void;
}

export function createUpdater(o: UpdaterOptions): Updater {
  const subs = new Set<() => void>();
  // Without a controller this is the first install, not an update.
  let waiting: WorkerLike | null = o.container.controller !== null ? o.registration.waiting : null;
  let asked = false;

  const notify = () => subs.forEach((cb) => cb());

  o.registration.addEventListener('updatefound', () => {
    const w = o.registration.installing;
    if (!w) return;
    w.addEventListener('statechange', () => {
      if (w.state === 'installed' && o.container.controller !== null) {
        waiting = w;
        notify();
      }
    });
  });

  return {
    get available() {
      return waiting !== null;
    },
    onChange(cb) {
      subs.add(cb);
      return () => subs.delete(cb);
    },
    apply() {
      if (!waiting || o.isPlaying()) return false;
      if (!asked) {
        asked = true;
        o.container.addEventListener('controllerchange', () => o.reload(), { once: true });
      }
      waiting.postMessage({ type: 'SKIP_WAITING' });
      return true;
    },
  };
}

/**
 * Whether a clip is playing. Only clip-playback videos (marked data-clip-player)
 * count: live tiles autoplay muted with no pause control, so counting them would
 * hold the Reload button for as long as the Live view is open.
 */
export function anyVideoPlaying(doc: Document): boolean {
  return [...doc.querySelectorAll('video[data-clip-player]')].some((v) => v instanceof HTMLVideoElement && !v.paused && !v.ended);
}
