import { KeyChecksumError, KeyFormatError } from './errors';
import { sha256 } from './sha256';

const HKDF_SALT = new TextEncoder().encode('zrec/v1');
const PREFIX = 'zk1';
// zk1-<64 hex>-<4 hex>
const KEY_TEXT = /^zk1-([0-9a-f]{64})-([0-9a-f]{4})$/;

const bytes = (s: string): Uint8Array<ArrayBuffer> => new TextEncoder().encode(s);

function hexToBytes(h: string): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(h.slice(2 * i, 2 * i + 2), 16);
  return out;
}

const toHex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/** hex(SHA-256("zk1" || key)[0:2]); synchronous, see sha256.ts. */
function checksum(raw: Uint8Array): string {
  const pre = new Uint8Array(PREFIX.length + raw.length);
  pre.set(bytes(PREFIX));
  pre.set(raw, PREFIX.length);
  return toHex(sha256(pre).subarray(0, 2));
}

/**
 * Parses zk1-<64 hex>-<4 hex> (Contract §3.1): case-insensitive, whitespace
 * stripped. A bad checksum throws KeyChecksumError (a typo), anything else
 * malformed throws KeyFormatError. Errors never contain key material.
 */
export function parseMasterKey(s: string): Uint8Array {
  const t = s.replace(/\s+/g, '').toLowerCase();
  const m = KEY_TEXT.exec(t);
  if (!m) throw new KeyFormatError('malformed master key: want zk1-<64 hex>-<4 hex>');
  const raw = hexToBytes(m[1]!);
  if (checksum(raw) !== m[2]) {
    raw.fill(0);
    throw new KeyChecksumError();
  }
  return raw;
}

/** HKDF base key for the master key: non-extractable, derive-only. */
export function importMasterKey(raw: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', raw as BufferSource, 'HKDF', false, ['deriveBits', 'deriveKey']);
}

function hkdfParams(label: string): HkdfParams {
  return { name: 'HKDF', hash: 'SHA-256', salt: HKDF_SALT, info: bytes(label) };
}

/** Public 8-byte key identifier (info `key-id`). */
export async function deriveKeyId(base: CryptoKey): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.deriveBits(hkdfParams('key-id'), base, 64));
}

/** K_wrap (info `wrap`) as a non-extractable AES-GCM key usable only to unwrap DEKs. */
export function deriveWrapKey(base: CryptoKey): Promise<CryptoKey> {
  return crypto.subtle.deriveKey(hkdfParams('wrap'), base, { name: 'AES-GCM', length: 256 }, false, ['unwrapKey']);
}

/** Bearer credential: unpadded base64url of the raw 32-byte K_api (info `api-token`). */
export async function deriveApiToken(base: CryptoKey): Promise<string> {
  const raw = new Uint8Array(await crypto.subtle.deriveBits(hkdfParams('api-token'), base, 256));
  let bin = '';
  for (const b of raw) bin += String.fromCharCode(b);
  raw.fill(0);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** Renders a key_id as a fingerprint, e.g. a1b2-c3d4-e5f6-0718. */
export function formatKeyId(id: Uint8Array): string {
  return (toHex(id).match(/.{4}/g) ?? []).join('-');
}
