import { describe, expect, it } from 'vitest';
import { fullView, MIN_SPAN_MS, panBy, stepPreset, ZOOM_PRESETS_MS, zoomAround } from './zoom';

const H = 3_600_000;
const M = 60_000;
const range: [number, number] = [1_000_000_000_000, 1_000_000_000_000 + 24 * H];

describe('zoom presets', () => {
  it('are 24 h, 6 h, 1 h, 10 min, 1 min', () => {
    expect(ZOOM_PRESETS_MS).toEqual([24 * H, 6 * H, H, 10 * M, M]);
  });
});

describe('fullView', () => {
  it('spans the whole range', () => {
    expect(fullView(range)).toEqual({ startMs: range[0], spanMs: 24 * H });
  });
});

describe('zoomAround', () => {
  it('keeps the anchor instant at the same screen fraction', () => {
    const v = { startMs: range[0], spanMs: 24 * H };
    const anchor = range[0] + 6 * H; // fraction 0.25
    const z = zoomAround(v, 0.25, anchor, range);
    expect(z.spanMs).toBe(6 * H);
    expect((anchor - z.startMs) / z.spanMs).toBeCloseTo(0.25, 9);
  });

  it('clamps to the shortest span (1 min)', () => {
    const z = zoomAround({ startMs: range[0], spanMs: 2 * M }, 0.001, range[0] + M, range);
    expect(z.spanMs).toBe(MIN_SPAN_MS);
  });

  it('clamps to the range length (no zooming out past the day)', () => {
    const z = zoomAround({ startMs: range[0] + 6 * H, spanMs: 6 * H }, 100, range[0] + 9 * H, range);
    expect(z).toEqual({ startMs: range[0], spanMs: 24 * H });
  });

  it('keeps the window inside the range when zooming near an edge', () => {
    const z = zoomAround({ startMs: range[0], spanMs: 24 * H }, 0.01, range[0], range);
    expect(z.startMs).toBeGreaterThanOrEqual(range[0]);
    expect(z.startMs + z.spanMs).toBeLessThanOrEqual(range[1]);
  });

  it('a 23 h day caps the span at 23 h', () => {
    const r: [number, number] = [range[0], range[0] + 23 * H];
    const z = zoomAround({ startMs: r[0], spanMs: 6 * H }, 100, r[0] + 3 * H, r);
    expect(z.spanMs).toBe(23 * H);
  });
});

describe('stepPreset', () => {
  const anchor = range[0] + 12 * H;
  it('zooming in walks 24 h -> 6 h -> 1 h -> 10 min -> 1 min and stops', () => {
    let v = fullView(range);
    const spans: number[] = [];
    for (let i = 0; i < 5; i++) {
      v = stepPreset(v, 'in', anchor, range);
      spans.push(v.spanMs);
    }
    expect(spans).toEqual([6 * H, H, 10 * M, M, M]);
  });

  it('zooming out walks back up and stops at the full day', () => {
    let v = { startMs: anchor, spanMs: M };
    const spans: number[] = [];
    for (let i = 0; i < 5; i++) {
      v = stepPreset(v, 'out', anchor, range);
      spans.push(v.spanMs);
    }
    expect(spans).toEqual([10 * M, H, 6 * H, 24 * H, 24 * H]);
  });

  it('from a between-presets span, in goes to the next smaller preset', () => {
    const v = stepPreset({ startMs: range[0], spanMs: 3 * H }, 'in', range[0] + H, range);
    expect(v.spanMs).toBe(H);
  });

  it('keeps the anchor in view', () => {
    const v = stepPreset(fullView(range), 'in', anchor, range);
    expect(anchor).toBeGreaterThanOrEqual(v.startMs);
    expect(anchor).toBeLessThanOrEqual(v.startMs + v.spanMs);
  });
});

describe('panBy', () => {
  it('moves the window', () => {
    const v = panBy({ startMs: range[0] + H, spanMs: H }, 30 * M, range);
    expect(v.startMs).toBe(range[0] + H + 30 * M);
  });

  it('clamps at both ends of the range', () => {
    const v = { startMs: range[0] + H, spanMs: H };
    expect(panBy(v, -100 * H, range).startMs).toBe(range[0]);
    expect(panBy(v, 100 * H, range).startMs).toBe(range[1] - H);
  });
});
