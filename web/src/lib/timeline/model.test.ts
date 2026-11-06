import { describe, expect, it } from 'vitest';
import {
  gapAt,
  glyphFits,
  hitTest,
  LANE_HEIGHT,
  layoutLanes,
  notMonitored,
  rulerTicks,
  SNAP_PX,
  triggerStyle,
  type Coverage,
  type TimelineCamera,
  type TimelineClip,
} from './model';
import { localDayRange } from './timezone';
import { fullView } from './zoom';

const H = 3_600_000;
const S = 1000;
const range: [number, number] = [0, 24 * H];
const W = 1000; // 24 h over 1000 px: 86.4 s per px

const cams: TimelineCamera[] = [
  { id: 'front', name: 'Front' },
  { id: 'back', name: 'Back' },
];

function clip(
  id: string,
  cameraId: string,
  startMs: number,
  durMs: number,
  triggers: TimelineClip['triggers'] = [],
): TimelineClip {
  return { id, cameraId, startMs, endMs: startMs + durMs, triggers };
}

function layout(clips: TimelineClip[], coverage: Coverage[] = [], extra: { nowMs?: number } = {}) {
  return layoutLanes({ cameras: cams, clips, coverage, range, view: fullView(range), width: W, ...extra });
}

describe('triggerStyle: glyph and color mapping', () => {
  it.each([
    ['person', 'P', '#f97316'],
    ['vehicle', 'V', '#22c55e'],
    ['pet', 'A', '#a855f7'],
    ['motion', 'M', '#3b82f6'],
    ['other', '•', '#9ca3af'],
  ])('%s -> %s %s', (type, glyph, color) => {
    expect(triggerStyle(type)).toMatchObject({ glyph, color });
  });

  it('an unknown trigger type is drawn as other', () => {
    expect(triggerStyle('doorbell')).toMatchObject({ glyph: '•', color: '#9ca3af' });
  });
});

describe('glyphFits', () => {
  it('draws a glyph only when the trigger span is wide enough', () => {
    expect(glyphFits(3)).toBe(false);
    expect(glyphFits(40)).toBe(true);
  });
});

describe('layoutLanes', () => {
  it('makes one lane per camera in order and places clips by camera', () => {
    const l = layout([clip('a', 'back', 12 * H, 60 * S), clip('b', 'front', 6 * H, 60 * S), clip('x', 'ghost', H, 60 * S)]);
    expect(l.lanes.map((x) => x.cameraId)).toEqual(['front', 'back']);
    expect(l.lanes[0]!.clips.map((c) => c.clipId)).toEqual(['b']);
    expect(l.lanes[1]!.clips.map((c) => c.clipId)).toEqual(['a']);
    expect(l.lanes[1]!.y).toBeGreaterThan(l.lanes[0]!.y + LANE_HEIGHT - 1);
  });

  it('maps time to x linearly across the view', () => {
    const l = layout([clip('a', 'front', 12 * H, 30 * 60 * S)]);
    const c = l.lanes[0]!.clips[0]!;
    expect(c.x0).toBeCloseTo(500, 6);
    expect(c.x1 - c.x0).toBeCloseTo(((30 * 60 * S) / (24 * H)) * W, 6);
  });

  it('keeps a very short clip visible at 24 h zoom', () => {
    const c = layout([clip('a', 'front', 12 * H, 5 * S)]).lanes[0]!.clips[0]!;
    expect(c.x1 - c.x0).toBeGreaterThanOrEqual(2);
  });

  it('omits clips outside the visible window', () => {
    const view = { startMs: 0, spanMs: H };
    const l = layoutLanes({
      cameras: cams,
      clips: [clip('in', 'front', 30 * 60 * S, 60 * S), clip('out', 'front', 5 * H, 60 * S)],
      coverage: [],
      range,
      view,
      width: W,
    });
    expect(l.lanes[0]!.clips.map((c) => c.clipId)).toEqual(['in']);
  });

  it('lays trigger spans from clip start + first_at_ms .. last_at_ms with their style', () => {
    const l = layout([
      clip('a', 'front', 12 * H, 3 * H, [
        { type: 'person', firstAtMs: 20 * 60 * S, lastAtMs: 40 * 60 * S },
        { type: 'vehicle', firstAtMs: 60 * 60 * S, lastAtMs: 90 * 60 * S },
      ]),
    ]);
    const c = l.lanes[0]!.clips[0]!;
    expect(c.triggers).toHaveLength(2);
    expect(c.triggers[0]).toMatchObject({ type: 'person', glyph: 'P', color: '#f97316' });
    expect(c.triggers[0]!.x0).toBeCloseTo(((12 * H + 20 * 60 * S) / (24 * H)) * W, 6);
    expect(c.triggers[0]!.x1).toBeCloseTo(((12 * H + 40 * 60 * S) / (24 * H)) * W, 6);
    expect(c.triggers[1]).toMatchObject({ type: 'vehicle', glyph: 'V', color: '#22c55e' });
  });

  it('the seek offset is the earliest trigger first_at_ms, or 0 without triggers', () => {
    const l = layout([
      clip('t', 'front', H, 600 * S, [
        { type: 'motion', firstAtMs: 15_800, lastAtMs: 32_400 },
        { type: 'person', firstAtMs: 9_000, lastAtMs: 29_100 },
      ]),
      clip('n', 'front', 3 * H, 600 * S),
    ]);
    expect(l.lanes[0]!.clips.map((c) => [c.clipId, c.offsetMs])).toEqual([
      ['t', 9_000],
      ['n', 0],
    ]);
  });
});

