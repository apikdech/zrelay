// Playback engine (ui-spec §4.3, §4.4). Chunks are authenticated and decrypted
// in the crypto worker; this module feeds the plaintext fragmented MP4 to a
// <video> through ManagedMediaSource, MediaSource or a Blob, keeping a bounded
// buffer window, and reports the integrity state of what is playing.
import type { ClipOpenResult, ClipReadResult, Request, Verification } from '../../worker/protocol';
import { ErrAuth, ErrAuthHeader, ErrFormat, ErrTrailingData, ErrTruncated, ErrUnknownKey } from '../zrec/errors';
import type { WorkerClient } from '../workerClient';
import { findNextClip } from './advance';
import { BLOB_LRU, BLOB_MAX, WINDOWS, failureTimeMs, needsMore, planEviction, planSeek, type Profile } from './buffer';
import { BlobLru, blobFromParts } from './blob';
import { mseCtor, mseSupports, openSink, type MseKind, type Sink } from './mse';
import { initialState } from './status';

export type IntegrityState = 'verifying' | 'verified' | 'mismatch' | 'tampered' | 'incomplete' | 'unknown-key';
export type EngineKind = MseKind | 'blob';

export interface PlayerState {
  clipId: string | null;
  integrity: IntegrityState;
  engine: EngineKind | null;
  /** tampered: ~media time (ms) where verification failed, when known. */
  failedAtMs: number | null;
  /** G3 comparison of the encrypted header against the sidecar. */
  verification: Verification | null;
  /** The encrypted header's values, authoritative over the sidecar. */
  header: { cameraId: string; startUnixMs: number } | null;
  /** unknown-key: the key id the clip was recorded with, grouped 4-4-4-4. */
  keyId: string | null;
  /** A user-visible problem that is not an integrity state (unsupported codec, too large, network). */
  message: string | null;
}

export interface Player {
  /** Opens a clip and starts playing it; resolves with the integrity state once the header is verified. */
  load(clipId: string): Promise<IntegrityState>;
  /** Moves the playhead to media time `tMs`; resolves once the data for it is being appended. */
  seek(tMs: number): Promise<void>;
  /** Calls `cb` with the current state now and on every change. Returns the unsubscribe function. */
  onState(cb: (s: PlayerState) => void): () => void;
  /** When on, a finished clip is followed by the next of its series, then of its camera. */
  setAutoAdvance(on: boolean): void;
  destroy(): void;
}

interface BlobEntry {
  url: string;
  outcome: { integrity: 'tampered' | 'incomplete'; failedAtMs: number | null } | null;
}

const isQuota = (e: unknown): boolean => e instanceof DOMException && e.name === 'QuotaExceededError';
const groupKeyId = (hex: string): string => (hex.match(/.{4}/g) ?? [hex]).join('-');

export function createPlayer(video: HTMLVideoElement, client: WorkerClient, opts: { profile: Profile }): Player {
  return new PlayerImpl(video, client, opts.profile);
}

class PlayerImpl implements Player {
  #state: PlayerState = initialState();
  #listeners = new Set<(s: PlayerState) => void>();
  /** Bumped by every load, seek restart and teardown: work from an older value is stale. */
  #gen = 0;
  #clipId: string | null = null;
  #open: ClipOpenResult | null = null;
  #sink: Sink | null = null;
  #waiters: (() => void)[] = [];
  /**
   * While a seek settles, the media time it is moving to (s). `video.currentTime` only
   * follows once the target fragment is buffered, so window decisions read this instead.
   */
  #seekHead: number | null = null;
  #autoAdvance = false;
  #destroyed = false;
  readonly #win;
  readonly #blobs: BlobLru<BlobEntry>;
  readonly #wake = () => this.#signal();
  readonly #onEnded = () => void this.#advance();

