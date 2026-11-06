// Clip sessions: header fetch and validation, G3 comparison and chunk reads
// (ui-spec §4.2, Contract §4.4). Ciphertext is fetched by Range, at most two
// chunk fetches in flight per clip, and everything is cancelled on close.
import { fetchSidecar, readBounded, type AuthFetch } from './api';
import type { ClipOpenResult, ClipReadResult, HeaderInfo, Verification } from './protocol';
import { SchemaError, isUlid } from './schemas';
import { ErrTruncated } from '../lib/zrec/errors';
import { parseHeader, type Header } from '../lib/zrec/header';
import { chunkRange, decryptChunk, numChunks, openClip, type ClipKey, type Keyring } from '../lib/zrec/reader';

/** Contract §4.1: the largest header is 105 + 64 bytes, so bytes=0-168 always covers it. */
const HEADER_RANGE = 'bytes=0-168';
const HEADER_RANGE_LEN = 169;
const MAX_INFLIGHT = 2;

/** A read or close for a clip that is not open. */
export class NotOpenError extends Error {
  override name = 'ErrNotOpen';
}

/** The server's range response is not the one we asked for. */
export class RangeMismatchError extends Error {
  override name = 'ErrRange';
}

const abortError = (): DOMException => new DOMException('aborted', 'AbortError');

/** Counting semaphore whose waiters give up when the clip is closed. */
class Semaphore {
  #free: number;
  #waiters: (() => void)[] = [];
  constructor(max: number) {
    this.#free = max;
  }

  async acquire(signal: AbortSignal): Promise<() => void> {
    if (signal.aborted) throw abortError();
    if (this.#free > 0) {
      this.#free--;
      return () => this.#release();
    }
    await new Promise<void>((resolve, reject) => {
      const grant = () => {
        signal.removeEventListener('abort', onAbort);
        resolve();
      };
      const onAbort = () => {
        const i = this.#waiters.indexOf(grant);
        if (i >= 0) this.#waiters.splice(i, 1);
        reject(abortError());
      };
      this.#waiters.push(grant);
      signal.addEventListener('abort', onAbort, { once: true });
    });
    return () => this.#release();
  }

  #release(): void {
    const next = this.#waiters.shift();
    if (next) next();
    else this.#free++;
  }
}

interface OpenState {
  key: ClipKey;
  fileLen: number;
  n: number;
  allowIncomplete: boolean;
  ctl: AbortController;
  sem: Semaphore;
}

const toHex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

function headerInfo(h: Header): HeaderInfo {
  return {
    formatVersion: h.formatVersion,
    suite: h.suite,
    headerLen: h.headerLen,
    chunkSize: h.chunkSize,
    clipId: h.clipId,
    startUnixMs: h.startUnixMs,
    cameraId: h.cameraId,
    keyId: toHex(h.keyId),
  };
}

/** Parses `Content-Range: bytes a-b/total`; the total must be a number. */
function parseContentRange(res: Response): { start: number; end: number; total: number } {
  const m = /^bytes (\d+)-(\d+)\/(\d+)$/.exec(res.headers.get('Content-Range') ?? '');
  if (!m) throw new RangeMismatchError('missing or unusable Content-Range');
  const start = Number(m[1]);
  const end = Number(m[2]);
  const total = Number(m[3]);
  if (![start, end, total].every(Number.isSafeInteger) || start > end || end >= total) {
    throw new RangeMismatchError('inconsistent Content-Range');
  }
  return { start, end, total };
}

/** Reads a range body, refusing more than the `max` bytes that were asked for. */
function rangeBody(res: Response, max: number): Promise<Uint8Array> {
  return readBounded(res, max, () => new RangeMismatchError('range body longer than requested'));
}

export class Clips {
  #open = new Map<string, OpenState>();
  #opening = new Map<string, AbortController>();

  constructor(
    private readonly authFetch: AuthFetch,
    private readonly keyring: () => Promise<Keyring>,
  ) {}

