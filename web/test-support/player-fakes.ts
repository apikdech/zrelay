// In-memory stand-ins for the media stack and the crypto worker, so the playback
// engine can be unit-tested in Node. The fake SourceBuffer derives buffered time
// from the bytes it is given: a chunk whose first byte is `c` covers
// [c, c + 1) seconds, which matches the fixture clip below (one second per
// 64 KiB chunk, one fragment per chunk).
import { readFileSync } from 'node:fs';
import type { ClipOpenResult, ClipReadResult, Request } from '../src/worker/protocol';

export const CHUNK = 65536;

export type R = [number, number];

/**
 * A clip's byte layout, for a SourceBuffer that parses like the real thing: bytes only become buffered
 * time once a whole fragment has been appended in one unbroken run, and abort() drops a partial one.
 * Chunk `c` is `CHUNK` bytes all equal to `c`, so an append's plaintext offset is recoverable from it.
 */
export interface Layout {
  /** `video.seek_index`: [t_ms, plaintext offset] per fragment. */
  seekIndex: [number, number][];
  /** Bytes of the init segment, at the start of the plaintext. */
  initBytes: number;
  /** Plaintext length (a whole number of chunks in the fake). */
  totalBytes: number;
  durationMs: number;
}

const FIXTURE_SIDECAR = new URL(
  '../../testdata/clips/complete/zrelay/clips/front-door/2026/11/06/171606359_01M6RXRJAQRJY105FSDF0QRBH0.json',
  import.meta.url,
);

/**
 * A clip laid out like the committed fixture: 2 s fragments at the fixture's own seek_index offsets
 * (mid-chunk, spanning chunk boundaries, several per chunk), its fragment sizes repeating to `durationMs`.
 * Fewer than 256 chunks, so the chunk number fits the fake's one-byte fill.
 */
export function fixtureLayout(durationMs = 120_000): Layout {
  const sc = JSON.parse(readFileSync(FIXTURE_SIDECAR, 'utf8')) as {
    video: { seek_index: [number, number][]; init_segment_bytes: number; plaintext_size_bytes: number };
  };
  const src = sc.video.seek_index;
  const sizes = src.map(([, off], i) => (src[i + 1]?.[1] ?? sc.video.plaintext_size_bytes) - off);
  const seekIndex: [number, number][] = [];
  let off = sc.video.init_segment_bytes;
  for (let i = 0; i * 2000 < durationMs; i++) {
    seekIndex.push([i * 2000, off]);
    off += sizes[i % sizes.length]!;
  }
  const totalBytes = Math.ceil(off / CHUNK) * CHUNK;
  if (totalBytes / CHUNK > 255) throw new Error('layout too long for the fake');
  return { seekIndex, initBytes: sc.video.init_segment_bytes, totalBytes, durationMs };
}

class FakeRanges {
  constructor(private readonly r: R[]) {}
  get length(): number {
    return this.r.length;
  }
  start(i: number): number {
    return this.r[i]![0];
  }
  end(i: number): number {
    return this.r[i]![1];
  }
}

function mergeRanges(r: R[]): R[] {
  const s = [...r].sort((a, b) => a[0] - b[0]);
  const out: R[] = [];
  for (const x of s) {
    const last = out[out.length - 1];
    if (last && x[0] <= last[1]) last[1] = Math.max(last[1], x[1]);
    else out.push([x[0], x[1]]);
  }
  return out;
}

function subtract(r: R[], a: number, b: number): R[] {
  const out: R[] = [];
  for (const [s, e] of r) {
    if (e <= a || s >= b) out.push([s, e]);
    else {
      if (s < a) out.push([s, a]);
      if (e > b) out.push([b, e]);
    }
  }
  return out;
}

