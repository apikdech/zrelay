import { describe, expect, it } from 'vitest';
import { createIdleLock, type TimerApi } from './idleLock';

function fixture() {
  const doc = Object.assign(new EventTarget(), { hidden: false });
  let now = 0;
  let nextId = 1;
  const timers = new Map<number, { at: number; fn: () => void }>();
  const timer: TimerApi = {
    set: (fn, ms) => {
      const id = nextId++;
      timers.set(id, { at: now + ms, fn });
      return id;
    },
    clear: (id) => void timers.delete(id),
  };
  const advance = (ms: number) => {
    now += ms;
    for (const [id, t] of [...timers]) {
      if (t.at <= now) {
        timers.delete(id);
        t.fn();
      }
    }
  };
  const setHidden = (hidden: boolean) => {
    doc.hidden = hidden;
    doc.dispatchEvent(new Event('visibilitychange'));
  };
  let locks = 0;
  const idle = createIdleLock({ doc, timer, now: () => now, lock: () => void locks++ });
  /** Wall clock moves but no timer runs: a suspended mobile tab. */
  const advanceWithoutTimers = (ms: number) => void (now += ms);
  return { idle, advance, advanceWithoutTimers, setHidden, locks: () => locks, timers };
}

describe('idle auto-lock', () => {
  it('is off by default', () => {
    const f = fixture();
    f.setHidden(true);
    f.advance(24 * 3600_000);
    expect(f.locks()).toBe(0);
  });

  it('locks after the configured minutes with the tab hidden', () => {
    const f = fixture();
    f.idle.setMinutes(5);
    f.setHidden(true);
    f.advance(5 * 60_000 - 1);
    expect(f.locks()).toBe(0);
    f.advance(1);
    expect(f.locks()).toBe(1);
  });

  it('does not lock while the tab is visible, and returning cancels the countdown', () => {
    const f = fixture();
    f.idle.setMinutes(1);
    f.advance(10 * 60_000);
    expect(f.locks()).toBe(0);
    f.setHidden(true);
    f.advance(30_000);
    f.setHidden(false);
    f.advance(10 * 60_000);
    expect(f.locks()).toBe(0);
  });

  it('starts counting immediately when enabled on an already hidden tab', () => {
    const f = fixture();
    f.setHidden(true);
    f.idle.setMinutes(1);
    f.advance(60_000);
    expect(f.locks()).toBe(1);
  });

  it('turning it off cancels a pending countdown, and dispose stops listening', () => {
    const f = fixture();
    f.idle.setMinutes(1);
    f.setHidden(true);
    f.idle.setMinutes(0);
    f.advance(2 * 60_000);
    expect(f.locks()).toBe(0);
    f.idle.setMinutes(1);
    f.idle.dispose();
    f.setHidden(true);
    f.advance(2 * 60_000);
    expect(f.locks()).toBe(0);
    expect(f.timers.size).toBe(0);
  });

  it('locks on return when the timer never fired while the tab was suspended', () => {
    const f = fixture();
    f.idle.setMinutes(5);
    f.setHidden(true);
    f.advanceWithoutTimers(5 * 60_000 + 1); // timers frozen by the OS
    expect(f.locks()).toBe(0);
    f.setHidden(false);
    expect(f.locks()).toBe(1);
    f.advance(10 * 60_000);
    expect(f.locks()).toBe(1); // exactly once
  });

  it('does not lock on return before the deadline, and does not double-lock after the timer fired', () => {
    const f = fixture();
    f.idle.setMinutes(5);
    f.setHidden(true);
    f.advanceWithoutTimers(5 * 60_000 - 1);
    f.setHidden(false);
    expect(f.locks()).toBe(0);
    f.setHidden(true);
    f.advance(5 * 60_000); // timer fires normally
    expect(f.locks()).toBe(1);
    f.setHidden(false);
    expect(f.locks()).toBe(1);
  });
});
