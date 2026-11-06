// Pure timeline model: no DOM, no canvas. Turns clips, coverage and a view
// window into pixel geometry (layoutLanes) and answers pointer queries
// (hitTest, gapAt). Rendering lives in render.ts.
import { formatClock, zoneOffsetMs } from './timezone';
import type { View } from './zoom';

export interface TimelineCamera {
  id: string;
  name: string;
}

export interface TimelineTrigger {
  /** `motion` | `person` | `vehicle` | `pet` | `other`; unknown types render as other. */
  type: string;
  /** Offsets from the clip start (Contract §7.5). */
  firstAtMs: number;
  lastAtMs: number;
}

export interface TimelineClip {
  id: string;
  cameraId: string;
  /** Epoch ms. Callers substitute `start + assumed_max_clip_seconds` for a null end. */
  startMs: number;
  endMs: number;
  triggers: TimelineTrigger[];
}

/** One `ready` interval of a camera (zrec.coverage/1): the recorder could have caught an event. */
export interface Coverage {
  cameraId: string;
  fromMs: number;
  toMs: number;
}

// Geometry (CSS px).
export const RULER_HEIGHT = 28;
export const LANE_HEIGHT = 44;
export const LANE_GAP = 6;
export const BAR_INSET = 6;
/** Bars and trigger spans never get thinner than this, so 20 s clips stay visible at 24 h zoom. */
export const MIN_CLIP_PX = 3;
/** Click-to-nearest-clip snap distance (ui-spec §4.6). */
export const SNAP_PX = 24;
/** Smallest trigger span (px) that still has room for its glyph. */
export const GLYPH_MIN_PX = 12;

export interface TriggerStyle {
  glyph: string;
  color: string;
  label: string;
}

const STYLES: Record<string, TriggerStyle> = {
  person: { glyph: 'P', color: '#f97316', label: 'Person' },
  vehicle: { glyph: 'V', color: '#22c55e', label: 'Vehicle' },
  pet: { glyph: 'A', color: '#a855f7', label: 'Pet' },
  motion: { glyph: 'M', color: '#3b82f6', label: 'Motion' },
  other: { glyph: '•', color: '#9ca3af', label: 'Other' },
};

export function triggerStyle(type: string): TriggerStyle {
  return Object.hasOwn(STYLES, type) ? STYLES[type]! : STYLES.other!;
}

export function glyphFits(widthPx: number): boolean {
  return widthPx >= GLYPH_MIN_PX;
}

export interface TriggerRect {
  type: string;
  glyph: string;
  color: string;
  x0: number;
  x1: number;
}

export interface ClipRect {
  clipId: string;
  x0: number;
  x1: number;
  y: number;
  h: number;
  startMs: number;
  endMs: number;
  /** Seek target on open: the earliest trigger `first_at_ms`, or 0. */
  offsetMs: number;
  triggers: TriggerRect[];
}

export interface GapRect {
  cameraId: string;
  fromMs: number;
  toMs: number;
  x0: number;
  x1: number;
}

export interface Lane {
  cameraId: string;
  name: string;
  y: number;
  height: number;
  clips: ClipRect[];
  gaps: GapRect[];
}

export interface Layout {
  width: number;
  height: number;
  view: View;
  lanes: Lane[];
}

export interface LayoutInput {
  cameras: readonly TimelineCamera[];
  clips: readonly TimelineClip[];
  coverage: readonly Coverage[];
  /** The day [from, to): the extent that can be "not monitored". */
  range: readonly [number, number];
  view: View;
  /** Plot width in CSS px. */
  width: number;
  /** When set, time after it is the future, not a coverage gap. */
  nowMs?: number;
}

export function timeToX(view: View, width: number, ms: number): number {
  return ((ms - view.startMs) / view.spanMs) * width;
}

export function xToTime(view: View, width: number, x: number): number {
  return view.startMs + (x / width) * view.spanMs;
}

type Interval = readonly [number, number];

/** The part of [from, to) not covered by `ready`, merging unsorted and overlapping input. */
export function notMonitored(ready: readonly Interval[], from: number, to: number): [number, number][] {
  const sorted = ready
    .map(([a, b]) => [Math.max(a, from), Math.min(b, to)] as const)
    .filter(([a, b]) => b > a)
    .sort((p, q) => p[0] - q[0]);
  const gaps: [number, number][] = [];
  let cursor = from;
  for (const [a, b] of sorted) {
    if (a > cursor) gaps.push([cursor, a]);
    if (b > cursor) cursor = b;
  }
  if (cursor < to) gaps.push([cursor, to]);
  return gaps;
}

