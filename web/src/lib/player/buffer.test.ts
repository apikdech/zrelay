import { describe, expect, it } from 'vitest';
import {
  BLOB_LRU,
  BLOB_MAX,
  WINDOWS,
  bufferedAhead,
  chooseProfile,
  failureTimeMs,
  isBuffered,
  needsMore,
  planEviction,
  planResume,
  planSeek,
  type Range,
} from './buffer';

const CS = 65536;
// A 2 s GOP fragment every 2 s, ~100 KiB each: offsets cross chunk boundaries.
const seekIndex: [number, number][] = Array.from({ length: 16 }, (_, i) => [i * 2000, i * 100_000]);

describe('buffer windows (ui-spec §4.3.2 table)', () => {
  it('mobile is 30 s ahead and 15 s behind, desktop 90 s and 30 s', () => {
    expect(WINDOWS.mobile).toEqual({ aheadSeconds: 30, behindSeconds: 15 });
    expect(WINDOWS.desktop).toEqual({ aheadSeconds: 90, behindSeconds: 30 });
  });

  it('blob fallback limits are 48/256 MiB with an LRU of 1/3 URLs (§4.3.3)', () => {
    expect(BLOB_MAX).toEqual({ mobile: 48 << 20, desktop: 256 << 20 });
    expect(BLOB_LRU).toEqual({ mobile: 1, desktop: 3 });
  });
});

describe('chooseProfile', () => {
  it('touch plus a narrow viewport is mobile', () => {
    expect(chooseProfile({ touch: true, narrow: true })).toBe('mobile');
  });
  it('deviceMemory <= 4 is mobile whatever the viewport', () => {
    expect(chooseProfile({ touch: false, narrow: false, deviceMemory: 4 })).toBe('mobile');
    expect(chooseProfile({ touch: false, narrow: false, deviceMemory: 2 })).toBe('mobile');
  });
  it('touch alone, narrow alone or a big-memory desktop is desktop', () => {
    expect(chooseProfile({ touch: true, narrow: false })).toBe('desktop');
    expect(chooseProfile({ touch: false, narrow: true })).toBe('desktop');
    expect(chooseProfile({ touch: false, narrow: false, deviceMemory: 8 })).toBe('desktop');
    expect(chooseProfile({ touch: false, narrow: false })).toBe('desktop');
  });
});

describe('bufferedAhead / needsMore', () => {
  const win = WINDOWS.mobile;
  it('measures to the end of the range holding the playhead', () => {
    expect(bufferedAhead([[0, 20]], 5)).toBe(15);
    expect(bufferedAhead([[0, 10], [12, 40]], 5)).toBe(5);
    expect(bufferedAhead([[0, 10], [12, 40]], 13)).toBe(27);
  });
  it('is zero when the playhead is outside every range', () => {
    expect(bufferedAhead([], 0)).toBe(0);
    expect(bufferedAhead([[10, 20]], 5)).toBe(0);
    expect(bufferedAhead([[10, 20]], 25)).toBe(0);
  });
  it('tolerates a playhead a hair before the first range (first frame is not at 0.000)', () => {
    expect(bufferedAhead([[0.067, 10]], 0)).toBeCloseTo(10, 5);
  });
  it('needs more only while ahead is below ahead_seconds', () => {
    expect(needsMore([], 0, win)).toBe(true);
    expect(needsMore([[0, 29.9]], 0, win)).toBe(true);
    expect(needsMore([[0, 30]], 0, win)).toBe(false);
    expect(needsMore([[0, 40]], 10, win)).toBe(false);
    expect(needsMore([[0, 40]], 12, win)).toBe(true);
  });
});

describe('isBuffered', () => {
  it('is true inside a range and false in a gap or past the end', () => {
    const r: Range[] = [[0, 10], [20, 30]];
    expect(isBuffered(r, 5)).toBe(true);
    expect(isBuffered(r, 25)).toBe(true);
    expect(isBuffered(r, 15)).toBe(false);
    expect(isBuffered(r, 30)).toBe(false);
  });
});