  async open(clipId: string): Promise<ClipOpenResult> {
    if (!isUlid(clipId)) throw new SchemaError('invalid clip id');
    this.close(clipId);
    const ctl = new AbortController();
    this.#opening.set(clipId, ctl);
    try {
      const sidecar = await fetchSidecar(this.authFetch, clipId, ctl.signal);

      // Always the same path: the sidecar's video_href is never followed.
      const res = await this.authFetch(`/api/v1/clips/${clipId}/video`, {
        method: 'GET',
        headers: { Range: HEADER_RANGE },
        signal: ctl.signal,
      });
      if (res.status === 416) throw new ErrTruncated('clip object is empty');
      if (res.status !== 206) {
        void res.body?.cancel();
        throw new RangeMismatchError(`header fetch returned ${res.status}, want 206`);
      }
      const cr = parseContentRange(res);
      if (cr.start !== 0) throw new RangeMismatchError('header range does not start at 0');
      const head = await rangeBody(res, HEADER_RANGE_LEN);
      if (head.length !== cr.end + 1) throw new RangeMismatchError('header body length mismatch');

      const header = parseHeader(head);
      const key = await openClip(header, await this.keyring());
      // The true object length comes from Content-Range, never from sidecar.video.size_bytes.
      const n = numChunks(header.headerLen, header.chunkSize, cr.total);
      if (n === 0) throw new ErrTruncated('clip has a header but no chunks');

      if (ctl.signal.aborted) throw abortError();
      this.#open.set(clipId, {
        key,
        fileLen: cr.total,
        n,
        allowIncomplete: sidecar.status === 'incomplete',
        ctl,
        sem: new Semaphore(MAX_INFLIGHT),
      });

      const verification: Verification = {
        camera: header.cameraId === sidecar.camera.id ? 'match' : 'mismatch',
        start: header.startUnixMs === Date.parse(sidecar.start_time) ? 'match' : 'mismatch',
        clipId: header.clipId === sidecar.clip_id ? 'match' : 'mismatch',
      };
      return {
        header: headerInfo(header),
        verification,
        numChunks: n,
        codec: sidecar.video.codec,
        initSegmentBytes: sidecar.video.init_segment_bytes,
        seekIndex: sidecar.video.seek_index.map(([t, o]) => [t, o]),
        status: sidecar.status,
        plaintextSizeBytes: sidecar.video.plaintext_size_bytes,
      };
    } finally {
      if (this.#opening.get(clipId) === ctl) this.#opening.delete(clipId);
    }
  }

  async read(clipId: string, chunk: number): Promise<ClipReadResult> {
    const st = this.#open.get(clipId);
    if (!st) throw new NotOpenError('clip is not open');
    const { header } = st.key;
    const [start, end] = chunkRange(chunk, header.headerLen, header.chunkSize, st.fileLen);

    const release = await st.sem.acquire(st.ctl.signal);
    let ct: Uint8Array;
    try {
      const res = await this.authFetch(`/api/v1/clips/${clipId}/video`, {
        method: 'GET',
        headers: { Range: `bytes=${start}-${end - 1}` },
        signal: st.ctl.signal,
      });
      if (res.status !== 206) {
        void res.body?.cancel();
        throw new RangeMismatchError(`chunk fetch returned ${res.status}, want 206`);
      }
      const cr = parseContentRange(res);
      if (cr.start !== start || cr.end !== end - 1 || cr.total !== st.fileLen) {
        void res.body?.cancel();
        throw new RangeMismatchError('chunk range or object length changed');
      }
      ct = await rangeBody(res, end - start);
      if (ct.length !== end - start) throw new RangeMismatchError('chunk body length mismatch');
    } finally {
      release();
    }
    const { plain, final } = await decryptChunk(st.key, chunk, st.n, ct, { allowIncomplete: st.allowIncomplete });
    return { plaintext: plain, final };
  }

  /** Aborts in-flight fetches and drops the DEK. Unknown ids are ignored. */
  close(clipId: string): void {
    this.#opening.get(clipId)?.abort();
    this.#open.get(clipId)?.ctl.abort();
    this.#open.delete(clipId);
  }

  closeAll(): void {
    for (const id of [...this.#opening.keys(), ...this.#open.keys()]) this.close(id);
  }
}