describe('notMonitored (coverage complement)', () => {
  it('is the day minus the ready intervals, merging unsorted/overlapping input', () => {
    const gaps = notMonitored(
      [
        [5 * H, 6 * H],
        [2 * H, 3 * H],
        [2.5 * H, 3.5 * H],
      ],
      0,
      24 * H,
    );
    expect(gaps).toEqual([
      [0, 2 * H],
      [3.5 * H, 5 * H],
      [6 * H, 24 * H],
    ]);
  });

  it('no coverage at all means the whole range is not monitored', () => {
    expect(notMonitored([], 0, 24 * H)).toEqual([[0, 24 * H]]);
  });

  it('clips intervals that stick out of the range (the +-1 h padding)', () => {
    expect(
      notMonitored(
        [
          [-H, 3 * H],
          [23 * H, 25 * H],
        ],
        0,
        24 * H,
      ),
    ).toEqual([[3 * H, 23 * H]]);
  });

  it('full coverage leaves no gaps', () => {
    expect(notMonitored([[0, 24 * H]], 0, 24 * H)).toEqual([]);
  });
});

describe('layoutLanes: coverage gaps', () => {
  const cov: Coverage[] = [
    { cameraId: 'front', fromMs: 2 * H, toMs: 3 * H },
    { cameraId: 'front', fromMs: 5 * H, toMs: 6 * H },
    { cameraId: 'back', fromMs: 0, toMs: 24 * H },
  ];

  it('shades the complement of ready coverage per camera', () => {
    const l = layout([], cov);
    const g = l.lanes[0]!.gaps;
    expect(g.map((x) => [x.fromMs, x.toMs])).toEqual([
      [0, 2 * H],
      [3 * H, 5 * H],
      [6 * H, 24 * H],
    ]);
    expect(g[0]!.x0).toBeCloseTo(0, 6);
    expect(g[0]!.x1).toBeCloseTo((2 / 24) * W, 6);
    expect(l.lanes[1]!.gaps).toEqual([]);
  });

  it('does not call the future "not monitored" when nowMs is given', () => {
    const l = layout([], cov, { nowMs: 10 * H });
    expect(l.lanes[0]!.gaps.at(-1)).toMatchObject({ fromMs: 6 * H, toMs: 10 * H });
  });

  it('gapAt finds the not-monitored region under the pointer', () => {
    const l = layout([], cov);
    const y = l.lanes[0]!.y + 5;
    expect(gapAt(l, (4 / 24) * W, y)).toMatchObject({ cameraId: 'front', fromMs: 3 * H, toMs: 5 * H });
    expect(gapAt(l, (2.5 / 24) * W, y)).toBeNull();
    expect(gapAt(l, (4 / 24) * W, l.lanes[1]!.y + 5)).toBeNull();
  });
});

