import { describe, expect, it } from 'vitest';
import { RECENT_CLIPS, keyMismatch } from './mismatch';

const A = 'aaaaaaaaaaaaaaaa';
const B = 'bbbbbbbbbbbbbbbb';
const C = 'cccccccccccccccc';

const clip = (i: number, keyId: string) => ({
  start_time: new Date(Date.UTC(2026, 10, 1, 0, 0, i)).toISOString(),
  video: { key_id: keyId },
});

describe('keyMismatch (does the imported key match recent recordings?)', () => {
  it('does not warn when there are no clips to compare against', () => {
    expect(keyMismatch([], A)).toBeNull();
  });

  it('does not warn when a recent clip carries the key', () => {
    expect(keyMismatch([clip(1, B), clip(2, A)], A)).toBeNull();
  });

  it('warns when none carry the key and lists the clips key ids', () => {
    expect(keyMismatch([clip(1, B), clip(2, C), clip(3, B)], A)).toEqual({ keyId: A, clipKeyIds: [B, C] });
  });

  it('looks only at the latest 20 clips by start time', () => {
    expect(RECENT_CLIPS).toBe(20);
    // 25 newer clips in B; the only A clip is the oldest, outside the window.
    const clips = [clip(0, A), ...Array.from({ length: 25 }, (_, i) => clip(i + 1, B))];
    expect(keyMismatch(clips, A)).toEqual({ keyId: A, clipKeyIds: [B] });
    // The same A clip, once it is among the latest 20, silences the warning.
    expect(keyMismatch([...clips, clip(100, A)], A)).toBeNull();
  });
});
