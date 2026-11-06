// Valibot schemas mirroring Contract §6-7. Everything from the data origin is
// untrusted (ui-spec §4.5): identifiers are validated strictly, display strings
// are length-capped, numbers are bounds-checked, unknown fields are dropped and
// unknown schema majors are rejected.
import * as v from 'valibot';

/** Display strings are capped at 128 characters (Contract §6.2). */
export const STRING_CAP = 128;
const TAGS_MAX = 16;
const TRIGGERS_MAX = 256;
const TOPICS_MAX = 16;
const SEEK_MAX = 1 << 20;

export class SchemaError extends Error {
  override name = 'ErrSchema';
}

const ULID_RE = /^[0-9A-HJKMNP-TV-Z]{26}$/;
const CAMERA_ID_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
// Contract §7.2: HHMMSSmmm_<ULID>.zrec, a bare filename.
const VIDEO_OBJECT_RE = /^[0-9]{9}_[0-9A-HJKMNP-TV-Z]{26}\.zrec$/;
const HEX16_RE = /^[0-9a-f]{16}$/;
const RFC3339_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$/;

export const isUlid = (s: unknown): s is string => typeof s === 'string' && ULID_RE.test(s);
export const isCameraId = (s: unknown): s is string => typeof s === 'string' && CAMERA_ID_RE.test(s);

/** Truncates to STRING_CAP code points, never splitting a surrogate pair. */
export function cap(s: string): string {
  if (s.length <= STRING_CAP) return s;
  return Array.from(s).slice(0, STRING_CAP).join('');
}

const capped = v.pipe(v.string(), v.transform(cap));
const ulid = v.pipe(v.string(), v.regex(ULID_RE));
const cameraId = v.pipe(v.string(), v.regex(CAMERA_ID_RE));
const time = v.pipe(v.string(), v.regex(RFC3339_RE), v.check((s) => !Number.isNaN(Date.parse(s))));
const nat = v.pipe(v.number(), v.integer(), v.minValue(0), v.maxValue(Number.MAX_SAFE_INTEGER));
const cappedList = (max: number) => v.pipe(v.array(capped), v.transform((a) => a.slice(0, max)));

const SidecarSchemaId = v.pipe(
  v.string(),
  v.check((s) => /^zrec\.sidecar\/2$/.test(s), 'unsupported sidecar schema (readers accept major 2 only)'),
);

const trigger = v.object({
  type: v.picklist(['motion', 'person', 'vehicle', 'pet', 'other']),
  first_at_ms: nat,
  last_at_ms: nat,
  raw_topics: v.optional(v.pipe(v.array(capped), v.transform((a) => a.slice(0, TOPICS_MAX)))),
});

const seekEntry = v.tuple([nat, nat]);

const video = v.object({
  object: v.pipe(v.string(), v.regex(VIDEO_OBJECT_RE)),
  format_version: v.literal(1),
  key_id: v.pipe(v.string(), v.regex(HEX16_RE)),
  chunk_size: nat,
  size_bytes: nat,
  plaintext_size_bytes: v.optional(v.nullable(nat), null),
  container: capped,
  codec: capped,
  width: v.optional(nat, 0),
  height: v.optional(nat, 0),
  init_segment_bytes: nat,
  seek_index: v.optional(v.pipe(v.array(seekEntry), v.maxLength(SEEK_MAX)), []),
});

const sidecarObject = v.object({
  schema: SidecarSchemaId,
  status: v.picklist(['complete', 'incomplete']),
  clip_id: ulid,
  series_id: ulid,
  segment_index: nat,
  camera: v.object({
    id: cameraId,
    name: capped,
    brand: capped,
    tags: cappedList(TAGS_MAX),
  }),
  start_time: time,
  end_time: v.nullable(time),
  duration_ms: v.nullable(nat),
  pre_roll_ms: nat,
  end_reason: v.picklist([
    'post_roll_elapsed',
    'max_duration',
    'stream_lost',
    'codec_change',
    'backlog_overflow',
    'shutdown',
    'recovered',
  ]),
  triggers: v.pipe(v.array(trigger), v.maxLength(TRIGGERS_MAX)),
  video,
  recorder: v.object({
    relay_id: capped,
    version: capped,
    time_source: v.picklist(['rtcp', 'arrival']),
  }),
  video_href: v.optional(capped),
});

type RawSidecar = v.InferOutput<typeof sidecarObject>;