export class FakeSourceBuffer extends EventTarget {
  mode = 'segments';
  updating = false;
  ranges: R[] = [];
  /** Every appendBuffer call, in order. */
  appends: Uint8Array[] = [];
  removes: R[] = [];
  aborts = 0;
  /** Number of upcoming appendBuffer calls that throw QuotaExceededError. */
  quotaFailures = 0;
  /** Set by the media source it belongs to; appending to an ended source reopens it. */
  source: FakeMediaSource | null = null;
  /** With a layout, buffered time comes from completed fragments instead of the chunk number. */
  layout: Layout | null = null;
  /**
   * WebKit mode (needs a layout): a fragment that is only partly appended already counts as buffered time,
   * in proportion to the bytes parsed so far (WebKit reports the frames it has decoded), and an abort() keeps them.
   */
  webkit = false;
  /** Appends that broke the parser's rules (a gap without abort, a fragment-less start after abort). */
  violations: string[] = [];
  private known = false;
  private pos = 0;
  /** Index of the fragment being assembled; -1 while in the init segment. */
  private asm = -1;
  get buffered(): FakeRanges {
    return new FakeRanges(this.ranges);
  }

  private finish(fn: () => void): void {
    this.updating = true;
    queueMicrotask(() => {
      fn();
      this.updating = false;
      this.dispatchEvent(new Event('updateend'));
    });
  }

  appendBuffer(data: BufferSource): void {
    if (this.updating) throw new DOMException('still updating', 'InvalidStateError');
    if (this.quotaFailures > 0) {
      this.quotaFailures--;
      throw new DOMException('quota', 'QuotaExceededError');
    }
    const u8 = ArrayBuffer.isView(data)
      ? new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
      : new Uint8Array(data);
    this.appends.push(u8.slice());
    if (this.source?.readyState === 'ended') this.source.readyState = 'open';
    const c = u8[0] ?? 0;
    if (this.layout) {
      const end = (c + 1) * CHUNK;
      const added = this.parse(end - u8.length, end);
      this.finish(() => {
        this.ranges = mergeRanges([...this.ranges, ...added]);
      });
      return;
    }
    this.finish(() => {
      this.ranges = mergeRanges([...this.ranges, [c, c + 1]]);
    });
  }

  /** The segment parser: consumes plaintext bytes [start, end) and returns the time ranges that completed. */
  private parse(start: number, end: number): R[] {
    const l = this.layout!;
    const at = l.seekIndex.findIndex(([, o]) => o === start);
    // Between segments (after a reset, or with the last fragment complete) any fragment may start next, as in
    // a real parser; in the middle of one the bytes must continue where they stopped.
    const f = l.seekIndex[this.asm];
    const atBoundary = !this.known || (this.asm === -1 ? this.pos === 0 : f === undefined || this.pos === f[1]);
    if (atBoundary) {
      if (start === 0) this.asm = -1;
      else if (at >= 0) this.asm = at;
      else {
        this.violations.push(`append at ${start} after a parser reset is not a fragment start`);
        this.known = false;
        return [];
      }
      this.known = true;
    } else if (start !== this.pos) {
      this.violations.push(`append at ${start} but the parser expected ${this.pos}`);
      this.known = false;
      return [];
    }
    this.pos = end;
    const done: R[] = [];
    for (;;) {
      if (this.asm === -1) {
        if (end < l.initBytes) break;
        this.asm = 0;
        continue;
      }
      const f = l.seekIndex[this.asm];
      if (!f) break;
      const fEnd = l.seekIndex[this.asm + 1]?.[1] ?? l.totalBytes;
      if (end < fEnd) {
        const dur = (l.seekIndex[this.asm + 1]?.[0] ?? l.durationMs) - f[0];
        const frac = Math.max(0, end - f[1]) / (fEnd - f[1]);
        if (this.webkit && frac > 0) done.push([f[0] / 1000, (f[0] + dur * frac) / 1000]);
        break;
      }
      done.push([f[0] / 1000, (l.seekIndex[this.asm + 1]?.[0] ?? l.durationMs) / 1000]);
      this.asm++;
    }
    return done;
  }

  remove(start: number, end: number): void {
    if (this.updating) throw new DOMException('still updating', 'InvalidStateError');
    this.removes.push([start, end]);
    if (this.source?.readyState === 'ended') this.source.readyState = 'open';
    this.finish(() => {
      this.ranges = subtract(this.ranges, start, end);
    });
  }

  abort(): void {
    this.aborts++;
    this.known = false; // the partial fragment, if any, is dropped
  }
}

