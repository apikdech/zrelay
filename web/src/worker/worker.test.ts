import { IDBFactory } from 'fake-indexeddb';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  keyText,
  loadVector,
  manifest,
  masterText,
  mockServer,
  sidecarFor,
  vectorToken,
  type ServerOpts,
} from '../../test-support/worker-fixtures';
import { sha256Hex } from '../../test-support/vectors-runner';
import { ErrAuth, ErrUnknownKey } from '../lib/zrec/errors';
import type { Request } from './protocol';
import { readJson } from './api';
import { attachWorker, createWorker } from './worker';

let token: string;
beforeEach(async () => {
  token = await vectorToken();
});

/** A worker over a fake network. Every reply is structured-cloned and recorded, like postMessage. */
function harness(opts: Partial<ServerOpts> = {}, idb = new IDBFactory()) {
  const server = mockServer({ token, ...opts });
  const worker = createWorker({ fetch: server.fetch, indexedDB: idb });
  const replies: unknown[] = [];
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const call = async <T = any>(msg: Request): Promise<T> => {
    const { result, transfer } = await worker.handle(msg);
    const copy = structuredClone(result, { transfer });
    replies.push(copy);
    return copy as T;
  };
  return { call, replies, server, worker, idb };
}

async function importVectorKey(h: ReturnType<typeof harness>, remember = false) {
  return h.call({ type: 'key.import', encoded: await masterText(), remember });
}

const CLIP = manifest.clip_id;

describe('key messages', () => {
  it('imports a key and returns only its key id', async () => {
    const h = harness();
    expect(await importVectorKey(h)).toEqual({ keyId: manifest.derived.key_id_hex });
  });

  it('reports a bad checksum', async () => {
    const h = harness();
    const text = await masterText();
    const typo = text.slice(0, -1) + (text.endsWith('0') ? '1' : '0');
    expect(await h.call({ type: 'key.import', encoded: typo, remember: false })).toEqual({ error: 'checksum' });
  });

  it('reports a malformed key', async () => {
    const h = harness();
    expect(await h.call({ type: 'key.import', encoded: 'zk1-nope', remember: false })).toEqual({ error: 'format' });
    expect(await h.call({ type: 'key.import', encoded: '', remember: false })).toEqual({ error: 'format' });
  });

  it('lists imported keys with their remembered flag', async () => {
    const h = harness();
    await importVectorKey(h, true);
    expect(await h.call({ type: 'key.list' })).toEqual({
      keys: [{ keyId: manifest.derived.key_id_hex, remembered: true }],
    });
  });

  it('a remembered key survives a worker restart; a session key does not', async () => {
    const idb = new IDBFactory();
    const a = harness({}, idb);
    await importVectorKey(a, true);
    const second = await keyText('ff'.repeat(32));
    const { keyId: sessionId } = await a.call({ type: 'key.import', encoded: second, remember: false });

    const b = harness({}, idb);
    const list = await b.call<{ keys: { keyId: string }[] }>({ type: 'key.list' });
    expect(list.keys.map((k) => k.keyId)).toEqual([manifest.derived.key_id_hex]);
    expect(list.keys.map((k) => k.keyId)).not.toContain(sessionId);
    // and it can use it
    expect((await b.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} })).clip_id).toBe(CLIP);
  });

  it('lock discards memory but keeps remembered base keys', async () => {
    const h = harness();
    await importVectorKey(h, true);
    await h.call({ type: 'clip.open', clipId: CLIP });
    await h.call({ type: 'key.lock' });
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 })).rejects.toMatchObject({ name: 'ErrNotOpen' });
    expect((await h.call<{ keys: unknown[] }>({ type: 'key.list' })).keys).toHaveLength(1);
  });

  it('lock drops a session-only key', async () => {
    const h = harness();
    await importVectorKey(h, false);
    await h.call({ type: 'key.lock' });
    expect(await h.call({ type: 'key.list' })).toEqual({ keys: [] });
    await expect(h.call({ type: 'api.get', path: '/api/v1/status', query: {} })).rejects.toMatchObject({ name: 'ErrNoKey' });
  });

  it('forget deletes the key from IndexedDB and locks', async () => {
    const idb = new IDBFactory();
    const a = harness({}, idb);
    await importVectorKey(a, true);
    await a.call({ type: 'key.forget', keyId: manifest.derived.key_id_hex });
    expect(await a.call({ type: 'key.list' })).toEqual({ keys: [] });
    const b = harness({}, idb);
    expect(await b.call({ type: 'key.list' })).toEqual({ keys: [] });
  });
});

