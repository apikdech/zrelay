import { describe, expect, it } from 'vitest';
import { localDayOf, localDayRange, zoneOffsetMs } from './timezone';

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

  // Zones whose DST change happens at local midnight: local 00:00 does not exist
  // on the spring-forward day (it jumps 00:00 -> 01:00).
  it('America/Santiago spring-forward day starts at the transition (01:00 local) and is 23 h', () => {
    const prev = localDayRange('2026-09-05', 'America/Santiago');
    const day = localDayRange('2026-09-06', 'America/Santiago');
    const next = localDayRange('2026-09-07', 'America/Santiago');
    expect(prev[0]).toBe(Date.UTC(2026, 8, 5, 4)); // 00:00 -04
    expect(prev[1]).toBe(Date.UTC(2026, 8, 6, 4)); // 24 h, includes 23:00-24:00
    expect(prev[1] - prev[0]).toBe(24 * H);
    expect(day[0]).toBe(prev[1]);
    expect(day[0]).toBe(Date.UTC(2026, 8, 6, 4));
    expect(day[1]).toBe(Date.UTC(2026, 8, 7, 3)); // 00:00 -03
    expect(day[1] - day[0]).toBe(23 * H);
    expect(next[0]).toBe(day[1]);
    for (const [date, [from]] of [['2026-09-05', prev], ['2026-09-06', day], ['2026-09-07', next]] as const) {
      expect(localDayOf(from, 'America/Santiago')).toBe(date);
    }
  });

  it('America/Havana spring-forward day starts at the transition, not an hour early', () => {
    const prev = localDayRange('2026-03-07', 'America/Havana');
    const day = localDayRange('2026-03-08', 'America/Havana');
    expect(day[0]).toBe(prev[1]);
    expect(day[0]).toBe(Date.UTC(2026, 2, 8, 5)); // 01:00 -04, the first instant of Mar 8
    expect(localDayOf(day[0], 'America/Havana')).toBe('2026-03-08');
    expect(localDayOf(day[0] - 1000, 'America/Havana')).toBe('2026-03-07');
    expect(day[1] - day[0]).toBe(23 * H);
  });

  it('localDayRange contains every instant that localDayOf places in that day (Santiago, 03:30Z on the DST day)', () => {
    const now = Date.UTC(2026, 8, 6, 3, 30);
    const date = localDayOf(now, 'America/Santiago');
    expect(date).toBe('2026-09-05');
    const [from, to] = localDayRange(date, 'America/Santiago');
    expect(now >= from && now < to).toBe(true);
  });

  it('every day of a year has exact, abutting boundaries in DST and midnight-DST zones', () => {
    const zones = [
      'America/Santiago', 'America/Havana', 'America/New_York', 'Europe/Berlin', 'Europe/London',
      'Asia/Beirut', 'Australia/Lord_Howe', 'Asia/Jakarta', 'Asia/Kolkata', 'America/Asuncion',
    ];
    for (const zone of zones) {
      let prevTo: number | null = null;
      for (let i = 0; i < 366; i++) {
        const date = new Date(Date.UTC(2026, 0, 1 + i)).toISOString().slice(0, 10);
        const [from, to] = localDayRange(date, zone);
        const ctx = `${zone} ${date}`;
        expect(localDayOf(from, zone), ctx).toBe(date);
        expect(localDayOf(from - 1000, zone) < date, ctx).toBe(true);
        expect(localDayOf(to - 1000, zone), ctx).toBe(date);
        if (prevTo !== null) expect(from, ctx).toBe(prevTo);
        prevTo = to;
      }
    }
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
