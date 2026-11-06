// In-memory stand-ins for the media stack and the crypto worker, so the playback
// engine can be unit-tested in Node. The fake SourceBuffer derives buffered time
// from the bytes it is given: a chunk whose first byte is `c` covers
// [c, c + 1) seconds, which matches the fixture clip below (one second per
// 64 KiB chunk, one fragment per chunk).
import type { ClipOpenResult, ClipReadResult, Request } from '../src/worker/protocol';

export const CHUNK = 65536;

export type R = [number, number];

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
  /** Appends still allowed before the next one throws (Infinity: unlimited). */
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
    const c = u8[0] ?? 0;
    this.finish(() => {
      this.ranges = mergeRanges([...this.ranges, [c, c + 1]]);
    });
  }

  remove(start: number, end: number): void {
    if (this.updating) throw new DOMException('still updating', 'InvalidStateError');
    this.removes.push([start, end]);
    this.finish(() => {
      this.ranges = subtract(this.ranges, start, end);
    });
  }

  abort(): void {
    this.aborts++;
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
  endedWith: string | undefined | null = null;
  addSourceBuffer(mime: string): FakeSourceBuffer {
    this.mime = mime;
    this.sb = new FakeSourceBuffer();
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
export function installMedia(opts: { mms?: boolean; mse?: boolean } = {}): Env {
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
      created.push(this);
    }
  };
  const MMS = class extends FakeManagedMediaSource {
    constructor() {
      super();
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
        if (this.hold.has(msg.chunk)) await new Promise<void>((r) => this.held.set(msg.chunk, r));
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