describe('api.get', () => {
  it('returns a validated sidecar', async () => {
    const h = harness({ sidecar: sidecarFor(163840, 1) });
    await importVectorKey(h);
    const s = await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    expect(s.camera.id).toBe('front-door');
  });

  it('rejects a sidecar with an unknown major', async () => {
    const h = harness({ sidecar: sidecarFor(163840, 1, { schema: 'zrec.sidecar/3' }) });
    await importVectorKey(h);
    await expect(h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} })).rejects.toMatchObject({
      name: 'ErrSchema',
    });
  });

  it('caps a 10 kB camera name', async () => {
    const sc = sidecarFor(163840, 1, { camera: { id: 'front-door', name: 'N'.repeat(10_240), brand: 't', tags: [] } });
    const h = harness({ sidecar: sc });
    await importVectorKey(h);
    const s = await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    expect(s.camera.name).toHaveLength(128);
  });

  it('encodes repeatable query parameters and sends the bearer token', async () => {
    const h = harness({ routes: { 'GET /api/v1/clips': () => new Response('[]') } });
    await importVectorKey(h);
    await h.call({
      type: 'api.get',
      path: '/api/v1/clips',
      query: { from: '2026-09-16T00:00:00Z', camera: ['a', 'b c'] },
    });
    const c = h.server.calls.at(-1)!;
    expect(c.url).toBe('/api/v1/clips?from=2026-09-16T00%3A00%3A00Z&camera=a&camera=b+c');
    expect(c.headers.get('Authorization')).toBe(`Bearer ${token}`);
  });

  it.each(['//evil.example/x', 'https://evil.example/api/v1/status', '/api/v1/../x', '/other', '/api/v1/unknown'])(
    'refuses path %s without touching the network',
    async (path) => {
      const h = harness();
      await importVectorKey(h);
      await expect(h.call({ type: 'api.get', path, query: {} })).rejects.toThrow();
      expect(h.server.calls).toHaveLength(0);
    },
  );

  it('turns an error response into a typed error', async () => {
    const h = harness({ routes: { 'GET /api/v1/status': () => new Response('{"title":"boom"}', { status: 500 }) } });
    await importVectorKey(h);
    await expect(h.call({ type: 'api.get', path: '/api/v1/status', query: {} })).rejects.toMatchObject({
      name: 'ApiError',
      status: 500,
    });
  });

  it('tries older keys when the newest token is rejected', async () => {
    const h = harness();
    await importVectorKey(h);
    await h.call({ type: 'key.import', encoded: await keyText('ee'.repeat(32)), remember: false });
    const s = await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    expect(s.clip_id).toBe(CLIP);
  });
});

