// Playback engine (ui-spec §4.3, §4.4). Chunks are authenticated and decrypted
// in the crypto worker; this module feeds the plaintext fragmented MP4 to a
// <video> through ManagedMediaSource, MediaSource or a Blob, keeping a bounded
// buffer window, and reports the integrity state of what is playing.
import type { ClipOpenResult, ClipReadResult, Request, Verification } from '../../worker/protocol';
import { ErrAuth, ErrAuthHeader, ErrFormat, ErrTrailingData, ErrTruncated, ErrUnknownKey } from '../zrec/errors';
import type { WorkerClient } from '../workerClient';
import { findNextClip } from './advance';
import { BLOB_LRU, BLOB_MAX, WINDOWS, alignToFragments, failureTimeMs, feedsRange, isBuffered, needsMore, planEviction, planResume, planSeek, type Profile } from './buffer';
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
  /** Bumped by every seek(): a settling seek whose token is no longer current must not move the playhead. */
  #seekTok = 0;
  /** True once the current clip has had its first fragment appended (before that, the fill is the initial fetch). */
  #appended = false;
  /**
   * The fragment (media ms) and generation of the latest fetch seek, while the playhead has not yet been
   * inside the buffer since. Its fill is already running: a seeking event for the same fragment (the
   * browser clamping the playhead to the end of the buffer until the fragment is complete) is not a new scrub.
   */
  #fetching: { fragmentMs: number; gen: number } | null = null;
  /**
   * Where the running sequential fill is: its generation and the plaintext offset of the next byte it appends.
   * Null when no fill is running (not started, ended, or failed).
   */
  #fillAt: { gen: number; offset: number } | null = null;
  #autoAdvance = false;
  #destroyed = false;
  readonly #win;
  readonly #blobs: BlobLru<BlobEntry>;
  readonly #onEnded = () => void this.#advance();
  readonly #onTime = () => {
    if (this.#fetching && this.#sink && isBuffered(this.#sink.buffered(), this.video.currentTime)) this.#fetching = null;
    this.#signal();
  };
  readonly #onSeeking = () => {
    this.#signal();
    this.#nativeSeek();
  };

  constructor(
    private readonly video: HTMLVideoElement,
    private readonly client: WorkerClient,
    private readonly profile: Profile,
  ) {
    this.#win = WINDOWS[profile];
    this.#blobs = new BlobLru<BlobEntry>(BLOB_LRU[profile]);
    video.addEventListener('timeupdate', this.#onTime);
    video.addEventListener('seeking', this.#onSeeking);
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
    const size = open.header.chunkSize;
    this.#fillAt = { gen: g, offset: from * size + intra };
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
        await this.#append(g, sink, new Uint8Array(res.plaintext, c === from ? intra : 0));
        if (g !== this.#gen) return;
        this.#appended = true;
        onFirst?.();
        onFirst = undefined;
        if (res.final) {
          await sink.endOfStream();
          return;
        }
        c++;
        this.#fillAt = { gen: g, offset: c * size };
        await this.#evictBehind(g, sink, false);
      }
    } catch (e) {
      if (g !== this.#gen) return;
      this.#fail(e, c);
      // What was verified plays out, then the stream ends.
      await sink.endOfStream().catch(() => undefined);
    } finally {
      if (this.#fillAt?.gen === g) this.#fillAt = null;
      onFirst?.();
    }
  }

  async #append(g: number, sink: Sink, bytes: Uint8Array): Promise<void> {
    try {
      await sink.append(bytes);
    } catch (e) {
      if (!isQuota(e)) throw e;
      // ui-spec §4.3.2: evict immediately, then retry once (unless a newer seek reset the parser meanwhile).
      await this.#evictBehind(g, sink, true);
      if (g !== this.#gen) return;
      await sink.append(bytes);
    }
  }

  /** Removes what is older than `currentTime − behind_seconds` (a tighter bound after a quota error). */
  async #evictBehind(g: number, sink: Sink, tight: boolean): Promise<void> {
    const win = tight ? { ...this.#win, behindSeconds: Math.min(this.#win.behindSeconds, 2) } : this.#win;
    for (const [s, e] of planEviction(sink.buffered(), this.#head(), win).behind) {
      // A newer seek has taken over: what this fill planned to trim may be what that seek is about to play.
      if (g !== this.#gen) return;
      await sink.remove(s, e);
    }
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

  /**
   * The viewer dragged the native seek bar (or the browser moved the playhead on its own). Goes through the
   * same path as seek(), minus assigning currentTime: the browser already did.
   */
  #nativeSeek(): void {
    void this.#move(this.video.currentTime * 1000, true);
  }

  async seek(tMs: number): Promise<void> {
    await this.#move(tMs, false);
  }

  /**
   * The one place the playhead moves: seek() and the browser's own `seeking` both end up here, and
   * `#seekTok` makes the latest call win. Every position it works with comes from planSeek over the
   * seek_index (Contract §6.4), the resume point after an in-buffer move included.
   *
   * `native`: the browser already assigned currentTime. A target that is not buffered must still go through
   * the planner, or the sequential fill would carry on from its old chunk. The engine's own assignment of
   * currentTime plans as buffered, so it does not re-enter.
   */
  async #move(tMs: number, native: boolean): Promise<void> {
    const open = this.#open;
    const sink = this.#sink;
    let target = Math.max(0, tMs);
    const st = this.#state;
    if (st.integrity === 'tampered' && st.failedAtMs !== null) {
      // Never play (or fetch) past the last verified frame. At the failure point itself the browser's own
      // assignment lands here: nothing to fetch, nothing to clamp.
      const limit = st.failedAtMs;
      if (target >= (native ? limit - 1 : limit)) {
        this.#seekTok++;
        if (!native || target > limit + 1) {
          this.#seekHead = null;
          this.video.currentTime = Math.min(target, limit) / 1000;
        }
        return;
      }
    }
    if (!open || !sink) {
      if (!native) {
        this.#seekTok++;
        this.#seekHead = null;
        this.video.currentTime = target / 1000;
      }
      return;
    }
    // Before the first fragment is appended the initial fetch is the one in progress.
    if (native && !this.#appended) return;
    const plan = planSeek({ tMs: target, seekIndex: open.seekIndex, chunkSize: open.header.chunkSize, buffered: sink.buffered() });
    if (plan.kind !== 'fetch') {
      ++this.#seekTok;
      const settling = plan.kind === 'buffered' && this.#seekHead !== null;
      this.#seekHead = null;
      if (plan.kind === 'buffered') this.#fetching = null;
      if (!native) this.video.currentTime = target / 1000;
      this.#signal();
      if (plan.kind === 'buffered') {
        // A move inside the buffer supersedes a fetch seek that is still settling (its token is stale now), and
        // must re-aim the fill when the running one is carrying some other range: that fill would never extend
        // the range the playhead is in.
        const resume = planResume({
          tMs: target,
          seekIndex: open.seekIndex,
          chunkSize: open.header.chunkSize,
          buffered: sink.buffered(),
          ended: sink.ended,
        });
        const at = this.#fillAt;
        const feeding = at !== null && at.gen === this.#gen && feedsRange(sink.buffered(), target, at.offset, open.seekIndex);
        if (!feeding && (settling || resume !== null)) this.#resumeBuffered(open, sink, target);
      }
      return;
    }
    // Once a fetch seek has handed the playhead over, the browser clamps it to the end of the buffer until
    // the seek's fragment is complete: that is not a new scrub. While the seek is still settling the engine
    // has not assigned currentTime yet, so any event is the viewer's, and the latest target wins.
    const f = this.#fetching;
    if (native && f && f.gen === this.#gen && f.fragmentMs === plan.fragmentMs && this.#seekHead === null) return;

    const tok = ++this.#seekTok;
    // ui-spec §4.3.2 step 2: drop the old fetches (their results are ignored), reset the parser, fetch the fragment.
    const g = ++this.#gen;
    this.#fetching = { fragmentMs: plan.fragmentMs, gen: g };
    this.#signal();
    target = Math.max(target, plan.fragmentMs);
    this.#seekHead = target / 1000;
    await sink.abort();
    if (g !== this.#gen) return;
    // Anything far ahead of the new position would otherwise leave a hole behind the sequential fill.
    // (A later seek inside the buffer may have taken over the playhead meanwhile: #head() follows it.
    // The fill below still runs, because this seek already stopped the previous one.)
    await this.#evictAhead(g, open, sink, this.#head());
    if (g !== this.#gen) return;
    await new Promise<void>((resolve) => {
      void this.#fill(g, open, sink, plan.chunk, plan.intraOffset, resolve);
    });
    // Move the playhead only once its fragment is buffered. Setting it earlier makes
    // WebKit settle on the nearest buffered time instead of waiting for the data.
    if (g === this.#gen && tok === this.#seekTok) {
      this.video.currentTime = target / 1000;
      this.#seekHead = null;
    }
  }

  /** Removes what is beyond `head + ahead_seconds`, starting on a fragment so that no fragment is left cut. */
  async #evictAhead(g: number, open: ClipOpenResult, sink: Sink, head: number): Promise<void> {
    const ahead = alignToFragments(planEviction(sink.buffered(), head, this.#win).ahead, open.seekIndex);
    for (const [s, e] of ahead) {
      if (g !== this.#gen) return;
      await sink.remove(s, e);
    }
  }

  /**
   * A move landed inside the buffer while an earlier fetch seek was still settling. Stop that seek's fill
   * (its token is already stale, so it never moves the playhead), drop what it left beyond the window, and
   * carry on sequentially from the fragment holding the end of the buffered range that holds the target.
   */
  #resumeBuffered(open: ClipOpenResult, sink: Sink, tMs: number): void {
    const g = ++this.#gen;
    this.#fetching = null;
    this.#fillAt = null;
    this.#signal();
    let from = 0;
    void (async () => {
      try {
        // The stopped fill may have been part-way through a fragment: reset the parser before appending again.
        await sink.abort();
        if (g !== this.#gen) return;
        await this.#evictAhead(g, open, sink, tMs / 1000);
        if (g !== this.#gen) return;
        // Plan from the buffer as the trim left it: it can end in the middle of a fragment.
        const resume = planResume({
          tMs,
          seekIndex: open.seekIndex,
          chunkSize: open.header.chunkSize,
          buffered: sink.buffered(),
          ended: sink.ended,
        });
        if (!resume) return;
        from = resume.chunk;
        await this.#fill(g, open, sink, resume.chunk, resume.intraOffset);
      } catch (e) {
        if (g === this.#gen) this.#fail(e, from);
      }
    })();
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
    this.#appended = false;
    this.#fetching = null;
    this.#fillAt = null;
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
    this.video.removeEventListener('timeupdate', this.#onTime);
    this.video.removeEventListener('seeking', this.#onSeeking);
    this.video.removeEventListener('ended', this.#onEnded);
    this.#listeners.clear();
  }
}