describe('hitTest', () => {
  const l = layout([
    clip('a', 'front', 12 * H, 30 * 60 * S, [{ type: 'person', firstAtMs: 42 * S, lastAtMs: 90 * S }]),
    clip('b', 'front', 18 * H, 30 * 60 * S),
  ]);
  const lane = l.lanes[0]!;
  const a = lane.clips[0]!;
  const midY = lane.y + lane.height / 2;

  it('a click on a clip lands on its first trigger offset', () => {
    expect(hitTest(l, (a.x0 + a.x1) / 2, midY)).toEqual({ clipId: 'a', offsetMs: 42 * S });
  });

  it('snaps to the nearest clip within 24 px of empty space', () => {
    expect(SNAP_PX).toBe(24);
    expect(hitTest(l, a.x1 + 23, midY)).toEqual({ clipId: 'a', offsetMs: 42 * S });
    expect(hitTest(l, a.x0 - 23, midY)).toEqual({ clipId: 'a', offsetMs: 42 * S });
  });

  it('misses beyond 24 px', () => {
    expect(hitTest(l, a.x1 + 25, midY)).toBeNull();
  });

  it('a clip with no triggers seeks to 0', () => {
    const b = lane.clips[1]!;
    expect(hitTest(l, (b.x0 + b.x1) / 2, midY)).toEqual({ clipId: 'b', offsetMs: 0 });
  });

  it('picks the nearer of two clips', () => {
    const b = lane.clips[1]!;
    expect(hitTest(l, b.x0 - 5, midY)?.clipId).toBe('b');
  });

  it('a click in another lane does not hit this lane\'s clip beyond the snap distance', () => {
    expect(hitTest(l, (a.x0 + a.x1) / 2, l.lanes[1]!.y + l.lanes[1]!.height - 1)).toBeNull();
  });

  it('a very short clip is still hit at its pixel position', () => {
    const l2 = layout([clip('s', 'front', 12 * H, 2 * S)]);
    const c = l2.lanes[0]!.clips[0]!;
    expect(hitTest(l2, c.x0 + 1, l2.lanes[0]!.y + 10)?.clipId).toBe('s');
  });
});

describe('rulerTicks', () => {
  it('24 h view: 2-hourly local ticks in the forced zone', () => {
    const r = localDayRange('2026-11-06', 'Asia/Jakarta');
    const ticks = rulerTicks(fullView(r), 1000, 'Asia/Jakarta');
    expect(ticks.map((t) => t.label)).toEqual(['00:00', '02:00', '04:00', '06:00', '08:00', '10:00', '12:00', '14:00', '16:00', '18:00', '20:00', '22:00']);
    expect(ticks[0]!.ms).toBe(r[0]);
  });

  it('a 1 min view has second-resolution ticks', () => {
    const start = Date.UTC(2026, 10, 6, 3, 0, 0);
    const ticks = rulerTicks({ startMs: start, spanMs: 60 * S }, 1000, 'UTC');
    expect(ticks[0]!.label).toBe('03:00:00');
    expect(ticks[1]!.ms - ticks[0]!.ms).toBe(5 * S);
  });

  it('aligns to local wall time in half-hour zones', () => {
    const r = localDayRange('2026-11-06', 'Asia/Kolkata');
    const ticks = rulerTicks({ startMs: r[0], spanMs: 2 * H }, 1000, 'Asia/Kolkata');
    expect(ticks[0]!.label).toBe('00:00');
  });
});
