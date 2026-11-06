import { describe, expect, it } from 'vitest';
import { seekToChunk } from './seek';

// The seek_index from the Contract §6.1 sidecar example, chunk_size 65536.
const idx: [number, number][] = [
  [0, 1187],
  [2000, 791344],
  [4000, 1580212],
];
const CS = 65536;

describe('seekToChunk (Contract §6.4)', () => {
  it('picks the last entry with t_ms <= t', () => {
    // t=2500 -> entry [2000, 791344]; 791344 = 12*65536 + 4912
    expect(seekToChunk(idx, 2500, CS)).toEqual({ chunk: 12, intraOffset: 4912 });
  });

  it('lands exactly on an entry', () => {
    // 1580212 = 24*65536 + 7348
    expect(seekToChunk(idx, 4000, CS)).toEqual({ chunk: 24, intraOffset: 7348 });
  });

  it('seeks to the init segment end at t=0', () => {
    expect(seekToChunk(idx, 0, CS)).toEqual({ chunk: 0, intraOffset: 1187 });
  });

  it('uses the last entry for t beyond the end', () => {
    expect(seekToChunk(idx, 999_999, CS)).toEqual({ chunk: 24, intraOffset: 7348 });
  });

  it('clamps t before the first entry to the first entry', () => {
    expect(seekToChunk(idx, -5, CS)).toEqual({ chunk: 0, intraOffset: 1187 });
  });

  it('handles an offset on an exact chunk boundary', () => {
    expect(seekToChunk([[0, 0], [1000, 3 * CS]], 1000, CS)).toEqual({ chunk: 3, intraOffset: 0 });
  });

  it('supports other power-of-two chunk sizes', () => {
    expect(seekToChunk([[0, 3_000_000]], 0, 1 << 20)).toEqual({ chunk: 2, intraOffset: 3_000_000 - 2 * (1 << 20) });
  });

  it('rejects an empty index', () => {
    expect(() => seekToChunk([], 0, CS)).toThrow(RangeError);
  });
});
