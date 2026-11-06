// Message protocol between the main thread and the crypto/data worker
// (ui-spec §4.2, plus the live.* messages of the design doc §7.2).
// Nothing in a reply may carry key material: no master key, K_wrap, K_api, DEK.

import type { Sidecar } from './schemas';

export type QueryValue = string | readonly string[];

export type Request =
  | { type: 'key.import'; encoded: string; remember: boolean }
  | { type: 'key.list' }
  | { type: 'key.forget'; keyId: string }
  | { type: 'key.lock' }
  | { type: 'api.get'; path: string; query: Readonly<Record<string, QueryValue>> }
  | { type: 'clip.open'; clipId: string }
  | { type: 'clip.read'; clipId: string; chunk: number }
  | { type: 'clip.close'; clipId: string }
  | { type: 'live.offer'; cameraId: string; sdp: string }
  | { type: 'live.close'; sessionPath: string };

/** What the worker returns for a request: the result plus buffers to transfer. */
export interface Handled {
  result: unknown;
  transfer?: Transferable[];
}

/** G3: header descriptor against the (unauthenticated) sidecar. */
export type Match = 'match' | 'mismatch';
export interface Verification {
  camera: Match;
  start: Match;
  clipId: Match;
}

/** Public header fields only (hex strings, numbers): no byte arrays cross to the main thread. */
export interface HeaderInfo {
  formatVersion: number;
  suite: number;
  headerLen: number;
  chunkSize: number;
  clipId: string;
  startUnixMs: number;
  cameraId: string;
  /** hex(8) */
  keyId: string;
}

export interface ClipOpenResult {
  header: HeaderInfo;
  verification: Verification;
  numChunks: number;
  codec: string;
  initSegmentBytes: number;
  seekIndex: [number, number][];
  status: Sidecar['status'];
  plaintextSizeBytes: number | null;
}

export interface ClipReadResult {
  plaintext: ArrayBuffer;
  final: boolean;
}

export type ImportResult = { keyId: string } | { error: 'checksum' | 'format' };
export interface KeyListResult {
  keys: { keyId: string; remembered: boolean }[];
}
export interface LiveOfferResult {
  sdp: string;
  sessionPath: string;
}

/** Envelope on the wire. `id` correlates a reply with its call. */
export interface Envelope {
  id: number;
  msg: Request;
}

export interface WireError {
  name: string;
  message: string;
  /** ErrAuth: the failing chunk. */
  chunk?: number;
  /** ApiError / LiveError: HTTP status. */
  status?: number;
}

export type Reply = { id: number; ok: true; result: unknown } | { id: number; ok: false; error: WireError };
