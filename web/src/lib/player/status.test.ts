import { describe, expect, it } from 'vitest';
import { describeIntegrity, formatMmss, initialState } from './status';
import type { PlayerState } from './engine';

const st = (over: Partial<PlayerState>): PlayerState => ({ ...initialState(), ...over });

describe('formatMmss', () => {
  it('rounds to the nearest second and pads', () => {
    expect(formatMmss(0)).toBe('00:00');
    expect(formatMmss(65_400)).toBe('01:05');
    expect(formatMmss(3_599_600)).toBe('60:00');
  });
});

describe('describeIntegrity (ui-spec §4.4 wording)', () => {
  it('verifying shows a spinner', () => {
    expect(describeIntegrity(st({ integrity: 'verifying' }))).toMatchObject({ kind: 'verifying', spinner: true });
  });
  it('verifying stops spinning once opening failed', () => {
    const d = describeIntegrity(st({ integrity: 'verifying', message: 'ErrNoKey' }));
    expect(d.spinner).toBe(false);
    expect(d.text).toBe('Could not verify this recording');
  });
  it('verified shows a subtle badge with the tooltip text', () => {
    expect(describeIntegrity(st({ integrity: 'verified' }))).toMatchObject({
      kind: 'verified',
      title: 'Encrypted recording verified',
    });
  });
  it('mismatch is a warning that shows the header values as authoritative', () => {
    const d = describeIntegrity(
      st({
        integrity: 'mismatch',
        header: { cameraId: 'front-door', startUnixMs: Date.UTC(2026, 10, 6, 16, 44, 6) },
        verification: { camera: 'mismatch', start: 'match', clipId: 'match' },
      }),
    );
    expect(d.kind).toBe('mismatch');
    expect(d.text).toContain('front-door');
    expect(d.text).toContain('2026-11-06 16:44:06 UTC');
  });
  it('tampered says where verification failed and offers no export', () => {
    const d = describeIntegrity(st({ integrity: 'tampered', failedAtMs: 16_000 }));
    expect(d.text).toBe('This recording failed verification at ~00:16');
    expect(d.export).toBe(false);
  });
  it('incomplete says the recording was interrupted', () => {
    expect(describeIntegrity(st({ integrity: 'incomplete' })).text).toBe('Recording was interrupted');
  });
  it('unknown-key names the key id', () => {
    expect(describeIntegrity(st({ integrity: 'unknown-key', keyId: 'a1b2-c3d4-e5f6-0718' })).text).toBe(
      'Recorded with key a1b2-c3d4-e5f6-0718; add that key to play',
    );
  });
  it('unknown-key without a hint still reads sensibly', () => {
    expect(describeIntegrity(st({ integrity: 'unknown-key' })).text).toContain('add that key to play');
  });
});