export class FakeMediaSource extends EventTarget {
  static supported = true;
  static isTypeSupported(): boolean {
    return this.supported;
  }
  readyState: 'closed' | 'open' | 'ended' = 'closed';
  sb: FakeSourceBuffer | null = null;
  mime = '';
  /** Applied to every SourceBuffer this source creates. */
  layout: Layout | null = null;
  webkit = false;
  endedWith: string | undefined | null = null;
  addSourceBuffer(mime: string): FakeSourceBuffer {
    this.mime = mime;
    this.sb = new FakeSourceBuffer();
    this.sb.source = this;
    this.sb.layout = this.layout;
    this.sb.webkit = this.webkit;
    return this.sb;
  }
  endOfStream(err?: string): void {
    this.readyState = 'ended';
    this.endedWith = err;
  }
}

export class FakeManagedMediaSource extends FakeMediaSource {
  static override supported = true;
  static override isTypeSupported(): boolean {
    return this.supported;
  }
}

/** The part of HTMLVideoElement the engine touches. */
export class FakeVideo extends EventTarget {
  currentTime = 0;
  disableRemotePlayback = false;
  paused = true;
  ended = false;
  canPlay = 'probably';
  private _src = '';
  attrs = new Map<string, string>();
  loads = 0;
  constructor(private readonly registry: Map<string, FakeMediaSource | Blob>) {
    super();
  }
  get src(): string {
    return this._src;
  }
  set src(v: string) {
    this._src = v;
    const o = this.registry.get(v);
    if (o instanceof FakeMediaSource) {
      queueMicrotask(() => {
        o.readyState = 'open';
        o.dispatchEvent(new Event('sourceopen'));
      });
    }
  }
  canPlayType(): string {
    return this.canPlay;
  }
  removeAttribute(n: string): void {
    if (n === 'src') this._src = '';
  }
  load(): void {
    this.loads++;
  }
  play(): Promise<void> {
    this.paused = false;
    return Promise.resolve();
  }
  fire(type: string): void {
    this.dispatchEvent(new Event(type));
  }
}

export interface Env {
  video: FakeVideo;
  registry: Map<string, FakeMediaSource | Blob>;
  revoked: string[];
  restore(): void;
  mediaSources(): FakeMediaSource[];
}

type G = Record<string, unknown>;

/** Installs MediaSource/ManagedMediaSource globals and URL.createObjectURL stubs. */
export function installMedia(opts: { mms?: boolean; mse?: boolean; layout?: Layout; webkit?: boolean } = {}): Env {
  const g = globalThis as unknown as G;
  const saved = {
    MediaSource: g.MediaSource,
    ManagedMediaSource: g.ManagedMediaSource,
    create: URL.createObjectURL,
    revoke: URL.revokeObjectURL,
  };
  const registry = new Map<string, FakeMediaSource | Blob>();
  const created: FakeMediaSource[] = [];
  const revoked: string[] = [];
  let n = 0;

  FakeMediaSource.supported = true;
  FakeManagedMediaSource.supported = true;
  const MS = class extends FakeMediaSource {
    constructor() {
      super();
      this.layout = opts.layout ?? null;
      this.webkit = opts.webkit ?? false;
      created.push(this);
    }
  };
  const MMS = class extends FakeManagedMediaSource {
    constructor() {
      super();
      this.layout = opts.layout ?? null;
      this.webkit = opts.webkit ?? false;
      created.push(this);
    }
  };
  g.MediaSource = opts.mse === false ? undefined : MS;
  g.ManagedMediaSource = opts.mms ? MMS : undefined;
  URL.createObjectURL = ((o: FakeMediaSource | Blob) => {
    const u = `blob:fake/${++n}`;
    registry.set(u, o);
    return u;
  }) as typeof URL.createObjectURL;
  URL.revokeObjectURL = ((u: string) => {
    revoked.push(u);
  }) as typeof URL.revokeObjectURL;

  const env: Env = {
    video: new FakeVideo(registry),
    registry,
    revoked,
    mediaSources: () => created,
    restore() {
      g.MediaSource = saved.MediaSource;
      g.ManagedMediaSource = saved.ManagedMediaSource;
      URL.createObjectURL = saved.create;
      URL.revokeObjectURL = saved.revoke;
    },
  };
  (env as unknown as { MS: unknown; MMS: unknown }).MS = MS;
  (env as unknown as { MS: unknown; MMS: unknown }).MMS = MMS;
  return env;
}

// ---- fake worker -----------------------------------------------------------

