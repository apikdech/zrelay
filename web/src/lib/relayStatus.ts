// The relay status dot (ui-spec §4.7): red when relay.json is older than 3
// minutes or a camera is down, amber while something is degraded or the
// storage breaker is not closed, grey when no status could be read.
import type { Status } from '../worker/schemas';

/** Contract §7.5.1: viewers treat the relay as down when updated_at is older than this. */
export const STALE_AFTER_MS = 3 * 60_000;

export type HealthLevel = 'ok' | 'warn' | 'bad' | 'unknown';

export interface RelayHealth {
  level: HealthLevel;
  /** One line per finding; the title attribute of the dot. */
  tooltip: string;
  /** The most important finding, for the accessible name. */
  summary: string;
}

export function relayHealth(status: Status | null, nowMs: number): RelayHealth {
  if (status === null) {
    const s = 'Relay status unavailable';
    return { level: 'unknown', tooltip: s, summary: s };
  }
  const bad: string[] = [];
  const warn: string[] = [];

  const updated = Date.parse(status.updated_at);
  if (Number.isNaN(updated) || nowMs - updated > STALE_AFTER_MS) {
    bad.push(`Relay status is stale: last updated ${status.updated_at}`);
  }
  for (const c of status.cameras) {
    if (c.rtsp === 'down') bad.push(`${c.name}: camera is down since ${c.since}`);
    else if (c.rtsp === 'degraded') warn.push(`${c.name}: video is degraded since ${c.since}`);
    else if (c.events !== 'up') warn.push(`${c.name}: events ${c.events} since ${c.since}`);
  }
  const st = status.storage;
  if (st && st.state !== 'closed') {
    warn.push(`Storage breaker ${st.state.replace('_', '-')}${st.since ? ` since ${st.since}` : ''}`);
  }

  const lines = [...bad, ...warn];
  if (bad.length > 0) return { level: 'bad', tooltip: lines.join('\n'), summary: bad[0]! };
  if (warn.length > 0) return { level: 'warn', tooltip: lines.join('\n'), summary: warn[0]! };
  const ok = `Relay OK, updated ${status.updated_at}`;
  return { level: 'ok', tooltip: ok, summary: ok };
}
