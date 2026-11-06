import { describe, expect, it } from 'vitest';
import type { Request } from '../../worker/protocol';
import type { Sidecar } from '../../worker/schemas';
import { ASSUMED_MAX_CLIP_MS, loadDay, toCoverage, toTimelineClip } from './data';

const clip = (over: Partial<Sidecar> = {}): Sidecar =>
  ({
    clip_id: '01M6RXRJAQRJY105FSDF0QRBH0',
    camera: { id: 'front-door', name: 'Front Door' },
    status: 'complete',
    start_time: '2026-11-06T14:59:05Z',
    end_time: '2026-11-06T15:00:05Z',
    duration_ms: 60_000,
    triggers: [{ type: 'person', first_at_ms: 1000, last_at_ms: 9000 }],
    ...over,
  }) as unknown as Sidecar;

type Get = Extract<Request, { type: 'api.get' }>;

describe('toTimelineClip', () => {
  it('maps times and triggers', () => {
    expect(toTimelineClip(clip())).toEqual({
      id: '01M6RXRJAQRJY105FSDF0QRBH0',
      cameraId: 'front-door',
      startMs: Date.parse('2026-11-06T14:59:05Z'),
      endMs: Date.parse('2026-11-06T15:00:05Z'),
      triggers: [{ type: 'person', firstAtMs: 1000, lastAtMs: 9000 }],
    });
  });
  it('bounds an incomplete clip without an end by start + 300 s', () => {
    const c = toTimelineClip(clip({ status: 'incomplete', end_time: null, duration_ms: null }));
    expect(c.endMs - c.startMs).toBe(ASSUMED_MAX_CLIP_MS);
    expect(ASSUMED_MAX_CLIP_MS).toBe(300_000);
  });
});

describe('toCoverage', () => {
  it('turns ready intervals into per-camera spans', () => {
    expect(toCoverage('front-door', { ready: [['2026-11-06T00:00:00Z', '2026-11-06T01:00:00Z']], dropped: [] })).toEqual([
      { cameraId: 'front-door', fromMs: Date.parse('2026-11-06T00:00:00Z'), toMs: Date.parse('2026-11-06T01:00:00Z') },
    ]);
  });
});

describe('loadDay', () => {
  function client(calls: Request[]) {
    return {
      async call<T>(msg: Request): Promise<T> {
        calls.push(msg);
        if (msg.type === 'api.get' && msg.path === '/api/v1/clips') return { clips: [clip()], invalid: 2 } as T;
        if (msg.type === 'api.get' && msg.path === '/api/v1/coverage') {
          return { ready: [['2026-11-06T00:00:00Z', '2026-11-06T01:00:00Z']], dropped: [] } as T;
        }
        throw new Error('unexpected');
      },
    };
  }
  const gets = (calls: Request[], path: string) => calls.filter((c): c is Get => c.type === 'api.get' && c.path === path);

  it('asks for the local day in UTC, with the camera and trigger filters', async () => {
    const calls: Request[] = [];
    await loadDay(client(calls), {
      day: '2026-11-06',
      zone: 'Asia/Jakarta',
      camera: 'front-door',
      trigger: 'person',
      cameras: ['front-door', 'yard'],
    });
    expect(gets(calls, '/api/v1/clips')[0]!.query).toEqual({
      from: '2026-11-05T17:00:00.000Z',
      to: '2026-11-06T17:00:00.000Z',
      camera: 'front-door',
      trigger: 'person',
    });
  });

  it('omits the filters when they are empty, and fetches coverage for every camera', async () => {
    const calls: Request[] = [];
    const out = await loadDay(client(calls), {
      day: '2026-11-06',
      zone: 'Asia/Jakarta',
      camera: '',
      trigger: '',
      cameras: ['front-door', 'yard'],
    });
    expect(gets(calls, '/api/v1/clips')[0]!.query).toEqual({
      from: '2026-11-05T17:00:00.000Z',
      to: '2026-11-06T17:00:00.000Z',
    });
    expect(gets(calls, '/api/v1/coverage').map((c) => c.query.camera)).toEqual(['front-door', 'yard']);
    expect(out.coverage.map((c) => c.cameraId)).toEqual(['front-door', 'yard']);
    expect(out.invalid).toBe(2);
    expect(out.clips).toHaveLength(1);
  });

  it('fetches coverage only for the filtered camera', async () => {
    const calls: Request[] = [];
    await loadDay(client(calls), { day: '2026-11-06', zone: 'UTC', camera: 'yard', trigger: '', cameras: ['front-door', 'yard'] });
    expect(gets(calls, '/api/v1/coverage').map((c) => c.query.camera)).toEqual(['yard']);
  });

  it('keeps going when one camera has no readable coverage', async () => {
    const c = {
      async call<T>(msg: Request): Promise<T> {
        if (msg.type === 'api.get' && msg.path === '/api/v1/clips') return { clips: [], invalid: 0 } as T;
        throw Object.assign(new Error('boom'), { name: 'ApiError' });
      },
    };
    const out = await loadDay(c, { day: '2026-11-06', zone: 'UTC', camera: '', trigger: '', cameras: ['a'] });
    expect(out.coverage).toEqual([]);
  });
});
