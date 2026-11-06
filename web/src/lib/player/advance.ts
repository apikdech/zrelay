// Auto-advance (ui-spec §4.3.2): the clip that follows a finished one. The same
// series comes first (segment_index + 1, as zview's NextInSeries); otherwise the
// camera's next clip by start time. The lookup spans the clip's UTC day and the
// next, because a max_duration series can cross midnight (Contract §7.1).
import type { Request } from '../../worker/protocol';

interface ClipRef {
  clip_id: string;
  series_id: string;
  segment_index: number;
  camera: { id: string };
  start_time: string;
}

export interface ApiCaller {
  call<T = unknown>(msg: Request): Promise<T>;
}

const DAY_MS = 86_400_000;

/** Picks the follower of `cur` among `list` (same camera only). Pure. */
export function pickNext(cur: ClipRef, list: readonly ClipRef[]): string | null {
  const others = list.filter((c) => c.clip_id !== cur.clip_id && c.camera.id === cur.camera.id);
  const sameSeries = others.find((c) => c.series_id === cur.series_id && c.segment_index === cur.segment_index + 1);
  if (sameSeries) return sameSeries.clip_id;
  const t = Date.parse(cur.start_time);
  const later = others
    .filter((c) => Date.parse(c.start_time) > t)
    .sort((a, b) => Date.parse(a.start_time) - Date.parse(b.start_time));
  return later[0]?.clip_id ?? null;
}

/** Looks up the clip after `clipId`, or null when there is none. */
export async function findNextClip(client: ApiCaller, clipId: string): Promise<string | null> {
  const cur = await client.call<ClipRef>({ type: 'api.get', path: `/api/v1/clips/${clipId}`, query: {} });
  const start = Date.parse(cur.start_time);
  if (Number.isNaN(start)) return null;
  const dayStart = Math.floor(start / DAY_MS) * DAY_MS;
  const res = await client.call<{ clips: ClipRef[] } | ClipRef[]>({
    type: 'api.get',
    path: '/api/v1/clips',
    query: {
      from: new Date(start).toISOString(),
      to: new Date(dayStart + 2 * DAY_MS).toISOString(),
      camera: cur.camera.id,
    },
  });
  const list = Array.isArray(res) ? res : res.clips;
  return pickNext({ ...cur, clip_id: clipId }, list);
}