/** Plaintext of chunk c: CHUNK bytes, all equal to c (so the fake SourceBuffer can place it). */
export const chunkBytes = (c: number): ArrayBuffer => new Uint8Array(CHUNK).fill(c).buffer;

export function openResult(over: Partial<ClipOpenResult> = {}): ClipOpenResult {
  const n = over.numChunks ?? 10;
  return {
    header: {
      formatVersion: 1,
      suite: 1,
      headerLen: 105,
      chunkSize: CHUNK,
      clipId: '01M2NBMVS0K8EKRQKGGAJCDT0B',
      startUnixMs: 1_700_000_000_000,
      cameraId: 'front-door',
      keyId: '24f301fa8d9378d6',
    },
    verification: { camera: 'match', start: 'match', clipId: 'match' },
    numChunks: n,
    codec: 'avc1.640016',
    initSegmentBytes: 686,
    // One 1 s fragment per chunk; chunk 5 also has a fragment 100 bytes in.
    seekIndex: Array.from({ length: n }, (_, c) => [c * 1000, c * CHUNK] as [number, number]),
    status: 'complete',
    plaintextSizeBytes: n * CHUNK,
    ...over,
  };
}

/** An `openResult` for a clip with this byte layout. */
export function layoutOpen(l: Layout): ClipOpenResult {
  return openResult({
    numChunks: l.totalBytes / CHUNK,
    seekIndex: l.seekIndex,
    initSegmentBytes: l.initBytes,
    plaintextSizeBytes: l.totalBytes,
  });
}

export interface FakeClientOpts {
  open?: ClipOpenResult | Error;
  /** Per-chunk override: throw to fail a read. */
  read?: (chunk: number) => ClipReadResult | Promise<ClipReadResult> | Error;
  sidecar?: unknown;
  clips?: unknown;
}

export class FakeClient {
  calls: Request[] = [];
  reads: number[] = [];
  /** Resolvers for reads held by `hold`. */
  held = new Map<number, () => void>();
  hold = new Set<number>();
  private waiting = new Map<number, (() => void)[]>();
  /** Stops holding new reads and frees every read held so far. */
  releaseAll(): void {
    this.hold.clear();
    for (const f of [...this.held.values()]) f();
  }
  constructor(public opts: FakeClientOpts = {}) {}

  async call<T = unknown>(msg: Request): Promise<T> {
    this.calls.push(msg);
    switch (msg.type) {
      case 'clip.open': {
        const o = this.opts.open ?? openResult();
        if (o instanceof Error) throw o;
        return o as T;
      }
      case 'clip.read': {
        this.reads.push(msg.chunk);
        if (this.hold.has(msg.chunk)) {
          // Every read of a held chunk waits, however many are in flight; `held.get(c)()` frees them all.
          await new Promise<void>((r) => {
            const w = this.waiting.get(msg.chunk) ?? [];
            w.push(r);
            this.waiting.set(msg.chunk, w);
            this.held.set(msg.chunk, () => {
              for (const x of this.waiting.get(msg.chunk) ?? []) x();
              this.waiting.delete(msg.chunk);
            });
          });
        }
        const n = (this.opts.open && !(this.opts.open instanceof Error) ? this.opts.open.numChunks : 10) as number;
        const custom = this.opts.read?.(msg.chunk);
        if (custom instanceof Error) throw custom;
        if (custom) return (await custom) as T;
        return { plaintext: chunkBytes(msg.chunk), final: msg.chunk === n - 1 } as T;
      }
      case 'api.get': {
        if (msg.path === '/api/v1/clips') return (this.opts.clips ?? []) as T;
        return (this.opts.sidecar ?? {}) as T;
      }
      default:
        return undefined as T;
    }
  }
}

export const tick = async (n = 20): Promise<void> => {
  for (let i = 0; i < n; i++) await new Promise<void>((r) => queueMicrotask(r));
};

/** Polls `cond` over macrotasks until true. */
export async function until(cond: () => boolean, what = 'condition'): Promise<void> {
  for (let i = 0; i < 400; i++) {
    try {
      if (cond()) return;
    } catch {
      /* not ready yet (e.g. the SourceBuffer is not created) */
    }
    await new Promise<void>((r) => setTimeout(r, 2));
  }
  throw new Error(`timed out waiting for ${what}`);
}
