// Pure view arithmetic for the timeline: a view is the visible window
// [startMs, startMs + spanMs), always kept inside the day range.

export interface View {
  startMs: number;
  spanMs: number;
}

const MIN = 60_000;
const HOUR = 3_600_000;

/** Zoom presets, widest to narrowest: 24 h, 6 h, 1 h, 10 min, 1 min. */
export const ZOOM_PRESETS_MS: readonly number[] = [24 * HOUR, 6 * HOUR, HOUR, 10 * MIN, MIN];
export const MIN_SPAN_MS = MIN;

type Range = readonly [number, number];

export function fullView(range: Range): View {
  return { startMs: range[0], spanMs: range[1] - range[0] };
}

function clampView(v: View, range: Range): View {
  const len = range[1] - range[0];
  const spanMs = Math.min(Math.max(v.spanMs, Math.min(MIN_SPAN_MS, len)), len);
  const startMs = Math.min(Math.max(v.startMs, range[0]), range[1] - spanMs);
  return { startMs, spanMs };
}

/** Continuous zoom: multiplies the span by `factor`, keeping `anchorMs` fixed on screen. */
export function zoomAround(v: View, factor: number, anchorMs: number, range: Range): View {
  const len = range[1] - range[0];
  const spanMs = Math.min(Math.max(v.spanMs * factor, MIN_SPAN_MS), len);
  const frac = (anchorMs - v.startMs) / v.spanMs;
  return clampView({ startMs: anchorMs - frac * spanMs, spanMs }, range);
}

/** Jumps to the next preset span in or out of the current one (button, double-click). */
export function stepPreset(v: View, dir: 'in' | 'out', anchorMs: number, range: Range): View {
  const len = range[1] - range[0];
  // The widest preset is the whole day, whatever its length (23/24/25 h).
  const presets = [len, ...ZOOM_PRESETS_MS.slice(1)];
  const eps = 1; // ms tolerance so a span sitting on a preset counts as on it
  const target =
    dir === 'in'
      ? (presets.find((p) => p < v.spanMs - eps) ?? presets[presets.length - 1]!)
      : ([...presets].reverse().find((p) => p > v.spanMs + eps) ?? presets[0]!);
  return zoomAround(v, target / v.spanMs, anchorMs, range);
}

export function panBy(v: View, deltaMs: number, range: Range): View {
  return clampView({ startMs: v.startMs + deltaMs, spanMs: v.spanMs }, range);
}
