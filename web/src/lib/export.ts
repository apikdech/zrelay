// Export a clip as plaintext MP4 to the device (ui-spec §4.8).
//
// Chromium streams chunk by chunk into a file chosen with showSaveFilePicker,
// so the full plaintext never sits in memory. Elsewhere the chunks are gathered
// into one Blob within the profile's blob_max (navigator.share on iOS, an
// anchor download otherwise). Nothing is exported for a clip that failed
// verification. Every chunk authenticates in the worker before it reaches here.
import type { ClipOpenResult, ClipReadResult, Request } from '../worker/protocol';
import type { IntegrityState } from './player/engine';
import { zoneOffsetMs } from './timeline/timezone';

/** Whether the recording may be exported: not tampered, not unplayable, not still verifying. */
export function canExport(integrity: IntegrityState): boolean {
  return integrity === 'verified' || integrity === 'mismatch' || integrity === 'incomplete';
}

const pad = (n: number, w = 2): string => String(n).padStart(w, '0');

/** `<camera_id>_<local YYYYMMDD-HHMMSS>.mp4`, with `-incomplete` before the extension for an interrupted clip. */
export function exportFilename(cameraId: string, startMs: number, zone: string, incomplete: boolean): string {
  const whole = Math.floor(startMs / 1000) * 1000;
  const d = new Date(whole + zoneOffsetMs(whole, zone));
  const stamp =
    `${pad(d.getUTCFullYear(), 4)}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}` +
    `-${pad(d.getUTCHours())}${pad(d.getUTCMinutes())}${pad(d.getUTCSeconds())}`;
  return `${cameraId}_${stamp}${incomplete ? '-incomplete' : ''}.mp4`;
}

export class ExportDisabledError extends Error {
  override name = 'ExportDisabled';
  constructor() {
    super('this recording cannot be exported');
  }
}

export class ExportTooLargeError extends Error {
  override name = 'ExportTooLarge';
  constructor() {
    super('this recording is too large to save with this browser');
  }
}

export interface SaveWritable {
  write(chunk: Uint8Array): Promise<void>;
  close(): Promise<void>;
  abort(): Promise<void>;
}
export interface SaveHandle {
  createWritable(): Promise<SaveWritable>;
}

/** The browser features export depends on, so tests can stand in for them. */
export interface ExportEnv {
  showSaveFilePicker?: (opts: { suggestedName: string; types: { description: string; accept: Record<string, string[]> }[] }) => Promise<SaveHandle>;
  share?: (data: { files: File[]; title?: string }) => Promise<void>;
  canShare?: (data: { files: File[] }) => boolean;
  isIOS: boolean;
  download(blob: Blob, filename: string): void;
}

export function browserExportEnv(): ExportEnv {
  const w = window as unknown as { showSaveFilePicker?: ExportEnv['showSaveFilePicker'] };
  const nav = navigator;
  const iOS = /iP(hone|ad|od)/.test(nav.userAgent) || (nav.platform === 'MacIntel' && nav.maxTouchPoints > 1);
  return {
    showSaveFilePicker: w.showSaveFilePicker?.bind(window),
    share: typeof nav.share === 'function' ? (d) => nav.share(d) : undefined,
    canShare: typeof nav.canShare === 'function' ? (d) => nav.canShare(d) : undefined,
    isIOS: iOS,
    download(blob, filename) {
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = filename;
      a.rel = 'noopener';
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    },
  };
}

export type ExportResult =
  | { kind: 'saved'; method: 'picker' | 'share' | 'download'; bytes: number }
  | { kind: 'cancelled' };

export interface ExportOptions {
  client: { call<T = unknown>(msg: Request): Promise<T> };
  clipId: string;
  filename: string;
  integrity: IntegrityState;
  /** Largest Blob to build, from the playback profile (`BLOB_MAX`). */
  blobMax: number;
  env: ExportEnv;
  /** Plaintext bytes written or gathered so far. */
  onProgress?: (bytes: number) => void;
}

const MIME = 'video/mp4';
const isAbort = (e: unknown): boolean => e instanceof DOMException && e.name === 'AbortError';

/**
 * Exports one clip. The save picker (a user-gesture API) is opened before any
 * await on the worker. Throws ExportDisabledError for a clip that failed
 * verification, ExportTooLargeError past blob_max, and whatever the worker
 * throws when a chunk fails to authenticate (a streamed file is aborted then,
 * so no partial file stays behind).
 */
export async function exportClip(o: ExportOptions): Promise<ExportResult> {
  if (!canExport(o.integrity)) throw new ExportDisabledError();
  const { client, clipId, env } = o;

  let out: SaveWritable | null = null;
  if (env.showSaveFilePicker) {
    try {
      const handle = await env.showSaveFilePicker({
        suggestedName: o.filename,
        types: [{ description: 'MP4 video', accept: { [MIME]: ['.mp4'] } }],
      });
      out = await handle.createWritable();
    } catch (e) {
      if (isAbort(e)) return { kind: 'cancelled' };
      throw e;
    }
  }

  try {
    const open = await client.call<ClipOpenResult>({ type: 'clip.open', clipId });
    if (!out && open.plaintextSizeBytes !== null && open.plaintextSizeBytes > o.blobMax) throw new ExportTooLargeError();

    const parts: ArrayBuffer[] = [];
    let total = 0;
    const read = (chunk: number) => client.call<ClipReadResult>({ type: 'clip.read', clipId, chunk });
    let pending = read(0);
    for (let i = 0; i < open.numChunks; i++) {
      const r = await pending;
      total += r.plaintext.byteLength;
      if (!out && total > o.blobMax) throw new ExportTooLargeError();
      // One chunk ahead, so the network and the write overlap.
      const more = !r.final && i + 1 < open.numChunks;
      if (more) {
        pending = read(i + 1);
        pending.catch(() => {});
      }
      if (out) await out.write(new Uint8Array(r.plaintext));
      else parts.push(r.plaintext);
      o.onProgress?.(total);
      if (!more) break;
    }

    if (out) {
      await out.close();
      return { kind: 'saved', method: 'picker', bytes: total };
    }
    const blob = new Blob(parts, { type: MIME });
    if (env.isIOS && env.share) {
      const file = new File([blob], o.filename, { type: MIME });
      if (env.canShare?.({ files: [file] })) {
        try {
          await env.share({ files: [file], title: o.filename });
        } catch (e) {
          if (isAbort(e)) return { kind: 'cancelled' };
          throw e;
        }
        return { kind: 'saved', method: 'share', bytes: total };
      }
    }
    env.download(blob, o.filename);
    return { kind: 'saved', method: 'download', bytes: total };
  } catch (e) {
    if (out) await out.abort().catch(() => {});
    throw e;
  }
}