/** Cross-field rules of Contract §6.2 and ui-spec §4.5. `full` also demands seek_index. */
function crossChecks(s: RawSidecar, full: boolean, add: (msg: string) => void): void {
  const vid = s.video;
  if (s.status === 'complete') {
    if (s.end_time === null || s.duration_ms === null) add('complete clip needs end_time and duration_ms');
    if (vid.plaintext_size_bytes === null) add('complete clip needs plaintext_size_bytes');
    if (full && vid.seek_index.length === 0) add('complete clip needs seek_index');
  }
  const size = vid.plaintext_size_bytes;
  let prevT = -1;
  let prevO = -1;
  for (const [t, o] of vid.seek_index) {
    if (t < prevT) add('seek_index time goes backwards');
    if (o <= prevO) add('seek_index offsets are not increasing');
    if (size !== null && o >= size) add('seek_index offset beyond plaintext_size_bytes');
    prevT = t;
    prevO = o;
  }
}

function withCross(full: boolean) {
  return v.pipe(
    sidecarObject,
    v.rawCheck(({ dataset, addIssue }) => {
      if (dataset.typed) crossChecks(dataset.value, full, (message) => addIssue({ message }));
    }),
  );
}

const fullSidecar = withCross(true);
const summary = withCross(false);

export type Sidecar = v.InferOutput<typeof fullSidecar>;

function run<T extends v.GenericSchema>(schema: T, input: unknown, what: string): v.InferOutput<T> {
  const r = v.safeParse(schema, input);
  if (!r.success) {
    const first = r.issues[0];
    const path = first?.path?.map((p) => String(p.key)).join('.') ?? '';
    throw new SchemaError(`invalid ${what}${path ? ` at ${path}` : ''}: ${first?.message ?? 'bad shape'}`);
  }
  return r.output;
}

/** A full sidecar (`GET /api/v1/clips/{id}`). Throws SchemaError. */
export const parseSidecar = (input: unknown): Sidecar => run(fullSidecar, input, 'sidecar');

/**
 * Clip summaries. Each is validated on its own: invalid ones are counted and
 * dropped, never served ("invalid" clips are not playable). Accepts a bare
 * array or a `{clips: [...]}` envelope.
 */
export function parseClipList(input: unknown): { clips: Sidecar[]; invalid: number } {
  const items = Array.isArray(input)
    ? input
    : input && typeof input === 'object' && Array.isArray((input as { clips?: unknown }).clips)
      ? (input as { clips: unknown[] }).clips
      : null;
  if (!items) throw new SchemaError('invalid clip list: expected an array');
  const clips: Sidecar[] = [];
  let invalid = 0;
  for (const it of items) {
    const r = v.safeParse(summary, it);
    if (r.success) clips.push(r.output);
    else invalid++;
  }
  return { clips, invalid };
}

const camera = v.object({
  id: cameraId,
  name: capped,
  brand: v.optional(capped, ''),
  tags: v.optional(cappedList(TAGS_MAX), []),
  last_clip_at: v.optional(v.nullable(time), null),
  online: v.optional(v.boolean(), false),
});
export type Camera = v.InferOutput<typeof camera>;
export const parseCameras = (input: unknown): Camera[] => run(v.array(camera), input, 'camera list');

const state = v.picklist(['up', 'down', 'degraded']);
const status = v.object({
  schema: v.pipe(v.string(), v.check((s) => /^zrec\.status\/1$/.test(s), 'unsupported status schema')),
  relay_id: capped,
  version: capped,
  updated_at: time,
  key_id: v.pipe(v.string(), v.regex(HEX16_RE)),
  cameras: v.array(
    v.object({
      id: cameraId,
      name: capped,
      rtsp: state,
      events: state,
      since: time,
      last_clip_at: v.optional(v.nullable(time), null),
    }),
  ),
  storage: v.optional(
    v.object({ state: capped, since: v.optional(capped), backlog_bytes: v.optional(nat) }),
  ),
  indexed_days: v.optional(nat),
  invalid_sidecars: v.optional(nat),
  index_fresh_at: v.optional(capped),
});
export type Status = v.InferOutput<typeof status>;
export const parseStatus = (input: unknown): Status => run(status, input, 'status');

const coverage = v.object({
  ready: v.array(v.tuple([time, time])),
  dropped: v.array(
    v.object({
      at: time,
      reason: v.picklist(['stream_lost', 'backlog', 'shutdown']),
      approx_ms: nat,
    }),
  ),
});
export type Coverage = v.InferOutput<typeof coverage>;
export const parseCoverage = (input: unknown): Coverage => run(coverage, input, 'coverage');
