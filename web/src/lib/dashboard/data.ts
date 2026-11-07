// Loading one local day of clips and coverage for the timeline page, and the
// mapping from API shapes to the timeline model.
import type { Request } from '../../worker/protocol';
import type { Coverage as ApiCoverage, Sidecar } from '../../worker/schemas';
import type { Coverage, TimelineClip } from '../timeline/model';
import { localDayOf, localDayRange } from '../timeline/timezone';

/** ui-spec §3.4 `assumed_max_clip_seconds` default (300): bounds an incomplete clip that has no end. */
export const ASSUMED_MAX_CLIP_MS = 300_000;

export { localDayOf };

export function toTimelineClip(c: Sidecar): TimelineClip {
  const startMs = Date.parse(c.start_time);
  const end = c.end_time === null ? NaN : Date.parse(c.end_time);
  return {
    id: c.clip_id,
    cameraId: c.camera.id,
    startMs,
    endMs: Number.isNaN(end) ? startMs + ASSUMED_MAX_CLIP_MS : end,
    triggers: c.triggers.map((t) => ({ type: t.type, firstAtMs: t.first_at_ms, lastAtMs: t.last_at_ms })),
  };
}

export function toCoverage(cameraId: string, c: ApiCoverage): Coverage[] {
  return c.ready.map(([from, to]) => ({ cameraId, fromMs: Date.parse(from), toMs: Date.parse(to) }));
}

export interface DayQuery {
  /** YYYY-MM-DD in `zone`. */
  day: string;
  zone: string;
  /** '' for all cameras. */
  camera: string;
  /** '' for all triggers. */
  trigger: string;
  /** Every known camera id; coverage is read per camera. */
  cameras: readonly string[];
}

export interface DayData {
  clips: Sidecar[];
  coverage: Coverage[];
  /** Summaries the API returned that failed validation and were dropped. */
  invalid: number;
}

type Caller = { call<T = unknown>(msg: Request): Promise<T> };

export async function loadDay(client: Caller, q: DayQuery): Promise<DayData> {
  const [from, to] = localDayRange(q.day, q.zone);
  const range = { from: new Date(from).toISOString(), to: new Date(to).toISOString() };
  const clipQuery: Record<string, string> = { ...range };
  if (q.camera) clipQuery.camera = q.camera;
  if (q.trigger) clipQuery.trigger = q.trigger;

  const covCameras = q.camera ? [q.camera] : [...q.cameras];
  const [list, perCamera] = await Promise.all([
    client.call<{ clips: Sidecar[]; invalid: number }>({ type: 'api.get', path: '/api/v1/clips', query: clipQuery }),
    Promise.all(
      covCameras.map(async (id) => {
        try {
          const c = await client.call<ApiCoverage>({ type: 'api.get', path: '/api/v1/coverage', query: { camera: id, ...range } });
          return toCoverage(id, c);
        } catch {
          // No coverage for one camera only means its gaps are not drawn.
          return [];
        }
      }),
    ),
  ]);
  return { clips: list.clips, invalid: list.invalid, coverage: perCamera.flat() };
}
