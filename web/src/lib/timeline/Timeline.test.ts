// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { untrack } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import Timeline from './Timeline.svelte';
import { drawTimeline } from './render';
import { localDayRange } from './timezone';

// Draw synchronously on every invalidation and record what is drawn, so the
// component's view state can be read through getLayout() without a real canvas.
// (untrack: the real scheduler draws in a later frame, outside any effect.)
vi.mock('./render', async (orig) => ({
  ...(await orig<typeof import('./render')>()),
  createScheduler: (draw: () => void) => ({ invalidate: () => untrack(draw), dispose() {} }),
  drawTimeline: vi.fn(),
}));

class FakeResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

const ZONE = 'Asia/Jakarta';
const DAY = '2026-11-06';
const [FROM, TO] = localDayRange(DAY, ZONE);
const HOUR = 3_600_000;
const MIN = 60_000;
// 22:00 in Jakarta, well inside the day.
const NOW = Date.UTC(2026, 10, 6, 15, 0, 0);

beforeEach(() => {
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
  HTMLCanvasElement.prototype.getContext = (() => ({ setTransform() {} })) as never;
  Object.defineProperty(HTMLCanvasElement.prototype, 'clientWidth', { get: () => 1000, configurable: true });
  vi.mocked(drawTimeline).mockClear();
});
afterEach(cleanup);

const props = (over: Record<string, unknown> = {}) => ({
  cameras: [{ id: 'front-door', name: 'Front Door' }],
  clips: [
    { id: 'a', cameraId: 'front-door', startMs: NOW - 3 * HOUR, endMs: NOW - 3 * HOUR + MIN, triggers: [] },
    { id: 'b', cameraId: 'front-door', startMs: NOW - 2 * HOUR, endMs: NOW - 2 * HOUR + MIN, triggers: [] },
  ],
  coverage: [],
  day: DAY,
  zone: ZONE,
  nowMs: NOW,
  onSelect: () => {},
  ...over,
});

type Api = { stepZoom(d: 'in' | 'out'): void; jumpToNow(): void; getLayout(): { view: { startMs: number; spanMs: number } } };
const mount = (over: Record<string, unknown> = {}) => {
  const r = render(Timeline, props(over));
  const api = r.component as unknown as Api;
  return { ...r, api, view: () => api.getLayout().view };
};

describe('Timeline.stepZoom', () => {
  it('starts on the whole day', () => {
    const { view } = mount();
    expect(view()).toEqual({ startMs: FROM, spanMs: TO - FROM });
  });

  it('steps in through the presets 24 h, 6 h, 1 h, 10 min, 1 min and stays at 1 min', () => {
    const { api, view } = mount();
    const spans: number[] = [];
    for (let i = 0; i < 5; i++) {
      api.stepZoom('in');
      spans.push(view().spanMs);
    }
    expect(spans).toEqual([6 * HOUR, HOUR, 10 * MIN, MIN, MIN]);
  });

  it('steps back out through the presets and stops at the whole day', () => {
    const { api, view } = mount();
    for (let i = 0; i < 4; i++) api.stepZoom('in');
    const spans: number[] = [];
    for (let i = 0; i < 5; i++) {
      api.stepZoom('out');
      spans.push(view().spanMs);
    }
    expect(spans).toEqual([10 * MIN, HOUR, 6 * HOUR, TO - FROM, TO - FROM]);
    expect(view().startMs).toBe(FROM);
  });

  it('zooms around the current time while it is on screen', () => {
    const { api, view } = mount();
    api.stepZoom('in');
    const v = view();
    expect(NOW).toBeGreaterThanOrEqual(v.startMs);
    expect(NOW).toBeLessThanOrEqual(v.startMs + v.spanMs);
    // The now line keeps its relative place on screen.
    expect((NOW - v.startMs) / v.spanMs).toBeCloseTo((NOW - FROM) / (TO - FROM), 6);
  });

  it('zooms around the middle of the view when the current time is not on this day', () => {
    const { api, view } = mount({ nowMs: TO + 5 * HOUR });
    api.stepZoom('in');
    const v = view();
    expect(v.spanMs).toBe(6 * HOUR);
    expect(v.startMs + v.spanMs / 2).toBeCloseTo((FROM + TO) / 2, 0);
  });

  it('repaints after zooming', () => {
    const { api } = mount();
    vi.mocked(drawTimeline).mockClear();
    api.stepZoom('in');
    expect(drawTimeline).toHaveBeenCalled();
  });
});

describe('Timeline.jumpToNow', () => {
  const panAway = (canvas: HTMLElement, key: 'ArrowLeft' | 'ArrowRight', times: number) => {
    for (let i = 0; i < times; i++) fireEvent.keyDown(canvas, { key });
  };

  it('centres the view on the current time', async () => {
    const { api, view } = mount();
    for (let i = 0; i < 3; i++) api.stepZoom('in'); // 10 min around now
    panAway(screen.getByRole('application'), 'ArrowLeft', 30); // three spans earlier
    expect(NOW).toBeGreaterThan(view().startMs + view().spanMs);
    api.jumpToNow();
    expect(view().spanMs).toBe(10 * MIN);
    expect(view().startMs + view().spanMs / 2).toBe(NOW);
  });

  it('clamps to the end of the day when now is near it', () => {
    const late = TO - 2 * MIN; // 23:58 local
    const { api, view } = mount({ nowMs: late });
    for (let i = 0; i < 3; i++) api.stepZoom('in');
    panAway(screen.getByRole('application'), 'ArrowLeft', 30);
    api.jumpToNow();
    expect(view().startMs + view().spanMs).toBe(TO);
  });

  it('clamps to the start of the day when now is before it', () => {
    const { api, view } = mount({ nowMs: FROM - 5 * HOUR });
    for (let i = 0; i < 3; i++) api.stepZoom('in');
    panAway(screen.getByRole('application'), 'ArrowRight', 30);
    api.jumpToNow();
    expect(view().startMs).toBe(FROM);
  });
});

describe('Timeline selectedClipId', () => {
  const lastSelected = () => vi.mocked(drawTimeline).mock.calls.at(-1)![2].selectedClipId;

  it('is passed to the renderer', () => {
    mount({ selectedClipId: 'b' });
    expect(lastSelected()).toBe('b');
  });

  it('follows a selection made elsewhere', async () => {
    const { rerender } = mount({ selectedClipId: 'a' });
    expect(lastSelected()).toBe('a');
    await rerender(props({ selectedClipId: 'b' }));
    expect(lastSelected()).toBe('b');
  });

  it('clears the highlight when the selection is cleared', async () => {
    const { rerender } = mount({ selectedClipId: 'a' });
    await rerender(props({ selectedClipId: null }));
    expect(lastSelected()).toBeNull();
  });

  it('draws nothing selected when the prop is not given', () => {
    mount();
    expect(lastSelected()).toBeNull();
  });
});
