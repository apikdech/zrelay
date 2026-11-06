import { ErrFormat, ErrTruncated } from './errors';

// Format constants (Contract §4.1).
const DESCRIPTOR_FIXED_LEN = 37;
const KEY_BLOCK_LEN = 68;
const MIN_CHUNK_SIZE = 1 << 16;
const MAX_CHUNK_SIZE = 1 << 24;

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

/** A parsed zrec header: descriptor D followed by key block K. */
export interface Header {
  formatVersion: number;
  suite: number;
  headerLen: number;
  chunkSize: number;
  /** ULID text of the clip. */
  clipId: string;
  startUnixMs: number;
  cameraId: string;
  keyId: Uint8Array;
  wrapNonce: Uint8Array;
  wrappedDek: Uint8Array;
  /** The serialized descriptor D; SHA-256(D) is the AAD of every chunk. */
  descriptor: Uint8Array;
}

function encodeUlid(b: Uint8Array): string {
  let v = 0n;
  for (const x of b) v = (v << 8n) | BigInt(x);
  let s = '';
  for (let i = 0; i < 26; i++) {
    s = CROCKFORD[Number(v & 31n)]! + s;
    v >>= 5n;
  }
  return s;
}

function validCameraId(s: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,63}$/.test(s);
}

/**
 * Parses and format-checks the header at the start of `b` in the Contract
 * §4.4 order. Bytes after header_len are ignored. It does not consult a
 * keyring or unwrap the DEK. Input shorter than the header is ErrTruncated;
 * anything malformed is ErrFormat.
 */
export function parseHeader(b: Uint8Array): Header {
  if (b.length < DESCRIPTOR_FIXED_LEN) {
    throw new ErrTruncated(`header needs at least ${DESCRIPTOR_FIXED_LEN} bytes, got ${b.length}`);
  }
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  if (String.fromCharCode(b[0]!, b[1]!, b[2]!, b[3]!) !== 'ZREC') throw new ErrFormat('bad magic');
  const formatVersion = b[4]!;
  const suite = b[5]!;
  if (formatVersion !== 1 || suite !== 1) {
    throw new ErrFormat(`unsupported format_version ${formatVersion} / suite ${suite}`);
  }
  const headerLen = dv.getUint16(6);
  const n = b[36]!;
  if (headerLen !== DESCRIPTOR_FIXED_LEN + n + KEY_BLOCK_LEN) {
    throw new ErrFormat(`header_len ${headerLen} inconsistent with camera_id_len ${n}`);
  }
  if (b.length < headerLen) throw new ErrTruncated(`header needs ${headerLen} bytes, got ${b.length}`);

  const chunkSize = dv.getUint32(8);
  if (chunkSize < MIN_CHUNK_SIZE || chunkSize > MAX_CHUNK_SIZE || (chunkSize & (chunkSize - 1)) !== 0) {
    throw new ErrFormat(`chunk_size ${chunkSize}`);
  }
  const startBig = dv.getBigInt64(28);
  const cameraId = new TextDecoder('utf-8', { fatal: false }).decode(b.subarray(37, 37 + n));
  if (!validCameraId(cameraId)) throw new ErrFormat('invalid camera_id');

  const clipBytes = b.slice(12, 28);
  let ts = 0n;
  for (let i = 0; i < 6; i++) ts = (ts << 8n) | BigInt(clipBytes[i]!);
  if (startBig < 0n || startBig > BigInt(Number.MAX_SAFE_INTEGER) || ts !== startBig) {
    throw new ErrFormat('clip_id timestamp does not equal start_unix_ms');
  }

  const k = DESCRIPTOR_FIXED_LEN + n;
  return {
    formatVersion,
    suite,
    headerLen,
    chunkSize,
    clipId: encodeUlid(clipBytes),
    startUnixMs: Number(startBig),
    cameraId,
    keyId: b.slice(k, k + 8),
    wrapNonce: b.slice(k + 8, k + 20),
    wrappedDek: b.slice(k + 20, k + 68),
    descriptor: b.slice(0, k),
  };
}
