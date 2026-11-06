import { describe, expect, it } from 'vitest';
import { backingSize, createScheduler } from './render';

function fakeRaf() {
  let next = 1;
  const pending = new Map<number, () => void>();
  return {
    raf: (cb: () => void) => {
      const id = next++;
      pending.set(id, cb);
      return id;
    },
    cancel: (id: number) => void pending.delete(id),
    flush() {
      const cbs = [...pending.values()];
      pending.clear();
      for (const cb of cbs) cb();
    },
    get pending() {
      return pending.size;
    },
  };
}

describe('createScheduler (redraw on rAF only when dirty)', () => {
  it('does nothing until invalidated', () => {
    const f = fakeRaf();
    let draws = 0;
    createScheduler(() => draws++, f.raf, f.cancel);
    f.flush();
    expect(draws).toBe(0);
    expect(f.pending).toBe(0);
  });

  it('coalesces many invalidations into one draw per frame', () => {
    const f = fakeRaf();
    let draws = 0;
    const s = createScheduler(() => draws++, f.raf, f.cancel);
    s.invalidate();
    s.invalidate();
    s.invalidate();
    expect(f.pending).toBe(1);
    f.flush();
    expect(draws).toBe(1);
    f.flush();
    expect(draws).toBe(1); // clean: no further frame
    expect(f.pending).toBe(0);
  });

  it('an invalidation during a draw schedules the next frame', () => {
    const f = fakeRaf();
    let draws = 0;
    const s = createScheduler(
      () => {
        draws++;
        if (draws === 1) s.invalidate();
      },
      f.raf,
      f.cancel,
    );
    s.invalidate();
    f.flush();
    expect(f.pending).toBe(1);
    f.flush();
    expect(draws).toBe(2);
  });

  it('dispose cancels a pending frame and ignores later invalidations', () => {
    const f = fakeRaf();
    let draws = 0;
    const s = createScheduler(() => draws++, f.raf, f.cancel);
    s.invalidate();
    s.dispose();
    s.invalidate();
    f.flush();
    expect(draws).toBe(0);
  });
});

describe('backingSize (DPR scaling)', () => {
  it('scales the backing store by devicePixelRatio', () => {
    expect(backingSize(300, 100, 2)).toEqual({ width: 600, height: 200 });
    expect(backingSize(333, 101, 1.5)).toEqual({ width: 500, height: 152 });
  });
  it('never goes below 1x or to zero', () => {
    expect(backingSize(300, 100, 0)).toEqual({ width: 300, height: 100 });
    expect(backingSize(0, 0, 2)).toEqual({ width: 1, height: 1 });
  });
});
