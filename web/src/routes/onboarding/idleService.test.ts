// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Request } from '../../worker/protocol';
import { acquireIdleLock } from './idleService';

const setHidden = (hidden: boolean) => {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event('visibilitychange'));
};

function client() {
  const calls: Request[] = [];
  return {
    calls,
    client: {
      async call<T>(msg: Request): Promise<T> {
        calls.push(msg);
        return { keys: [] } as T;
      },
    },
  };
}

beforeEach(() => {
  localStorage.clear();
  vi.useFakeTimers();
});
afterEach(() => {
  setHidden(false);
  vi.useRealTimers();
});

describe('shared idle lock', () => {
  it('keeps locking after the Key page that set the time has gone', async () => {
    const app = client();
    const appHold = acquireIdleLock(app.client);
    const keyPage = acquireIdleLock(client().client);
    keyPage.setMinutes(1);
    keyPage.release();
    setHidden(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(app.calls).toEqual([{ type: 'key.lock' }]);
    appHold.release();
  });

  it('tells a listener that an idle lock happened, once', async () => {
    const { client: c } = client();
    const onLocked = vi.fn();
    const hold = acquireIdleLock(c, { onLocked });
    hold.setMinutes(1);
    setHidden(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(onLocked).toHaveBeenCalledTimes(1);
    hold.release();
  });

  it('stops listening to visibility once every holder has released', async () => {
    const { client: c, calls } = client();
    localStorage.setItem('zview.idleLockMinutes', '1');
    acquireIdleLock(c).release();
    setHidden(true);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(calls).toEqual([]);
  });

  it('reports a failing lock to listeners without an unhandled rejection', async () => {
    const onError = vi.fn();
    const hold = acquireIdleLock({ call: () => Promise.reject(new Error('boom')) }, { onError });
    hold.setMinutes(1);
    setHidden(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(onError).toHaveBeenCalledTimes(1);
    hold.release();
  });
});