describe('planEviction', () => {
  const win = WINDOWS.mobile; // [t-15, t+30]
  it('removes nothing when everything is inside the window', () => {
    expect(planEviction([[10, 40]], 20, win)).toEqual({ behind: [], ahead: [] });
  });
  it('removes the part behind currentTime - behind_seconds', () => {
    expect(planEviction([[0, 40]], 30, win)).toEqual({ behind: [[0, 15]], ahead: [] });
  });
  it('removes the part beyond currentTime + ahead_seconds', () => {
    expect(planEviction([[0, 100]], 10, win)).toEqual({ behind: [], ahead: [[40, 100]] });
  });
  it('splits across several ranges and drops ranges wholly outside', () => {
    const p = planEviction([[0, 5], [10, 50], [90, 100]], 40, win);
    expect(p.behind).toEqual([[0, 5], [10, 25]]);
    expect(p.ahead).toEqual([[90, 100]]);
  });
  it('never returns an empty or reversed range', () => {
    const p = planEviction([[0, 15], [15, 30]], 30, win);
    for (const [s, e] of [...p.behind, ...p.ahead]) expect(e).toBeGreaterThan(s);
  });
  it('a tighter window (quota eviction) removes more', () => {
    const tight = planEviction([[0, 40]], 30, { aheadSeconds: 30, behindSeconds: 2 });
    expect(tight.behind).toEqual([[0, 28]]);
  });
});

describe('planSeek (Contract §6.4)', () => {
  it('is a no-fetch when the target is already buffered', () => {
    expect(planSeek({ tMs: 20_000, seekIndex, chunkSize: CS, buffered: [[10, 30]] })).toEqual({ kind: 'buffered' });
  });
  it('picks the last fragment at or before t and its chunk and intra-chunk offset', () => {
    // t = 20_000 -> entry 10 (t 20000, offset 1_000_000) -> chunk 15, offset 1_000_000 - 15*65536
    const p = planSeek({ tMs: 20_000, seekIndex, chunkSize: CS, buffered: [] });
    expect(p).toEqual({ kind: 'fetch', chunk: 15, intraOffset: 1_000_000 - 15 * CS, fragmentMs: 20_000 });
  });
  it('rounds down to the fragment before t', () => {
    const p = planSeek({ tMs: 21_900, seekIndex, chunkSize: CS, buffered: [] });
    expect(p).toMatchObject({ kind: 'fetch', fragmentMs: 20_000 });
  });
  it('seeking past the last entry uses the last fragment', () => {
    const p = planSeek({ tMs: 999_999, seekIndex, chunkSize: CS, buffered: [] });
    expect(p).toMatchObject({ kind: 'fetch', fragmentMs: 30_000 });
  });
  it('without an index (recovered clip) only buffered seeks are possible', () => {
    expect(planSeek({ tMs: 5000, seekIndex: [], chunkSize: CS, buffered: [] })).toEqual({ kind: 'unindexed' });
    expect(planSeek({ tMs: 5000, seekIndex: [], chunkSize: CS, buffered: [[0, 9]] })).toEqual({ kind: 'buffered' });
  });
});

describe('failureTimeMs', () => {
  it('is the media time of the last fragment starting at or before the failing chunk', () => {
    // chunk 15 starts at plaintext offset 983_040: fragment 9 (offset 900_000, t 18000).
    expect(failureTimeMs(seekIndex, 15, CS)).toBe(18_000);
  });
  it('is 0 for a failure in the first chunk or with no index', () => {
    expect(failureTimeMs(seekIndex, 0, CS)).toBe(0);
    expect(failureTimeMs([], 7, CS)).toBe(0);
  });
});

describe('planResume (continue after the buffered range holding a seek target)', () => {
  const idx: [number, number][] = Array.from({ length: 100 }, (_, i) => [i * 1000, i * CS]);
  const at = (buffered: Range[], tMs: number) => planResume({ tMs, seekIndex: idx, chunkSize: CS, buffered });

  it('resumes at the fragment starting where the holding range ends', () => {
    expect(at([[0, 30]], 5000)).toEqual({ chunk: 30, intraOffset: 0 });
    expect(at([[0, 10], [20, 40]], 25_000)).toEqual({ chunk: 40, intraOffset: 0 });
  });

  it('tolerates a range end a hair past the fragment start', () => {
    expect(at([[0, 30.033]], 5000)).toEqual({ chunk: 30, intraOffset: 0 });
  });

  it('is null when the range reaches the last fragment, the target is unbuffered, or there is no index', () => {
    expect(at([[0, 100]], 5000)).toBeNull();
    expect(at([[0, 30]], 50_000)).toBeNull();
    expect(planResume({ tMs: 5000, seekIndex: [], chunkSize: CS, buffered: [[0, 30]] })).toBeNull();
  });
});