describe('clip.open', () => {
  it('fetches Range bytes=0-168 and derives numChunks from Content-Range', async () => {
    const h = harness();
    await importVectorKey(h);
    const r = await h.call({ type: 'clip.open', clipId: CLIP });
    const video = h.server.calls.filter((c) => c.url.endsWith('/video'));
    expect(video).toHaveLength(1);
    expect(video[0]!.headers.get('Range')).toBe('bytes=0-168');
    expect(video[0]!.headers.get('Authorization')).toBe(`Bearer ${token}`);
    expect(r.verification).toEqual({ camera: 'match', start: 'match', clipId: 'match' });
    expect(r.numChunks).toBe(3);
    expect(r.header.chunkSize).toBe(65536);
    expect(r.header.cameraId).toBe('front-door');
    expect(r.codec).toBe('avc1.640032');
    expect(r.initSegmentBytes).toBe(1);
    expect(r.seekIndex).toEqual([[0, 0]]);
    expect(r.status).toBe('complete');
  });

  it.each([1, 123, 10 ** 12])('ignores a sidecar size_bytes lie (%i)', async (lie) => {
    const h = harness({ sidecar: sidecarFor(163840, lie) });
    await importVectorKey(h);
    expect((await h.call({ type: 'clip.open', clipId: CLIP })).numChunks).toBe(3);
  });

  it('takes chunk_size from the header, not from the sidecar hint', async () => {
    const h = harness({ sidecar: sidecarFor(163840, 1, {}, { chunk_size: 1 << 24 }) });
    await importVectorKey(h);
    const r = await h.call({ type: 'clip.open', clipId: CLIP });
    expect(r.header.chunkSize).toBe(65536);
    expect(r.numChunks).toBe(3);
  });

  it('flags every mismatch between header and sidecar (G3)', async () => {
    const sc = sidecarFor(163840, 1, {
      camera: { id: 'garage', name: 'G', brand: 't', tags: [] },
      start_time: '2026-01-01T00:00:00.000Z',
      clip_id: '01M2NBMVS0K8EKRQKGGAJCDT0C',
    });
    const h = harness({ sidecar: sc });
    await importVectorKey(h);
    expect((await h.call({ type: 'clip.open', clipId: CLIP })).verification).toEqual({
      camera: 'mismatch',
      start: 'mismatch',
      clipId: 'mismatch',
    });
  });

  it('reports ErrUnknownKey when the keyring lacks the clip key', async () => {
    // The server accepts this other key's token, but the clip was written under the vector key.
    const h = harness({ token: await tokenOf('ee'.repeat(32)) });
    await h.call({ type: 'key.import', encoded: await keyText('ee'.repeat(32)), remember: false });
    await expect(h.call({ type: 'clip.open', clipId: CLIP })).rejects.toBeInstanceOf(ErrUnknownKey);
  });

  it('rejects an invalid clip id before any request', async () => {
    const h = harness();
    await importVectorKey(h);
    await expect(h.call({ type: 'clip.open', clipId: '../x' })).rejects.toThrow();
    expect(h.server.calls).toHaveLength(0);
  });

  it('refuses a server that ignores Range', async () => {
    const h = harness({ routes: { ['GET /api/v1/clips/' + CLIP + '/video']: () => new Response(new Uint8Array(300)) } });
    await importVectorKey(h);
    await expect(h.call({ type: 'clip.open', clipId: CLIP })).rejects.toMatchObject({ name: 'ErrRange' });
  });
});

async function tokenOf(hex: string): Promise<string> {
  const { deriveApiToken, importMasterKey } = await import('../lib/zrec/keys');
  const { fromHex } = await import('../../test-support/vectors-runner');
  return deriveApiToken(await importMasterKey(fromHex(hex)));
}

