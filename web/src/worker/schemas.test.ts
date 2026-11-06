import { describe, expect, it } from 'vitest';
import { sidecarFor } from '../../test-support/worker-fixtures';
import { SchemaError, parseCameras, parseClipList, parseCoverage, parseSidecar, parseStatus } from './schemas';

const good = () => sidecarFor(1000, 1200, {}, { seek_index: [[0, 0], [2000, 400], [4000, 900]] });
const withVideo = (v: Record<string, unknown>) => {
  const s = good();
  s.video = { ...(s.video as object), ...v };
  return s;
};

describe('parseSidecar', () => {
  it('accepts a complete sidecar and ignores unknown fields', () => {
    const s = parseSidecar({ ...good(), future_field: { x: 1 } });
    expect(s.clip_id).toBe('01M2NBMVS0K8EKRQKGGAJCDT0B');
    expect(s).not.toHaveProperty('future_field');
  });

  it.each(['zrec.sidecar/3', 'zrec.sidecar/1', 'zrec.sidecar/', 'other/2', 'zrec.sidecar/2.1'])(
    'rejects schema %s',
    (schema) => {
      expect(() => parseSidecar({ ...good(), schema })).toThrow(SchemaError);
    },
  );

  it('caps a 10 kB camera name at 128 characters', () => {
    const s = good();
    s.camera = { ...(s.camera as object), name: 'x'.repeat(10_000) };
    expect(parseSidecar(s).camera.name).toHaveLength(128);
  });

  it('caps tags at 16 entries of 128 characters', () => {
    const s = good();
    s.camera = { ...(s.camera as object), tags: Array.from({ length: 40 }, () => 'y'.repeat(500)) };
    const tags = parseSidecar(s).camera.tags;
    expect(tags).toHaveLength(16);
    expect(tags.every((t) => t.length === 128)).toBe(true);
  });

  it('does not split a surrogate pair when capping', () => {
    const s = good();
    s.camera = { ...(s.camera as object), name: '😀'.repeat(200) };
    const name = parseSidecar(s).camera.name;
    expect([...name]).toHaveLength(128);
  });

  it('rejects a camera id outside the header pattern', () => {
    const s = good();
    s.camera = { ...(s.camera as object), id: '../etc' };
    expect(() => parseSidecar(s)).toThrow(SchemaError);
  });

  it('rejects a video.object that is a path', () => {
    expect(() => parseSidecar(withVideo({ object: '../x/145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec' }))).toThrow(SchemaError);
  });

  it('rejects a seek_index that goes backwards in time', () => {
    expect(() => parseSidecar(withVideo({ seek_index: [[0, 0], [2000, 400], [1000, 900]] }))).toThrow(SchemaError);
  });

  it('rejects a seek_index with non-increasing offsets', () => {
    expect(() => parseSidecar(withVideo({ seek_index: [[0, 100], [2000, 100]] }))).toThrow(SchemaError);
  });

  it('rejects a seek_index offset at or beyond plaintext_size_bytes', () => {
    expect(() => parseSidecar(withVideo({ seek_index: [[0, 0], [2000, 1000]] }))).toThrow(SchemaError);
    expect(() => parseSidecar(withVideo({ seek_index: [[0, 0], [2000, 999]] }))).not.toThrow();
  });

  it('rejects a complete clip missing its end time or plaintext size', () => {
    expect(() => parseSidecar({ ...good(), end_time: null })).toThrow(SchemaError);
    expect(() => parseSidecar(withVideo({ plaintext_size_bytes: null }))).toThrow(SchemaError);
  });

  it('accepts an incomplete clip with null end, duration and plaintext size', () => {
    const s: Record<string, unknown> = { ...good(), status: 'incomplete', end_time: null, duration_ms: null, end_reason: 'recovered' };
    s.video = { ...(s.video as object), plaintext_size_bytes: null, seek_index: [] };
    expect(parseSidecar(s).status).toBe('incomplete');
  });

  it('accepts the null seek_index the Go writer emits for a recovered clip and normalises it to []', () => {
    const s: Record<string, unknown> = { ...good(), status: 'incomplete', end_time: null, duration_ms: null, end_reason: 'recovered' };
    s.video = { ...(s.video as object), plaintext_size_bytes: null, seek_index: null };
    expect(parseSidecar(s).video.seek_index).toEqual([]);
    expect(parseClipList([s]).clips[0]!.video.seek_index).toEqual([]);
  });

  it('still rejects a null or missing seek_index on a complete full sidecar', () => {
    expect(() => parseSidecar(withVideo({ seek_index: null }))).toThrow(SchemaError);
  });

  it('accepts an empty but present seek_index on a complete clip, like sidecar.Validate', () => {
    expect(parseSidecar(withVideo({ seek_index: [] })).video.seek_index).toEqual([]);
  });

  it('rejects init_segment_bytes of 0, like sidecar.Validate', () => {
    expect(() => parseSidecar(withVideo({ init_segment_bytes: 0 }))).toThrow(SchemaError);
    expect(() => parseSidecar(withVideo({ init_segment_bytes: 1 }))).not.toThrow();
  });

  it('requires seek_index on a complete full sidecar', () => {
    const s = good();
    const { seek_index: _drop, ...video } = s.video as Record<string, unknown>;
    void _drop;
    expect(() => parseSidecar({ ...s, video })).toThrow(SchemaError);
  });

  it('rejects negative or fractional numbers', () => {
    expect(() => parseSidecar({ ...good(), pre_roll_ms: -1 })).toThrow(SchemaError);
    expect(() => parseSidecar(withVideo({ size_bytes: 1.5 }))).toThrow(SchemaError);
  });

  it('rejects a bad key_id hint and an unknown enum', () => {
    expect(() => parseSidecar(withVideo({ key_id: 'zz' }))).toThrow(SchemaError);
    expect(() => parseSidecar({ ...good(), end_reason: 'because' })).toThrow(SchemaError);
  });
});

describe('list, cameras, status, coverage', () => {
  it('counts invalid clip summaries instead of serving them', () => {
    const { seek_index: _drop, ...video } = good().video as Record<string, unknown>;
    void _drop;
    const ok = { ...good(), video };
    const bad = { ...good(), schema: 'zrec.sidecar/9' };
    const out = parseClipList([ok, bad]);
    expect(out.clips).toHaveLength(1);
    expect(out.invalid).toBe(1);
  });

  it('accepts the {clips: [...]} envelope too', () => {
    expect(parseClipList({ clips: [good()] }).clips).toHaveLength(1);
  });

  it('caps camera display strings', () => {
    const cams = parseCameras([
      { id: 'front-door', name: 'n'.repeat(999), brand: 'tapo', tags: [], last_clip_at: null, online: true },
    ]);
    expect(cams[0]!.name).toHaveLength(128);
  });

  it('validates status and coverage shapes', () => {
    expect(
      parseStatus({
        schema: 'zrec.status/1',
        relay_id: 'home-mini',
        version: '2.0.0',
        updated_at: '2026-09-16T15:00:00Z',
        key_id: 'a1b2c3d4e5f60718',
        cameras: [],
        indexed_days: 2,
      }).relay_id,
    ).toBe('home-mini');
    expect(() => parseStatus({ schema: 'zrec.status/2' })).toThrow(SchemaError);
    expect(parseCoverage({ ready: [['2026-09-16T14:00:00Z', '2026-09-16T15:00:00Z']], dropped: [] }).ready).toHaveLength(1);
    expect(() => parseCoverage({ ready: 'nope', dropped: [] })).toThrow(SchemaError);
  });
});
