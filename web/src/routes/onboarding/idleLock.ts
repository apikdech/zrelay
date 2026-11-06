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

export function createIdleLock(opts: { doc: VisibilitySource; lock: () => void; timer?: TimerApi }): IdleLock {
  const { doc, lock } = opts;
  const timer = opts.timer ?? realTimer;
  let minutes = 0;
  let pending: number | null = null;

  const cancel = () => {
    if (pending !== null) timer.clear(pending);
    pending = null;
  };
  const sync = () => {
    cancel();
    if (minutes > 0 && doc.hidden) {
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