describe('clip.read', () => {
  it('returns verified plaintext as a transferred ArrayBuffer', async () => {
    const h = harness();
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    const parts: Uint8Array[] = [];
    for (let i = 0; i < 3; i++) {
      const { result, transfer } = await h.worker.handle({ type: 'clip.read', clipId: CLIP, chunk: i });
      const r = result as { plaintext: ArrayBuffer; final: boolean };
      expect(transfer).toEqual([r.plaintext]);
      expect(r.final).toBe(i === 2);
      parts.push(new Uint8Array(r.plaintext));
    }
    const all = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let off = 0;
    for (const p of parts) {
      all.set(p, off);
      off += p.length;
    }
    expect(await sha256Hex(all)).toBe(manifest.positive.find((p) => p.name === 'two-and-half-chunks')!.plaintext_sha256_hex);
  });

  it('requests exactly the chunk range with the bearer token', async () => {
    const h = harness();
    await importVectorKey(h);
    const open = await h.call({ type: 'clip.open', clipId: CLIP });
    await h.call({ type: 'clip.read', clipId: CLIP, chunk: 1 });
    const c = h.server.calls.at(-1)!;
    const s = 65536 + 16;
    const start = open.header.headerLen + s;
    expect(c.headers.get('Range')).toBe(`bytes=${start}-${start + s - 1}`);
    expect(c.headers.get('Authorization')).toBe(`Bearer ${token}`);
  });

  it('rejects a tampered chunk with ErrAuth', async () => {
    const h = harness({ video: loadVector('neg-bitflip-chunk.zrec'), sidecar: sidecarFor(131072, 1) });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    expect(await h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 })).toMatchObject({ final: false });
    const err = await h.call({ type: 'clip.read', clipId: CLIP, chunk: 1 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ErrAuth);
    expect((err as ErrAuth).chunk).toBe(1);
  });

  it('reports a damaged last chunk as truncated when the sidecar says incomplete', async () => {
    const video = loadVector('pos-two-chunks.zrec').slice();
    video[video.length - 1] = video[video.length - 1]! ^ 0xff;
    const incomplete = sidecarFor(131072, 1, { status: 'incomplete', end_time: null, duration_ms: null, end_reason: 'recovered' }, { plaintext_size_bytes: null, seek_index: [] });
    const h = harness({ video, sidecar: incomplete });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 1 })).rejects.toMatchObject({ name: 'ErrTruncated' });

    const complete = harness({ video, sidecar: sidecarFor(131072, 1) });
    await importVectorKey(complete);
    await complete.call({ type: 'clip.open', clipId: CLIP });
    await expect(complete.call({ type: 'clip.read', clipId: CLIP, chunk: 1 })).rejects.toMatchObject({ name: 'ErrAuth' });
  });

  it('opens and reads a recovered clip whose sidecar has seek_index null (what pkg/sidecar emits)', async () => {
    const video = loadVector('pos-two-chunks.zrec').slice();
    video[video.length - 1] = video[video.length - 1]! ^ 0xff;
    const recovered = sidecarFor(131072, 1, { status: 'incomplete', end_time: null, duration_ms: null, end_reason: 'recovered' }, { plaintext_size_bytes: null, seek_index: null });
    const h = harness({ video, sidecar: recovered });
    await importVectorKey(h);
    const sc = await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    expect(sc.video.seek_index).toEqual([]);
    const opened = await h.call({ type: 'clip.open', clipId: CLIP });
    expect(opened.status).toBe('incomplete');
    expect(opened.seekIndex).toEqual([]);
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 1 })).rejects.toMatchObject({ name: 'ErrTruncated' });
  });

  it('rejects an out-of-range chunk index', async () => {
    const h = harness();
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 3 })).rejects.toThrow();
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: -1 })).rejects.toThrow();
  });

  it('rejects a chunk response whose Content-Range total changed', async () => {
    let first = true;
    const base = harness();
    const h = harness({
      routes: {
        ['GET /api/v1/clips/' + CLIP + '/video']: async (req) => {
          const r = await base.server.fetch(req.url, { method: 'GET', headers: req.headers });
          if (first) {
            first = false;
            return r;
          }
          const body = new Uint8Array(await r.arrayBuffer());
          return new Response(body, {
            status: 206,
            headers: { 'Content-Range': r.headers.get('Content-Range')!.replace(/\/\d+$/, '/99999999') },
          });
        },
      },
    });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 })).rejects.toMatchObject({ name: 'ErrRange' });
  });

  it('keeps at most 2 chunk fetches in flight per clip', async () => {
    let inflight = 0;
    let max = 0;
    const waiters: (() => void)[] = [];
    const h = harness({
      onChunk: async () => {
        inflight++;
        max = Math.max(max, inflight);
        await new Promise<void>((r) => waiters.push(r));
        inflight--;
      },
    });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    const reads = [0, 1, 2].map((chunk) => h.call({ type: 'clip.read', clipId: CLIP, chunk }));
    // chunk 0 starts at headerLen > 0, so all three go through onChunk
    await vi_waitFor(() => waiters.length >= 2);
    expect(inflight).toBe(2);
    waiters.splice(0).forEach((r) => r());
    await vi_waitFor(() => waiters.length >= 1);
    waiters.splice(0).forEach((r) => r());
    await Promise.all(reads);
    expect(max).toBe(2);
  });

  it('clip.close aborts in-flight fetches and drops the DEK', async () => {
    const signals: (AbortSignal | undefined)[] = [];
    const h = harness({
      onChunk: (_s, _e, signal) =>
        new Promise<void>((resolve) => {
          signals.push(signal);
          signal?.addEventListener('abort', () => resolve());
        }),
    });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    const pending = h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 }).catch((e: unknown) => e);
    const queued = [1, 2].map((chunk) => h.call({ type: 'clip.read', clipId: CLIP, chunk }).catch((e: unknown) => e));
    await vi_waitFor(() => signals.length >= 2);
    await h.call({ type: 'clip.close', clipId: CLIP });
    expect(signals.every((s) => s?.aborted)).toBe(true);
    for (const e of await Promise.all([pending, ...queued])) expect((e as Error).name).toBe('AbortError');
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 })).rejects.toMatchObject({ name: 'ErrNotOpen' });
  });
});

async function vi_waitFor(cond: () => boolean): Promise<void> {
  for (let i = 0; i < 1000 && !cond(); i++) await new Promise((r) => setTimeout(r, 0));
  if (!cond()) throw new Error('condition never became true');
}