  constructor(
    private readonly video: HTMLVideoElement,
    private readonly client: WorkerClient,
    private readonly profile: Profile,
  ) {
    this.#win = WINDOWS[profile];
    this.#blobs = new BlobLru<BlobEntry>(BLOB_LRU[profile]);
    video.addEventListener('timeupdate', this.#wake);
    video.addEventListener('seeking', this.#wake);
    video.addEventListener('ended', this.#onEnded);
  }

  // ---- state ---------------------------------------------------------------

  /** The playhead for buffer-window decisions: the seek target while one is settling. */
  #head(): number {
    return this.#seekHead ?? this.video.currentTime;
  }

  #set(patch: Partial<PlayerState>): void {
    this.#state = { ...this.#state, ...patch };
    for (const cb of [...this.#listeners]) cb(this.#state);
  }

  onState(cb: (s: PlayerState) => void): () => void {
    this.#listeners.add(cb);
    cb(this.#state);
    return () => this.#listeners.delete(cb);
  }

  setAutoAdvance(on: boolean): void {
    this.#autoAdvance = on;
  }

  // ---- load ----------------------------------------------------------------

  async load(clipId: string): Promise<IntegrityState> {
    this.#teardown();
    const g = this.#gen;
    this.#clipId = clipId;
    this.#set({ ...initialState(), clipId });

    let open: ClipOpenResult;
    try {
      open = await this.client.call<ClipOpenResult>({ type: 'clip.open', clipId });
    } catch (e) {
      if (g !== this.#gen) return this.#state.integrity;
      return this.#openFailed(e, clipId);
    }
    if (g !== this.#gen) {
      if (clipId !== this.#clipId) this.#close(clipId);
      return this.#state.integrity;
    }
    this.#open = open;
    const v = open.verification;
    const mismatch = v.camera === 'mismatch' || v.start === 'mismatch' || v.clipId === 'mismatch';
    // A Blob cached from an earlier load already knows how that clip ended.
    const cached = this.#blobs.get(clipId)?.outcome ?? null;
    const integrity: IntegrityState = cached
      ? cached.integrity
      : mismatch
        ? 'mismatch'
        : open.status === 'incomplete'
          ? 'incomplete'
          : 'verified';
    this.#set({
      integrity,
      failedAtMs: cached?.failedAtMs ?? null,
      verification: v,
      header: { cameraId: open.header.cameraId, startUnixMs: open.header.startUnixMs },
    });
    void this.#start(open, g);
    return integrity;
  }

  async #openFailed(e: unknown, clipId: string): Promise<IntegrityState> {
    if (e instanceof ErrUnknownKey) {
      let keyId: string | null = null;
      try {
        const sc = await this.client.call<{ video?: { key_id?: unknown } }>({
          type: 'api.get',
          path: `/api/v1/clips/${clipId}`,
          query: {},
        });
        const hint = sc.video?.key_id;
        if (typeof hint === 'string' && /^[0-9a-f]{16}$/.test(hint)) keyId = groupKeyId(hint);
      } catch {
        /* the hint is best-effort */
      }
      if (clipId === this.#clipId) this.#set({ integrity: 'unknown-key', keyId });
      return 'unknown-key';
    }
    if (e instanceof ErrAuthHeader || e instanceof ErrFormat) {
      this.#set({ integrity: 'tampered' });
      return 'tampered';
    }
    if (e instanceof ErrTruncated) {
      this.#set({ integrity: 'incomplete' });
      return 'incomplete';
    }
    this.#set({ message: e instanceof Error ? e.message : 'Could not open this recording.' });
    throw e;
  }

  /** Picks the playback path (ui-spec §4.3) and starts it. */
  async #start(open: ClipOpenResult, g: number): Promise<void> {
    const mime = `video/mp4; codecs="${open.codec}"`;
    const kind: EngineKind | null = mseSupports('mms', mime)
      ? 'mms'
      : mseSupports('mse', mime)
        ? 'mse'
        : this.video.canPlayType(mime) !== ''
          ? 'blob'
          : null;
    if (kind === null) {
      this.#set({ message: `This browser can't play ${open.codec}` });
      return;
    }
    if (kind === 'blob') {
      const size = open.plaintextSizeBytes ?? open.numChunks * open.header.chunkSize;
      if (size > BLOB_MAX[this.profile]) {
        this.#set({ message: 'Too large to play on this browser' });
        return;
      }
    }
    this.#set({ engine: kind });
    try {
      if (kind === 'blob') await this.#runBlob(open, g);
      else await this.#runMse(kind, mime, open, g);
    } catch (e) {
      if (g === this.#gen) this.#fail(e, 0);
    }
  }

  // ---- MSE path ------------------------------------------------------------

  async #runMse(kind: MseKind, mime: string, open: ClipOpenResult, g: number): Promise<void> {
    if (!mseCtor(kind)) return;
    const sink = await openSink(this.video, kind, mime, { onStreaming: () => this.#signal() });
    if (g !== this.#gen) {
      sink.destroy();
      return;
    }
    this.#sink = sink;
    await this.#fill(g, open, sink, 0, 0);
  }

  #wantMore(sink: Sink): boolean {
    return sink.streaming && needsMore(sink.buffered(), this.#head(), this.#win);
  }

  #demand(): Promise<void> {
    return new Promise((resolve) => this.#waiters.push(resolve));
  }

  #signal(): void {
    const w = this.#waiters;
    this.#waiters = [];
    for (const r of w) r();
  }

  #read(chunk: number): Promise<ClipReadResult> {
    const p = this.client.call<ClipReadResult>({ type: 'clip.read', clipId: this.#clipId!, chunk });
    p.catch(() => undefined); // a prefetch nobody awaited must not be an unhandled rejection
    return p;
  }

  /**
   * Appends chunks from `from` (the first one from `intra` bytes in) for as long
   * as the buffer wants more, until the final chunk or a newer generation.
   */
  async #fill(g: number, open: ClipOpenResult, sink: Sink, from: number, intra: number, onFirst?: () => void): Promise<void> {
    const pending = new Map<number, Promise<ClipReadResult>>();
    let c = from;
    // A seek always fetches its first chunk: the playhead has not moved yet, so the buffer window
    // (and ManagedMediaSource's streaming flag) still describe the old position.
    let force = onFirst !== undefined;
    try {
      while (g === this.#gen && c < open.numChunks) {
        if (!force && !this.#wantMore(sink)) {
          await this.#demand();
          continue;
        }
        force = false;
        const cur = pending.get(c) ?? this.#read(c);
        pending.delete(c);
        if (c + 1 < open.numChunks && !pending.has(c + 1)) pending.set(c + 1, this.#read(c + 1));
        const res = await cur;
        if (g !== this.#gen) return;
        await this.#append(sink, new Uint8Array(res.plaintext, c === from ? intra : 0));
        if (g !== this.#gen) return;
        onFirst?.();
        onFirst = undefined;
        if (res.final) {
          await sink.endOfStream();
          return;
        }
        c++;
        await this.#evictBehind(sink, false);
      }
    } catch (e) {
      if (g !== this.#gen) return;
      this.#fail(e, c);
      // What was verified plays out, then the stream ends.
      await sink.endOfStream().catch(() => undefined);
    } finally {
      onFirst?.();
    }
  }

  async #append(sink: Sink, bytes: Uint8Array): Promise<void> {
    try {
      await sink.append(bytes);
    } catch (e) {
      if (!isQuota(e)) throw e;
      // ui-spec §4.3.2: evict immediately, then retry once.
      await this.#evictBehind(sink, true);
      await sink.append(bytes);
    }
  }

  /** Removes what is older than `currentTime − behind_seconds` (a tighter bound after a quota error). */
  async #evictBehind(sink: Sink, tight: boolean): Promise<void> {
    const win = tight ? { ...this.#win, behindSeconds: Math.min(this.#win.behindSeconds, 2) } : this.#win;
    for (const [s, e] of planEviction(sink.buffered(), this.#head(), win).behind) await sink.remove(s, e);
  }

  // ---- Blob path -----------------------------------------------------------

  async #runBlob(open: ClipOpenResult, g: number): Promise<void> {
    const clipId = this.#clipId!;
    const hit = this.#blobs.get(clipId);
    if (hit) {
      this.video.src = hit.url;
      return;
    }
    const parts: ArrayBuffer[] = [];
    let outcome: BlobEntry['outcome'] = null;
    let next: Promise<ClipReadResult> | null = this.#read(0);
    for (let c = 0; c < open.numChunks; c++) {
      const cur = next!;
      next = c + 1 < open.numChunks ? this.#read(c + 1) : null;
      try {
        parts.push((await cur).plaintext);
      } catch (e) {
        if (g !== this.#gen) return;
        this.#fail(e, c);
        outcome = this.#integrityFailure(e, c);
        if (outcome === null) return;
        break;
      }
      if (g !== this.#gen) return;
    }
    if (parts.length === 0) return;
    const url = URL.createObjectURL(blobFromParts(parts));
    this.#blobs.add(clipId, { url, outcome });
    this.video.src = url;
  }

  // ---- failures ------------------------------------------------------------

  /** Maps a chunk error to the sticky integrity outcome, or null for a non-integrity error. */
  #integrityFailure(e: unknown, chunk: number): BlobEntry['outcome'] {
    const open = this.#open;
    if (!open) return null;
    if (e instanceof ErrAuth || e instanceof ErrTrailingData || e instanceof ErrAuthHeader) {
      const at = e instanceof ErrAuth && e.chunk >= 0 ? e.chunk : chunk;
      return { integrity: 'tampered', failedAtMs: failureTimeMs(open.seekIndex, at, open.header.chunkSize) };
    }
    if (e instanceof ErrTruncated) return { integrity: 'incomplete', failedAtMs: null };
    return null;
  }

  #fail(e: unknown, chunk: number): void {
    const o = this.#integrityFailure(e, chunk);
    if (o === null) {
      this.#set({ message: e instanceof Error ? e.message : 'Playback failed.' });
      return;
    }
    // Tampered is sticky: a later truncation cannot soften it.
    if (this.#state.integrity === 'tampered') return;
    this.#set(o.integrity === 'tampered' ? { integrity: 'tampered', failedAtMs: o.failedAtMs } : { integrity: 'incomplete' });
  }

  // ---- seek ----------------------------------------------------------------

  async seek(tMs: number): Promise<void> {
    const open = this.#open;
    const sink = this.#sink;
    let target = Math.max(0, tMs);
    if (this.#state.integrity === 'tampered' && this.#state.failedAtMs !== null) {
      // Never play (or fetch) past the last verified frame.
      const limit = this.#state.failedAtMs;
      target = Math.min(target, limit);
      if (target >= limit) {
        this.video.currentTime = target / 1000;
        return;
      }
    }
    if (!open || !sink) {
      this.video.currentTime = target / 1000;
      return;
    }
    const plan = planSeek({ tMs: target, seekIndex: open.seekIndex, chunkSize: open.header.chunkSize, buffered: sink.buffered() });
    if (plan.kind !== 'fetch') {
      this.video.currentTime = target / 1000;
      this.#signal();
      return;
    }
    // ui-spec §4.3.2 step 2: drop the old fetches (their results are ignored), reset the parser, fetch the fragment.
    const g = ++this.#gen;
    this.#signal();
    target = Math.max(target, plan.fragmentMs);
    this.#seekHead = target / 1000;
    await sink.abort();
    if (g !== this.#gen) return;
    // Anything far ahead of the new position would otherwise leave a hole behind the sequential fill.
    for (const [s, e] of planEviction(sink.buffered(), target / 1000, this.#win).ahead) await sink.remove(s, e);
    if (g !== this.#gen) return;
    await new Promise<void>((resolve) => {
      void this.#fill(g, open, sink, plan.chunk, plan.intraOffset, resolve);
    });
    // Move the playhead only once its fragment is buffered. Setting it earlier makes
    // WebKit settle on the nearest buffered time instead of waiting for the data.
    if (g === this.#gen) {
      this.video.currentTime = target / 1000;
      this.#seekHead = null;
    }
  }

  // ---- auto-advance --------------------------------------------------------

  async #advance(): Promise<void> {
    const id = this.#clipId;
    if (!this.#autoAdvance || id === null || this.#state.integrity === 'tampered') return;
    const g = this.#gen;
    try {
      const next = await findNextClip(this.client, id);
      if (next === null || g !== this.#gen) return;
      await this.load(next);
      void Promise.resolve(this.video.play()).catch(() => undefined);
    } catch {
      /* no next clip is not an error the viewer needs to see */
    }
  }

  // ---- teardown ------------------------------------------------------------

  #close(clipId: string): void {
    const msg: Request = { type: 'clip.close', clipId };
    void this.client.call(msg).catch(() => undefined);
  }

  /** Stops everything belonging to the current clip. */
  #teardown(): void {
    this.#gen++;
    this.#seekHead = null;
    this.#signal();
    if (this.#clipId !== null) this.#close(this.#clipId);
    this.#sink?.destroy();
    this.#sink = null;
    this.#open = null;
    this.#clipId = null;
    this.video.removeAttribute('src');
  }

  destroy(): void {
    if (this.#destroyed) return;
    this.#destroyed = true;
    this.#teardown();
    this.#blobs.clear();
    this.video.removeEventListener('timeupdate', this.#wake);
    this.video.removeEventListener('seeking', this.#wake);
    this.video.removeEventListener('ended', this.#onEnded);
    this.#listeners.clear();
  }
}
