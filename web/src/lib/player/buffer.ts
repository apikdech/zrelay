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

/** How far short of the next fragment start a buffered range can end and still count as having reached it. */
const END_SLACK = 0.1;

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

/**
 * Moves the start of each range up to the next fragment start (dropping what would be empty), so trimming
 * never cuts through a fragment: the buffer then ends on a complete one, and a resume knows where to pick up.
 * Without an index the ranges are returned as they are.
 */
export function alignToFragments(ranges: readonly Range[], seekIndex: readonly SeekEntry[]): Range[] {
  if (seekIndex.length === 0) return [...ranges];
  const out: Range[] = [];
  for (const [s, e] of ranges) {
    const next = seekIndex.find(([t]) => t / 1000 >= s - 1e-9);
    if (next && next[0] / 1000 < e) out.push([next[0] / 1000, e]);
  }
  return out;
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

export interface ResumeInput {
  /** The seek target (ms), expected to be buffered. */
  tMs: number;
  seekIndex: readonly SeekEntry[];
  chunkSize: number;
  buffered: readonly Range[];
  /** The media source has ended: the final chunk is appended and nothing was removed since. */
  ended?: boolean;
}

/**
 * Where a sequential fill picks up for a target inside the buffer: the fragment holding the end of the
 * buffered range that holds the target, found with the seek planner (Contract §6.4) like any other position.
 * The range end can sit inside that fragment (a fill stops on a chunk boundary, not a fragment boundary), so
 * the fragment is the last one starting at or before the end, with a hair of slack for a range that ends just
 * short of the next start. Re-appending the part already buffered is harmless in `segments` mode.
 *
 * Null only when there is nothing to continue with: the target is not buffered, there is no index, or the
 * range already reaches the end of the clip (the stream has ended and no range follows it).
 */
export function planResume(i: ResumeInput): { chunk: number; intraOffset: number } | null {
  const r = holding(i.buffered, i.tMs / 1000);
  if (!r || i.seekIndex.length === 0) return null;
  if (i.ended && i.buffered.every(([, e]) => e <= r[1])) return null;
  const plan = planSeek({
    tMs: r[1] * 1000 + END_SLACK * 1000,
    seekIndex: i.seekIndex,
    chunkSize: i.chunkSize,
    buffered: [],
  });
  return plan.kind === 'fetch' ? { chunk: plan.chunk, intraOffset: plan.intraOffset } : null;
}

/**
 * True when a sequential fill whose next byte is plaintext `offset` is already carrying the range that holds
 * `tMs`: the fragment it is assembling starts inside that range or right at its end, so it re-appends up to the
 * end and then extends it. A fill anywhere else (feeding a different range) never extends this one, however
 * much it appends.
 */
export function feedsRange(buffered: readonly Range[], tMs: number, offset: number, seekIndex: readonly SeekEntry[]): boolean {
  const r = holding(buffered, tMs / 1000);
  if (!r || seekIndex.length === 0) return false;
  let at = seekIndex[0]![0];
  for (const [t, o] of seekIndex) {
    if (o <= offset) at = t;
    else break;
  }
  return at / 1000 >= r[0] - START_SLACK && at / 1000 <= r[1] + START_SLACK;
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
