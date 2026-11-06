// Idle auto-lock (ui-spec §4.1): lock after `minutes` with the tab hidden.
// 0 means off. Visible again before the deadline cancels the countdown.

export interface TimerApi {
  set(fn: () => void, ms: number): number;
  clear(id: number): void;
}

export interface VisibilitySource {
  readonly hidden: boolean;
  addEventListener(type: 'visibilitychange', fn: () => void): void;
  removeEventListener(type: 'visibilitychange', fn: () => void): void;
}

export interface IdleLock {
  setMinutes(minutes: number): void;
  dispose(): void;
}

export const realTimer: TimerApi = {
  set: (fn, ms) => setTimeout(fn, ms) as unknown as number,
  clear: (id) => clearTimeout(id),
};

export function createIdleLock(opts: {
  doc: VisibilitySource;
  lock: () => void;
  timer?: TimerApi;
  /** Wall clock in ms; injectable for tests. */
  now?: () => number;
}): IdleLock {
  const { doc, lock } = opts;
  const timer = opts.timer ?? realTimer;
  const now = opts.now ?? Date.now;
  let minutes = 0;
  let pending: number | null = null;
  let hiddenAt = 0;

  const cancel = () => {
    if (pending !== null) timer.clear(pending);
    pending = null;
  };
  const sync = () => {
    // Mobile browsers freeze timers in the background, so a countdown armed on
    // 'hidden' may never have fired. Check the wall clock before dropping it.
    const armed = pending !== null;
    const overdue = armed && !doc.hidden && now() - hiddenAt >= minutes * 60_000;
    cancel();
    if (overdue) {
      lock();
      return;
    }
    if (minutes > 0 && doc.hidden) {
      if (!armed) hiddenAt = now(); // keep the original hide time when only the setting changed
      pending = timer.set(() => {
        pending = null;
        lock();
      }, minutes * 60_000);
    }
  };
  doc.addEventListener('visibilitychange', sync);

  return {
    setMinutes(m) {
      minutes = Number.isFinite(m) && m > 0 ? m : 0;
      sync();
    },
    dispose() {
      cancel();
      doc.removeEventListener('visibilitychange', sync);
    },
  };
}
