// Pure planners for the playback engine (ui-spec §4.3): which profile applies,
// when to fetch more, what to evict, and where a seek lands. No DOM, no I/O.
import { seekToChunk, type SeekEntry } from '../zrec/seek';

export type Profile = 'mobile' | 'desktop';

/** A buffered time range in seconds, `[start, end)`. */
export type Range = readonly [start: number, end: number];

export interface BufferWindow {
  aheadSeconds: number;
  behindSeconds: number;
}

/** ui-spec §4.3.2 buffer window table. */
export const WINDOWS: Readonly<Record<Profile, BufferWindow>> = {
  mobile: { aheadSeconds: 30, behindSeconds: 15 },
  desktop: { aheadSeconds: 90, behindSeconds: 30 },
};

/** ui-spec §4.3.3: largest plaintext the Blob fallback accepts. */
export const BLOB_MAX: Readonly<Record<Profile, number>> = { mobile: 48 << 20, desktop: 256 << 20 };
/** ui-spec §4.3.3: how many Blob URLs stay alive. */
export const BLOB_LRU: Readonly<Record<Profile, number>> = { mobile: 1, desktop: 3 };

export interface ProfileEnv {
  /** Primary pointer is touch. */
  touch: boolean;
  /** Viewport is narrow (a phone, not a laptop). */
  narrow: boolean;
  /** `navigator.deviceMemory` in GiB when the browser reports it. */
  deviceMemory?: number;
}

/** Mobile = touch + narrow viewport, or `deviceMemory ≤ 4` (ui-spec §4.3.2). */
export function chooseProfile(env: ProfileEnv): Profile {
  if (env.touch && env.narrow) return 'mobile';
  if (env.deviceMemory !== undefined && env.deviceMemory <= 4) return 'mobile';
  return 'desktop';
}

/** Reads the current browser's profile. */
export function detectProfile(): Profile {
  const nav = navigator as Navigator & { deviceMemory?: number };
  const media = (q: string): boolean => (typeof matchMedia === 'function' ? matchMedia(q).matches : false);
  return chooseProfile({
    touch: media('(pointer: coarse)') || nav.maxTouchPoints > 0,
    narrow: media('(max-width: 820px)'),
    deviceMemory: nav.deviceMemory,
  });
}

/** The first frame of a fragment rarely sits at exactly 0.000 s. */
const START_SLACK = 0.25;

/** The range holding `t` (allowing a hair before its start), or undefined. */
function holding(ranges: readonly Range[], t: number): Range | undefined {
  return ranges.find(([s, e]) => t >= s - START_SLACK && t < e);
}

/** True when `t` is inside a buffered range. */
export const isBuffered = (ranges: readonly Range[], t: number): boolean => holding(ranges, t) !== undefined;

/** Seconds buffered from `t` to the end of its range; 0 when `t` is not buffered. */
export function bufferedAhead(ranges: readonly Range[], t: number): number {
  const r = holding(ranges, t);
  return r ? r[1] - t : 0;
}

/** ui-spec §4.3.1 step 3: keep fetching while `buffered.end − currentTime < ahead_seconds`. */
export function needsMore(ranges: readonly Range[], t: number, win: BufferWindow): boolean {
  return bufferedAhead(ranges, t) < win.aheadSeconds;
}

export interface Eviction {
  /** Ranges older than `t − behind_seconds`. */
  behind: Range[];
  /** Ranges newer than `t + ahead_seconds`. */
  ahead: Range[];
}

/** ui-spec §4.3.2: everything outside `[t − behind, t + ahead]` is removed. */
export function planEviction(ranges: readonly Range[], t: number, win: BufferWindow): Eviction {
  const lo = t - win.behindSeconds;
  const hi = t + win.aheadSeconds;
  const out: Eviction = { behind: [], ahead: [] };
  for (const [s, e] of ranges) {
    if (s < lo) out.behind.push([s, Math.min(e, lo)]);
    if (e > hi) out.ahead.push([Math.max(s, hi), e]);
  }
  return out;
}

export type SeekPlan =
  | { kind: 'buffered' }
  /** No seek_index (a recovered clip): only already-buffered targets are reachable. */
  | { kind: 'unindexed' }
  | { kind: 'fetch'; chunk: number; intraOffset: number; fragmentMs: number };

export interface SeekInput {
  tMs: number;
  seekIndex: readonly SeekEntry[];
  chunkSize: number;
  buffered: readonly Range[];
}

/** ui-spec §4.3.2 steps 1-2 with the arithmetic of Contract §6.4. */
export function planSeek(i: SeekInput): SeekPlan {
  if (isBuffered(i.buffered, i.tMs / 1000)) return { kind: 'buffered' };
  if (i.seekIndex.length === 0) return { kind: 'unindexed' };
  const { chunk, intraOffset } = seekToChunk(i.seekIndex, i.tMs, i.chunkSize);
  // The entry seekToChunk picked: the last with t_ms <= tMs, or the first.
  let fragment = i.seekIndex[0]![0];
  for (const [t] of i.seekIndex) {
    if (t <= i.tMs) fragment = t;
    else break;
  }
  return { kind: 'fetch', chunk, intraOffset, fragmentMs: fragment };
}

/**
 * ~Media time at which verification failed: the start of the last fragment at
 * or before the failing chunk, so what was shown before it was all verified.
 */
export function failureTimeMs(seekIndex: readonly SeekEntry[], chunk: number, chunkSize: number): number {
  const at = chunk * chunkSize;
  let t = 0;
  for (const [ms, offset] of seekIndex) {
    if (offset <= at) t = ms;
    else break;
  }
  return t;
}