describe('live', () => {
  const sdpAnswer = 'v=0\r\nanswer\r\n';
  const route = (status = 201) => ({
    'POST /live/front-door': () =>
      new Response(sdpAnswer, {
        status,
        headers: { 'Content-Type': 'application/sdp', Location: '/live/front-door/sess-123' },
      }),
  });

  it('live.offer sends a bearer-authenticated SDP offer and returns the answer', async () => {
    const h = harness({ routes: route() });
    await importVectorKey(h);
    const r = await h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'v=0\r\noffer\r\n' });
    expect(r).toEqual({ sdp: sdpAnswer, sessionPath: '/live/front-door/sess-123' });
    const c = h.server.calls.at(-1)!;
    expect(c.method).toBe('POST');
    expect(c.headers.get('Authorization')).toBe(`Bearer ${token}`);
    expect(c.headers.get('Content-Type')).toBe('application/sdp');
    expect(c.body).toBe('v=0\r\noffer\r\n');
  });

  it('keeps only the path of an absolute Location header', async () => {
    const h = harness({
      routes: {
        'POST /live/front-door': () =>
          new Response(sdpAnswer, { status: 201, headers: { Location: 'https://evil.example/live/front-door/s1' } }),
      },
    });
    await importVectorKey(h);
    const r = await h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'x' });
    expect(r.sessionPath).toBe('/live/front-door/s1');
  });

  it('refuses a Location outside the camera session space', async () => {
    const h = harness({
      routes: { 'POST /live/front-door': () => new Response(sdpAnswer, { status: 201, headers: { Location: '/api/v1/status' } }) },
    });
    await importVectorKey(h);
    await expect(h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'x' })).rejects.toMatchObject({ name: 'LiveError' });
  });

  it('maps 503 to the viewer-limit error', async () => {
    const h = harness({ routes: { 'POST /live/front-door': () => new Response('{}', { status: 503 }) } });
    await importVectorKey(h);
    await expect(h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'x' })).rejects.toMatchObject({
      name: 'LiveBusy',
    });
  });

  it('rejects a bad camera id and an oversized offer without a request', async () => {
    const h = harness({ routes: route() });
    await importVectorKey(h);
    await expect(h.call({ type: 'live.offer', cameraId: '../x', sdp: 'x' })).rejects.toThrow();
    await expect(h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'x'.repeat(65 * 1024) })).rejects.toThrow();
    expect(h.server.calls).toHaveLength(0);
  });

  it('live.close sends an authenticated DELETE', async () => {
    const h = harness({ routes: { 'DELETE /live/front-door/sess-123': () => new Response(null, { status: 204 }) } });
    await importVectorKey(h);
    await h.call({ type: 'live.close', sessionPath: '/live/front-door/sess-123' });
    const c = h.server.calls.at(-1)!;
    expect(c.method).toBe('DELETE');
    expect(c.headers.get('Authorization')).toBe(`Bearer ${token}`);
  });

  it('live.close refuses paths outside /live/<camera>/<session>', async () => {
    const h = harness();
    await importVectorKey(h);
    for (const p of ['/api/v1/status', '/live/front-door', '//evil/live/a/b', '/live/a/../b']) {
      await expect(h.call({ type: 'live.close', sessionPath: p })).rejects.toThrow();
    }
    expect(h.server.calls).toHaveLength(0);
  });
});

