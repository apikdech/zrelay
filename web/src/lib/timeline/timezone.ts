// Time-zone helpers built on Intl only (no date library). All instants are
// epoch milliseconds; wall-clock days are `YYYY-MM-DD` strings in a zone.

const dtfCache = new Map<string, Intl.DateTimeFormat>();

function partsFormat(zone: string): Intl.DateTimeFormat {
  let f = dtfCache.get(zone);
  if (!f) {
    // Throws RangeError for an unknown zone.
    f = new Intl.DateTimeFormat('en-US', {
      timeZone: zone,
      hourCycle: 'h23',
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
    dtfCache.set(zone, f);
  }
  return f;
}

/** Offset of `zone` from UTC (local - UTC) in force at instant `ms`. */
export function zoneOffsetMs(ms: number, zone: string): number {
  const p: Record<string, number> = {};
  for (const part of partsFormat(zone).formatToParts(new Date(ms))) {
    if (part.type !== 'literal') p[part.type] = Number(part.value);
  }
  const asUTC = Date.UTC(p.year!, p.month! - 1, p.day!, p.hour!, p.minute!, p.second!);
  return asUTC - Math.floor(ms / 1000) * 1000;
}

/** The calendar day (YYYY-MM-DD) that instant `ms` falls on in `zone`. */
export function localDayOf(ms: number, zone: string): string {
  const whole = Math.floor(ms / 1000) * 1000;
  return new Date(whole + zoneOffsetMs(whole, zone)).toISOString().slice(0, 10);
}

/**
 * The first instant whose calendar day in `zone` is y-m-d. Normally that is
 * the instant the wall clock reads 00:00; where a DST change skips local
 * midnight (America/Santiago, America/Havana) it is the transition instant.
 */
function startOfLocalDay(y: number, m: number, d: number, zone: string): number {
  const wall = Date.UTC(y, m - 1, d);
  const date = new Date(wall).toISOString().slice(0, 10);
  // Two fixed-point steps settle on the right offset, including across DST.
  let guess = wall - zoneOffsetMs(wall, zone);
  guess = wall - zoneOffsetMs(guess, zone);
  if (localDayOf(guess, zone) >= date) return guess;
  // 00:00 does not exist in `zone` on this date, so the estimate landed in the
  // previous day. The day starts at the transition, within the next 24 h: find
  // the first whole second whose local date has reached `date`.
  let lo = guess; // local date < date
  let hi = guess + 24 * 3_600_000; // local date >= date
  while (hi - lo > 1000) {
    const mid = lo + Math.floor((hi - lo) / 2000) * 1000;
    if (localDayOf(mid, zone) >= date) hi = mid;
    else lo = mid;
  }
  return hi;
}

function parseDate(date: string): [number, number, number] {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!m) throw new RangeError(`invalid date ${JSON.stringify(date)}, want YYYY-MM-DD`);
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const check = new Date(Date.UTC(y, mo - 1, d));
  if (check.getUTCFullYear() !== y || check.getUTCMonth() !== mo - 1 || check.getUTCDate() !== d) {
    throw new RangeError(`invalid date ${JSON.stringify(date)}`);
  }
  return [y, mo, d];
}

/**
 * The half-open instant range [from, to) covered by calendar day `date`
 * (YYYY-MM-DD) in `zone`. A day is 23 h or 25 h across a DST change.
 */
export function localDayRange(date: string, zone: string): [number, number] {
  const [y, m, d] = parseDate(date);
  const from = startOfLocalDay(y, m, d, zone);
  const next = new Date(Date.UTC(y, m - 1, d + 1));
  const to = startOfLocalDay(next.getUTCFullYear(), next.getUTCMonth() + 1, next.getUTCDate(), zone);
  return [from, to];
}

const clockCache = new Map<string, Intl.DateTimeFormat>();

/** `HH:mm` (or `HH:mm:ss`) wall-clock label of `ms` in `zone`. */
export function formatClock(ms: number, zone: string, seconds = false): string {
  const key = `${zone}|${seconds}`;
  let f = clockCache.get(key);
  if (!f) {
    f = new Intl.DateTimeFormat('en-GB', {
      timeZone: zone,
      hourCycle: 'h23',
      hour: '2-digit',
      minute: '2-digit',
      ...(seconds ? { second: '2-digit' } : {}),
    });
    clockCache.set(key, f);
  }
  return f.format(new Date(ms));
}

/** The viewer's own IANA zone. */
export function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}
