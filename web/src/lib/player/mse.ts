// MediaSource / ManagedMediaSource wrapper (ui-spec §4.3, §4.3.1). One
// SourceBuffer in `segments` mode; every operation is serialised because a
// SourceBuffer runs one at a time.
import type { Range } from './buffer';

export type MseKind = 'mms' | 'mse';

type MseCtor = typeof MediaSource;

/** The constructor for `kind` if this browser has it (looked up at call time). */
export function mseCtor(kind: MseKind): MseCtor | undefined {
  const g = globalThis as unknown as { MediaSource?: MseCtor; ManagedMediaSource?: MseCtor };
  const c = kind === 'mms' ? g.ManagedMediaSource : g.MediaSource;
  return typeof c === 'function' ? c : undefined;
}

/** `isTypeSupported` gate of ui-spec §4.3. */
export function mseSupports(kind: MseKind, mime: string): boolean {
  const c = mseCtor(kind);
  return c !== undefined && c.isTypeSupported(mime);
}

export interface Sink {
  readonly kind: MseKind;
  /** False between `endstreaming` and `startstreaming` (ManagedMediaSource only). */
  readonly streaming: boolean;
  /** `endOfStream()` has run and no append or remove has reopened the source since. */
  readonly ended: boolean;
  append(data: Uint8Array): Promise<void>;
  remove(start: number, end: number): Promise<void>;
  /** Resets the segment parser (ui-spec §4.3.2). The init segment stays valid. */
  abort(): Promise<void>;
  buffered(): Range[];
  endOfStream(): Promise<void>;
  destroy(): void;
}

export interface SinkEvents {
  /** ManagedMediaSource asked for data to start (true) or stop (false). */
  onStreaming(on: boolean): void;
}

/**
 * Attaches a new MediaSource of `kind` to `video` and resolves once it is open
 * with a `segments`-mode SourceBuffer for `mime`.
 */
export async function openSink(video: HTMLVideoElement, kind: MseKind, mime: string, events: SinkEvents): Promise<Sink> {
  const Ctor = mseCtor(kind)!;
  const ms = new Ctor();
  // Required for ManagedMediaSource unless an AirPlay alternative is provided.
  if (kind === 'mms') video.disableRemotePlayback = true;
  const url = URL.createObjectURL(ms);
  const opened = new Promise<void>((resolve) => ms.addEventListener('sourceopen', () => resolve(), { once: true }));
  video.src = url;
  await opened;
  const sb = ms.addSourceBuffer(mime);
  sb.mode = 'segments';
  return new MseSink(video, kind, ms, sb, url, events);
}

class MseSink implements Sink {
  streaming = true;
  #chain: Promise<unknown> = Promise.resolve();
  #dead = false;
  /** Removals queued but not finished. */
  #removing: Range[] = [];
  readonly #onStart = () => this.#stream(true);
  readonly #onEnd = () => this.#stream(false);

  constructor(
    private readonly video: HTMLVideoElement,
    readonly kind: MseKind,
    private readonly ms: MediaSource,
    private readonly sb: SourceBuffer,
    private readonly url: string,
    private readonly events: SinkEvents,
  ) {
    if (kind === 'mms') {
      ms.addEventListener('startstreaming', this.#onStart);
      ms.addEventListener('endstreaming', this.#onEnd);
    }
  }

  get ended(): boolean {
    return !this.#dead && this.ms.readyState === 'ended';
  }

  #stream(on: boolean): void {
    this.streaming = on;
    this.events.onStreaming(on);
  }

  /** Runs `fn` after every earlier operation has finished. */
  #queue<T>(fn: () => Promise<T> | T): Promise<T> {
    const p = this.#chain.then(fn);
    this.#chain = p.catch(() => undefined);
    return p;
  }

  /** Starts a SourceBuffer operation and resolves on its `updateend`. */
  #op(start: () => void): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const done = () => {
        this.sb.removeEventListener('updateend', done);
        this.sb.removeEventListener('error', fail);
        resolve();
      };
      const fail = () => {
        this.sb.removeEventListener('updateend', done);
        this.sb.removeEventListener('error', fail);
        reject(new Error('SourceBuffer operation failed'));
      };
      this.sb.addEventListener('updateend', done);
      this.sb.addEventListener('error', fail);
      try {
        start();
      } catch (e) {
        this.sb.removeEventListener('updateend', done);
        this.sb.removeEventListener('error', fail);
        reject(e);
      }
    });
  }

  append(data: Uint8Array): Promise<void> {
    return this.#queue(() => (this.#dead ? undefined : this.#op(() => this.sb.appendBuffer(data as BufferSource))));
  }

  remove(start: number, end: number): Promise<void> {
    // Decisions made while the removal waits in the queue must already see its effect.
    const pending: Range = [start, end];
    this.#removing.push(pending);
    const done = () => {
      this.#removing = this.#removing.filter((r) => r !== pending);
    };
    return this.#queue(() => (this.#dead ? undefined : this.#op(() => this.sb.remove(start, end)))).finally(done);
  }

  abort(): Promise<void> {
    return this.#queue(() => {
      // abort() is only legal while the MediaSource is open; after end-of-stream the
      // parser already sits on a segment boundary.
      if (!this.#dead && this.ms.readyState === 'open') this.sb.abort();
    });
  }

  /** What is buffered once every removal already queued has run. */
  buffered(): Range[] {
    if (this.#dead) return [];
    try {
      const b = this.sb.buffered;
      let out: Range[] = Array.from({ length: b.length }, (_, i) => [b.start(i), b.end(i)] as const);
      for (const [a, z] of this.#removing) {
        out = out.flatMap(([s, e]): Range[] => {
          if (e <= a || s >= z) return [[s, e]];
          const keep: Range[] = [];
          if (s < a) keep.push([s, a]);
          if (e > z) keep.push([z, e]);
          return keep;
        });
      }
      return out;
    } catch {
      return [];
    }
  }

  endOfStream(): Promise<void> {
    return this.#queue(() => {
      if (!this.#dead && this.ms.readyState === 'open') this.ms.endOfStream();
    });
  }

  destroy(): void {
    if (this.#dead) return;
    this.#dead = true;
    this.ms.removeEventListener('startstreaming', this.#onStart);
    this.ms.removeEventListener('endstreaming', this.#onEnd);
    URL.revokeObjectURL(this.url);
    this.video.removeAttribute('src');
    this.video.load();
  }
}
