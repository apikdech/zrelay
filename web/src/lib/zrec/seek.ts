/** One `video.seek_index` entry: [t_ms, plaintext_offset] (Contract §6.2). */
export type SeekEntry = readonly [tMs: number, offset: number];

/**
 * Maps media time to the chunk holding the nearest fragment at or before it
 * (Contract §6.4). Times before the first entry use the first entry.
 */
export function seekToChunk(
  seekIndex: readonly SeekEntry[],
  tMs: number,
  chunkSize: number,
): { chunk: number; intraOffset: number } {
  if (seekIndex.length === 0) throw new RangeError('empty seek_index');
  // Binary search for the last entry with t_ms <= tMs.
  let lo = 0;
  let hi = seekIndex.length - 1;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (seekIndex[mid]![0] <= tMs) lo = mid;
    else hi = mid - 1;
  }
  const offset = seekIndex[lo]![1];
  return { chunk: Math.floor(offset / chunkSize), intraOffset: offset % chunkSize };
}