describe('main thread never receives key material', () => {
  it('no reply carries a 32-byte array, a zk1- string, the token or key hex', async () => {
    const h = harness({
      routes: {
        'POST /live/front-door': () => new Response('v=0', { status: 201, headers: { Location: '/live/front-door/s' } }),
      },
    });
    const secrets = [manifest.master_key_hex, manifest.derived.k_wrap_hex, manifest.derived.k_api_hex, token];
    await h.call({ type: 'key.import', encoded: await masterText(), remember: true });
    await h.call({ type: 'key.list' });
    await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    await h.call({ type: 'clip.open', clipId: CLIP });
    const readReplies: unknown[] = [];
    for (let i = 0; i < 3; i++) readReplies.push(await h.call({ type: 'clip.read', clipId: CLIP, chunk: i }));
    await h.call({ type: 'live.offer', cameraId: 'front-door', sdp: 'o' });
    const errs = [await h.call({ type: 'key.import', encoded: 'zk1-' + 'a'.repeat(64) + '-0000', remember: false })];

    const hits: string[] = [];
    const walk = (v: unknown, path: string, isPlaintext: boolean) => {
      if (typeof v === 'string') {
        if (v.includes('zk1-')) hits.push(`${path}: zk1- string`);
        for (const s of secrets) if (v.toLowerCase().includes(s.toLowerCase())) hits.push(`${path}: secret`);
      } else if (v instanceof ArrayBuffer || ArrayBuffer.isView(v)) {
        if (!isPlaintext && (v as ArrayBufferView | ArrayBuffer).byteLength === 32) hits.push(`${path}: 32 bytes`);
        if (!isPlaintext) hits.push(`${path}: binary in a non-plaintext reply`);
      } else if (Array.isArray(v)) {
        if (v.length === 32 && v.every((x) => typeof x === 'number' && x >= 0 && x <= 255)) hits.push(`${path}: 32 numbers`);
        v.forEach((x, i) => walk(x, `${path}[${i}]`, isPlaintext));
      } else if (v && typeof v === 'object') {
        for (const [k, x] of Object.entries(v)) walk(x, `${path}.${k}`, isPlaintext || k === 'plaintext');
      }
    };
    h.replies.forEach((r, i) => walk(r, `reply${i}`, false));
    expect(hits).toEqual([]);
    expect(readReplies).toHaveLength(3);
    expect(errs[0]).toEqual({ error: 'checksum' });
  });

  it('error messages never contain the key text or token', async () => {
    const h = harness({ routes: { 'GET /api/v1/status': () => new Response('x', { status: 500 }) } });
    const text = await masterText();
    await h.call({ type: 'key.import', encoded: text, remember: false });
    const err = await h.call({ type: 'api.get', path: '/api/v1/status', query: {} }).catch((e: Error) => e);
    const s = JSON.stringify({ n: (err as Error).name, m: (err as Error).message });
    expect(s).not.toContain(token);
    expect(s).not.toContain('zk1-');
  });
});

describe('token fallback', () => {
  it('remembers the token that worked instead of drawing a 401 on every request', async () => {
    const h = harness();
    await importVectorKey(h);
    // A newer, unrelated key that the server does not accept.
    await h.call({ type: 'key.import', encoded: await keyText('ee'.repeat(32)), remember: false });
    for (let i = 0; i < 4; i++) {
      await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    }
    const rejected = h.server.calls.filter((c) => c.headers.get('Authorization') !== `Bearer ${token}`);
    expect(rejected).toHaveLength(1);
  });
});

describe('token memory after lock and forget', () => {
  /** Pin the vector token, lock or forget, re-import both keys, and report the first token tried. */
  async function firstTokenAfter(action: 'key.lock' | 'key.forget') {
    const h = harness();
    const { keyId } = await importVectorKey(h);
    await h.call({ type: 'key.import', encoded: await keyText('ee'.repeat(32)), remember: false });
    await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} }); // pins the vector token
    if (action === 'key.lock') await h.call({ type: 'key.lock' });
    else await h.call({ type: 'key.forget', keyId });
    await h.call({ type: 'key.lock' });
    // Re-import: the unaccepted key is newest, so a stale pin would show as the vector token first.
    await importVectorKey(h);
    await h.call({ type: 'key.import', encoded: await keyText('ee'.repeat(32)), remember: false });
    const before = h.server.calls.length;
    await h.call({ type: 'api.get', path: `/api/v1/clips/${CLIP}`, query: {} });
    return h.server.calls[before]!.headers.get('Authorization');
  }

  it('key.lock drops the pinned token, so the next request starts from the newest key', async () => {
    expect(await firstTokenAfter('key.lock')).not.toBe(`Bearer ${token}`);
  });

  it('key.forget drops the pinned token', async () => {
    expect(await firstTokenAfter('key.forget')).not.toBe(`Bearer ${token}`);
  });
});

