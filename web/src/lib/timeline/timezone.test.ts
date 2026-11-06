import { describe, expect, it } from 'vitest';
import { localDayRange, zoneOffsetMs } from './timezone';

const H = 3_600_000;

describe('localDayRange', () => {
  it('Asia/Jakarta is UTC+7 with no DST: a 24 h day starting at 17:00Z', () => {
    const [from, to] = localDayRange('2026-11-06', 'Asia/Jakarta');
    expect(from).toBe(Date.UTC(2026, 10, 5, 17));
    expect(to).toBe(Date.UTC(2026, 10, 6, 17));
  });

  it('America/New_York on an ordinary winter day is UTC-5', () => {
    const [from, to] = localDayRange('2026-11-06', 'America/New_York');
    expect(from).toBe(Date.UTC(2026, 10, 6, 5));
    expect(to - from).toBe(24 * H);
  });

  it('spring-forward day in New York is 23 h long', () => {
    const [from, to] = localDayRange('2026-03-08', 'America/New_York');
    expect(from).toBe(Date.UTC(2026, 2, 8, 5)); // 00:00 EST
    expect(to).toBe(Date.UTC(2026, 2, 9, 4)); // 00:00 EDT
    expect(to - from).toBe(23 * H);
  });

  it('fall-back day in New York is 25 h long', () => {
    const [from, to] = localDayRange('2026-11-01', 'America/New_York');
    expect(from).toBe(Date.UTC(2026, 10, 1, 4)); // 00:00 EDT
    expect(to).toBe(Date.UTC(2026, 10, 2, 5)); // 00:00 EST
    expect(to - from).toBe(25 * H);
  });

  it('half-hour zones are handled (Asia/Kolkata, UTC+5:30)', () => {
    const [from, to] = localDayRange('2026-11-06', 'Asia/Kolkata');
    expect(from).toBe(Date.UTC(2026, 10, 5, 18, 30));
    expect(to - from).toBe(24 * H);
  });

  it('ranges of consecutive days abut exactly', () => {
    const a = localDayRange('2026-03-07', 'America/New_York');
    const b = localDayRange('2026-03-08', 'America/New_York');
    const c = localDayRange('2026-03-09', 'America/New_York');
    expect(a[1]).toBe(b[0]);
    expect(b[1]).toBe(c[0]);
  });

  it('rejects a malformed date or an unknown zone', () => {
    expect(() => localDayRange('06/11/2026', 'UTC')).toThrow();
    expect(() => localDayRange('2026-13-40', 'UTC')).toThrow();
    expect(() => localDayRange('2026-11-06', 'Not/AZone')).toThrow();
  });
});

describe('zoneOffsetMs', () => {
  it('reports the offset in force at the instant', () => {
    expect(zoneOffsetMs(Date.UTC(2026, 6, 1), 'America/New_York')).toBe(-4 * H);
    expect(zoneOffsetMs(Date.UTC(2026, 0, 1), 'America/New_York')).toBe(-5 * H);
    expect(zoneOffsetMs(Date.UTC(2026, 0, 1), 'Asia/Jakarta')).toBe(7 * H);
  });
});