export function layoutLanes(input: LayoutInput): Layout {
  const { view, width } = input;
  const byCamera = new Map<string, TimelineClip[]>();
  for (const c of input.clips) {
    let list = byCamera.get(c.cameraId);
    if (!list) byCamera.set(c.cameraId, (list = []));
    list.push(c);
  }
  const readyBy = new Map<string, Interval[]>();
  for (const c of input.coverage) {
    let list = readyBy.get(c.cameraId);
    if (!list) readyBy.set(c.cameraId, (list = []));
    list.push([c.fromMs, c.toMs]);
  }
  const gapEnd = input.nowMs === undefined ? input.range[1] : Math.min(input.range[1], input.nowMs);
  const viewEnd = view.startMs + view.spanMs;

  const lanes = input.cameras.map((cam, i): Lane => {
    const y = RULER_HEIGHT + i * (LANE_HEIGHT + LANE_GAP);
    const barY = y + BAR_INSET;
    const barH = LANE_HEIGHT - 2 * BAR_INSET;

    const clips: ClipRect[] = [];
    for (const c of byCamera.get(cam.id) ?? []) {
      if (c.endMs < view.startMs || c.startMs > viewEnd) continue;
      const x0 = timeToX(view, width, c.startMs);
      const x1 = Math.max(timeToX(view, width, c.endMs), x0 + MIN_CLIP_PX);
      if (x1 < 0 || x0 > width) continue;
      let offsetMs = Infinity;
      const triggers: TriggerRect[] = c.triggers.map((t) => {
        offsetMs = Math.min(offsetMs, t.firstAtMs);
        const s = triggerStyle(t.type);
        const tx0 = timeToX(view, width, c.startMs + t.firstAtMs);
        const tx1 = Math.max(timeToX(view, width, c.startMs + t.lastAtMs), tx0 + MIN_CLIP_PX);
        return { type: t.type, glyph: s.glyph, color: s.color, x0: tx0, x1: tx1 };
      });
      clips.push({
        clipId: c.id,
        x0,
        x1,
        y: barY,
        h: barH,
        startMs: c.startMs,
        endMs: c.endMs,
        offsetMs: Number.isFinite(offsetMs) ? offsetMs : 0,
        triggers,
      });
    }

    const gaps: GapRect[] = [];
    if (gapEnd > input.range[0]) {
      for (const [fromMs, toMs] of notMonitored(readyBy.get(cam.id) ?? [], input.range[0], gapEnd)) {
        if (toMs <= view.startMs || fromMs >= viewEnd) continue;
        gaps.push({
          cameraId: cam.id,
          fromMs,
          toMs,
          x0: Math.max(0, timeToX(view, width, fromMs)),
          x1: Math.min(width, timeToX(view, width, toMs)),
        });
      }
    }
    return { cameraId: cam.id, name: cam.name, y, height: LANE_HEIGHT, clips, gaps };
  });

  const height = RULER_HEIGHT + Math.max(input.cameras.length, 1) * (LANE_HEIGHT + LANE_GAP);
  return { width, height, view, lanes };
}

export interface Hit {
  clipId: string;
  offsetMs: number;
}

/**
 * The clip a pointer at (x, y) selects: the clip bar nearest the pointer, if
 * within SNAP_PX. A click on a bar has distance 0; a click on empty space
 * snaps to the nearest bar. The offset is the clip's first trigger offset.
 */
export function hitTest(layout: Layout, x: number, y: number): Hit | null {
  let best: ClipRect | null = null;
  let bestD = Infinity;
  for (const lane of layout.lanes) {
    for (const c of lane.clips) {
      const dx = x < c.x0 ? c.x0 - x : x > c.x1 ? x - c.x1 : 0;
      const dy = y < c.y ? c.y - y : y > c.y + c.h ? y - (c.y + c.h) : 0;
      const d = Math.hypot(dx, dy);
      if (d <= SNAP_PX && d < bestD) {
        best = c;
        bestD = d;
      }
    }
  }
  return best ? { clipId: best.clipId, offsetMs: best.offsetMs } : null;
}

/** The "not monitored" region under (x, y), for the hover label. */
export function gapAt(layout: Layout, x: number, y: number): GapRect | null {
  for (const lane of layout.lanes) {
    if (y < lane.y || y > lane.y + lane.height) continue;
    for (const g of lane.gaps) if (x >= g.x0 && x <= g.x1) return g;
  }
  return null;
}

export interface Tick {
  ms: number;
  label: string;
}

const SEC = 1000;
const MINUTE = 60 * SEC;
const HOUR = 60 * MINUTE;
const TICK_STEPS = [
  1 * SEC, 2 * SEC, 5 * SEC, 10 * SEC, 15 * SEC, 30 * SEC,
  1 * MINUTE, 2 * MINUTE, 5 * MINUTE, 10 * MINUTE, 15 * MINUTE, 30 * MINUTE,
  1 * HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR,
];

/** Ruler ticks aligned to wall-clock multiples in `zone`, at least `minSpacingPx` apart. */
export function rulerTicks(view: View, width: number, zone: string, minSpacingPx = 80): Tick[] {
  const want = (minSpacingPx / width) * view.spanMs;
  const step = TICK_STEPS.find((s) => s >= want) ?? TICK_STEPS[TICK_STEPS.length - 1]!;
  const off0 = zoneOffsetMs(view.startMs, zone);
  const end = view.startMs + view.spanMs;
  const ticks: Tick[] = [];
  for (let wall = Math.ceil((view.startMs + off0) / step) * step; ; wall += step) {
    // Re-derive the offset per tick so a DST change inside the view stays aligned.
    const ms = wall - zoneOffsetMs(wall - off0, zone);
    if (ms >= end) break;
    if (ms < view.startMs) continue;
    ticks.push({ ms, label: formatClock(ms, zone, step < MINUTE) });
  }
  return ticks;
}
