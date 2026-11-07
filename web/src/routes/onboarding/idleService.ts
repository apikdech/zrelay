// App-level idle auto-lock (ui-spec §4.1). The saved setting must apply on every
// view, so the lock is shared and reference-counted: App holds it for the page's
// lifetime, and the Key page also holds it so it works when mounted on its own.
import type { KeyListResult, Request } from '../../worker/protocol';
import { createIdleLock, type IdleLock } from './idleLock';

export const IDLE_KEY = 'zview.idleLockMinutes';
export const IDLE_CHOICES = [0, 1, 5, 15, 30, 60];

export function savedIdleMinutes(): number {
  try {
    const n = Number(localStorage.getItem(IDLE_KEY));
    return IDLE_CHOICES.includes(n) ? n : 0;
  } catch {
    return 0; // storage blocked: the setting simply does not persist
  }
}

export interface LockClient {
  call<T = unknown>(msg: Request): Promise<T>;
}

export interface IdleListener {
  /** An idle lock reached the worker; `result` is its key.lock reply. */
  onLocked?(result: KeyListResult): void;
  onError?(): void;
}

export interface IdleHandle {
  setMinutes(minutes: number): void;
  release(): void;
}

let lock: IdleLock | null = null;
let holders = 0;
const listeners = new Set<IdleListener>();

async function doLock(client: LockClient) {
  try {
    const result = await client.call<KeyListResult>({ type: 'key.lock' });
    for (const l of [...listeners]) l.onLocked?.(result);
  } catch {
    for (const l of [...listeners]) l.onError?.();
  }
}

/**
 * Takes a hold on the shared idle lock, creating it (armed with the saved
 * setting) if nobody holds it yet. The first holder's client does the locking.
 */
export function acquireIdleLock(client: LockClient, listener: IdleListener = {}): IdleHandle {
  if (!lock) {
    lock = createIdleLock({ doc: document, lock: () => void doLock(client) });
    lock.setMinutes(savedIdleMinutes());
  }
  holders++;
  listeners.add(listener);
  let released = false;
  return {
    setMinutes(minutes) {
      try {
        localStorage.setItem(IDLE_KEY, String(minutes));
      } catch {
        /* not persisted */
      }
      lock?.setMinutes(minutes);
    },
    release() {
      if (released) return;
      released = true;
      listeners.delete(listener);
      if (--holders === 0) {
        lock?.dispose();
        lock = null;
      }
    },
  };
}