describe('response size caps', () => {
  /** A very long body; counts how much was pulled and whether it was cancelled. */
  function endless(chunkBytes: number) {
    const state = { pulled: 0, cancelled: false };
    const body = new ReadableStream<Uint8Array>({
      pull(ctl) {
        // "Endless" is bounded at 48 MiB so a reader that never stops cannot exhaust memory.
        if (state.pulled >= 48 << 20) return ctl.close();
        state.pulled += chunkBytes;
        ctl.enqueue(new Uint8Array(chunkBytes));
      },
      cancel() {
        state.cancelled = true;
      },
    });
    return { state, body };
  }

  it('readJson refuses an oversized Content-Length without reading the body', async () => {
    const { state, body } = endless(1 << 20);
    const res = new Response(body, { headers: { 'Content-Length': String(64 << 20) } });
    await expect(readJson(res, (j) => j)).rejects.toMatchObject({ name: 'ErrSchema' });
    expect(state.cancelled).toBe(true);
    expect(state.pulled).toBeLessThanOrEqual(2 << 20); // only the stream's own prefetch, nothing read by us
  });

  it('readJson aborts a streamed body once the cap is exceeded', async () => {
    const { state, body } = endless(1 << 20);
    await expect(readJson(new Response(body), (j) => j)).rejects.toMatchObject({ name: 'ErrSchema' });
    expect(state.cancelled).toBe(true);
    expect(state.pulled).toBeLessThanOrEqual(20 << 20);
  });

  it('clip.open aborts a header response longer than the requested range', async () => {
    const { state, body } = endless(4096);
    const h = harness({
      routes: {
        ['GET /api/v1/clips/' + CLIP + '/video']: () =>
          new Response(body, { status: 206, headers: { 'Content-Range': 'bytes 0-168/200000' } }),
      },
    });
    await importVectorKey(h);
    await expect(h.call({ type: 'clip.open', clipId: CLIP })).rejects.toMatchObject({ name: 'ErrRange' });
    expect(state.cancelled).toBe(true);
    expect(state.pulled).toBeLessThan(1 << 20);
  });

  it('clip.read aborts a chunk response longer than the requested range', async () => {
    const base = harness();
    const { state, body } = endless(65536);
    let served = 0;
    const h = harness({
      routes: {
        ['GET /api/v1/clips/' + CLIP + '/video']: async (req) => {
          if (served++ === 0) return base.server.fetch(req.url, { method: 'GET', headers: req.headers });
          const m = /^bytes=(\d+)-(\d+)$/.exec(req.headers.get('Range')!)!;
          return new Response(body, { status: 206, headers: { 'Content-Range': `bytes ${m[1]}-${m[2]}/${loadVector('pos-two-and-half-chunks.zrec').length}` } });
        },
      },
    });
    await importVectorKey(h);
    await h.call({ type: 'clip.open', clipId: CLIP });
    await expect(h.call({ type: 'clip.read', clipId: CLIP, chunk: 0 })).rejects.toMatchObject({ name: 'ErrRange' });
    expect(state.cancelled).toBe(true);
    expect(state.pulled).toBeLessThan(1 << 20);
  });
});

describe('attachWorker', () => {
  it('replies with an error for the same id when postMessage of the result throws', async () => {
    const posted: { msg: { id: number; ok: boolean; error?: { name: string } }; transfer: Transferable[] }[] = [];
    const scope = {
      onmessage: null as ((e: { data: unknown }) => void) | null,
      postMessage(msg: unknown, transfer: Transferable[] = []) {
        const m = msg as { id: number; ok: boolean; error?: { name: string } };
        if (m.ok) throw new DOMException('cannot clone', 'DataCloneError');
        posted.push({ msg: m, transfer });
      },
    };
    attachWorker({ handle: async () => ({ result: { fn: () => 1 } }) }, scope);
    scope.onmessage!({ data: { id: 7, msg: { type: 'key.list' } } });
    await vi.waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]!.msg).toMatchObject({ id: 7, ok: false, error: { name: 'DataCloneError' } });
  });
});

describe('worker bootstrap', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('attaches the protocol to the global scope when running inside a Dedicated Worker', async () => {
    class FakeWorkerGlobalScope {}
    const server = mockServer({ token });
    const posted: unknown[] = [];
    const scope = Object.assign(new FakeWorkerGlobalScope(), {
      onmessage: null as ((e: { data: unknown }) => void) | null,
      postMessage: (msg: unknown) => void posted.push(msg),
      fetch: server.fetch,
      indexedDB: new IDBFactory(),
    });
    vi.stubGlobal('WorkerGlobalScope', FakeWorkerGlobalScope);
    vi.stubGlobal('self', scope);
    vi.resetModules();
    await import('./worker');
    expect(scope.onmessage).toBeTypeOf('function');
    scope.onmessage!({ data: { id: 3, msg: { type: 'key.list' } } });
    await vi.waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ id: 3, ok: true, result: { keys: [] } });
  });

  it('does nothing when imported outside a worker', async () => {
    vi.resetModules();
    await expect(import('./worker')).resolves.toBeDefined();
  });
});
