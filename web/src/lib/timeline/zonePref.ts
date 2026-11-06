// The viewer's forced display zone (ui-spec §5: "an optional forced display
// zone (e.g. Asia/Jakarta) for relatives abroad"). A per-viewer convenience
// only: storage can be blocked or empty, so every access is guarded and the
// page works without it.

/** localStorage key of the forced zone. */
export const ZONE_KEY = 'zview.zone';

/** Whether `zone` is a time zone name this browser's Intl knows. */
export function isValidZone(zone: string): boolean {
  if (zone === '') return false;
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/** The remembered forced zone, or null when none is set or it is not valid. */
export function loadZonePref(): string | null {
  try {
    const v = localStorage.getItem(ZONE_KEY);
    return v !== null && isValidZone(v) ? v : null;
  } catch {
    return null;
  }
}

/** Remember `zone` as the forced zone; null (or an invalid name) clears it. */
export function saveZonePref(zone: string | null): void {
  try {
    if (zone !== null && isValidZone(zone)) localStorage.setItem(ZONE_KEY, zone);
    else localStorage.removeItem(ZONE_KEY);
  } catch {
    // Storage unavailable: the choice lasts for this visit only.
  }
}

/** Zones offered by the picker: every IANA zone the browser lists, plus `current`. */
export function zoneChoices(current: string | null): string[] {
  let all: string[] = [];
  try {
    all = Intl.supportedValuesOf('timeZone');
  } catch {
    // Older engines: fall back to the zone the family needs most.
  }
  const set = new Set(all);
  set.add('Asia/Jakarta');
  set.add('UTC');
  if (current !== null && isValidZone(current)) set.add(current);
  return [...set].sort();
}
