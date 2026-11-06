// Shared fixtures for the crypto/data worker tests: the cross-language zrec
// vectors served by a fetch mock that behaves like zview-server (Bearer auth,
// Range requests, Content-Range totals).
import { readFileSync } from 'node:fs';
import { deriveApiToken, importMasterKey } from '../src/lib/zrec/keys';
import { fromHex, toHex, type Manifest } from './vectors-runner';

const dir = new URL('../../testdata/zrec/v1/', import.meta.url);
export const manifest: Manifest = JSON.parse(readFileSync(new URL('vectors.json', dir), 'utf8'));
export const loadVector = (file: string): Uint8Array => new Uint8Array(readFileSync(new URL(file, dir)));

/** zk1-<hex>-<checksum> text for a raw key (Contract §3.1). */
export async function keyText(hex: string): Promise<string> {
  const raw = fromHex(hex);
  const pre = new Uint8Array(3 + raw.length);
  pre.set(new TextEncoder().encode('zk1'));
  pre.set(raw, 3);
  const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', pre));
  return `zk1-${hex}-${toHex(sum.subarray(0, 2))}`;
}

export const masterText = (): Promise<string> => keyText(manifest.master_key_hex);

/** The Bearer credential the vector master key derives (what zview verifies). */
export async function vectorToken(): Promise<string> {
  return deriveApiToken(await importMasterKey(fromHex(manifest.master_key_hex)));
}

const pad = (n: number, w: number): string => String(n).padStart(w, '0');

/** A schema-valid sidecar for the vector clip, overridable per test. */
export function sidecarFor(
  plaintextLen: number,
  videoLen: number,
  over: Record<string, unknown> = {},
  videoOver: Record<string, unknown> = {},
): Record<string, unknown> {
  const d = new Date(manifest.start_unix_ms);
  const hhmmssmmm =
    pad(d.getUTCHours(), 2) + pad(d.getUTCMinutes(), 2) + pad(d.getUTCSeconds(), 2) + pad(d.getUTCMilliseconds(), 3);
  return {
    schema: 'zrec.sidecar/2',
    status: 'complete',
    clip_id: manifest.clip_id,
    series_id: manifest.clip_id,
    segment_index: 0,
    camera: { id: manifest.camera_id, name: 'Front Door', brand: 'tapo', tags: ['exterior'] },
    start_time: d.toISOString(),
    end_time: new Date(manifest.start_unix_ms + 42500).toISOString(),
    duration_ms: 42500,
    pre_roll_ms: 15800,
    end_reason: 'post_roll_elapsed',
    triggers: [{ type: 'motion', first_at_ms: 15800, last_at_ms: 32400, raw_topics: ['tns1:x'] }],
    video: {
      object: `${hhmmssmmm}_${manifest.clip_id}.zrec`,
      format_version: 1,
      key_id: manifest.derived.key_id_hex,
      chunk_size: manifest.chunk_size,
      size_bytes: videoLen,
      plaintext_size_bytes: plaintextLen,
      container: 'fmp4',
      codec: 'avc1.640032',
      width: 2560,
      height: 1440,
      init_segment_bytes: 1,
      seek_index: [[0, 0]],
      ...videoOver,
    },
    recorder: { relay_id: 'home-mini', version: '2.0.0', time_source: 'rtcp' },
    video_href: `/api/v1/clips/${manifest.clip_id}/video`,
    ...over,
  };
}

export interface Call {
  url: string;
  method: string;
  headers: Headers;
  body?: string;
  signal?: AbortSignal;
}

export interface ServerOpts {
  token: string;
  /** Ciphertext served for the clip (default: the two-and-a-half-chunk vector). */
  video?: Uint8Array;
  /** Sidecar JSON served for the clip. */
  sidecar?: unknown;
  /** Extra routes: `${METHOD} ${path}` to a response factory. */
  routes?: Record<string, (req: Call) => Response | Promise<Response>>;
  /** Awaited before every video Range request that does not start at byte 0. */
  onChunk?: (start: number, end: number, signal?: AbortSignal) => Promise<void>;
}

export function mockServer(opts: ServerOpts) {
  const calls: Call[] = [];
  const video = opts.video ?? loadVector('pos-two-and-half-chunks.zrec');
  const sidecar = opts.sidecar ?? sidecarFor(163840, video.length);
  const json = (v: unknown, status = 200): Response =>
    new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } });

  const fetchFn = async (input: string | URL | Request, init: RequestInit = {}): Promise<Response> => {
    const url = String(input);
    const headers = new Headers(init.headers);
    const call: Call = {
      url,
      method: init.method ?? 'GET',
      headers,
      body: typeof init.body === 'string' ? init.body : undefined,
      signal: init.signal ?? undefined,
    };
    calls.push(call);
    if (init.signal?.aborted) throw new DOMException('aborted', 'AbortError');
    if (headers.get('Authorization') !== `Bearer ${opts.token}`) {
      return json({ title: 'unauthorized' }, 401);
    }
    const path = url.split('?')[0]!;
    const route = opts.routes?.[`${call.method} ${path}`];
    if (route) return route(call);
    if (path === `/api/v1/clips/${manifest.clip_id}`) return json(sidecar);
    if (path === `/api/v1/clips/${manifest.clip_id}/video`) {
      const m = /^bytes=(\d+)-(\d+)$/.exec(headers.get('Range') ?? '');
      if (!m) return new Response(video as BodyInit, { status: 200 });
      const start = Number(m[1]);
      const end = Math.min(Number(m[2]), video.length - 1);
      if (start > 0 && opts.onChunk) {
        await opts.onChunk(start, end + 1, init.signal ?? undefined);
        if (init.signal?.aborted) throw new DOMException('aborted', 'AbortError');
      }
      return new Response(video.slice(start, end + 1) as BodyInit, {
        status: 206,
        headers: { 'Content-Range': `bytes ${start}-${end}/${video.length}` },
      });
    }
    return json({ title: 'not found' }, 404);
  };
  return { fetch: fetchFn as typeof fetch, calls };
}
