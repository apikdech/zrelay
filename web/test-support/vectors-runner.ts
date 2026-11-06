// Shared cross-language vector loop (Contract §4.5). Used by Vitest (Node
// Web Crypto) and by the Playwright test page (Chromium and WebKit), so both
// run exactly the same assertions.
import { ErrAuth } from '../src/lib/zrec/errors';
import { openClip, decryptChunk, numChunks, type Keyring } from '../src/lib/zrec/reader';
import { parseHeader } from '../src/lib/zrec/header';
import {
  deriveApiToken,
  deriveKeyId,
  deriveWrapKey,
  importMasterKey,
} from '../src/lib/zrec/keys';
import { ErrTruncated } from '../src/lib/zrec/errors';

export interface Manifest {
  format_version: number;
  suite: number;
  master_key_hex: string;
  chunk_size: number;
  camera_id: string;
  clip_id: string;
  start_unix_ms: number;
  derived: { key_id_hex: string; k_wrap_hex: string; k_api_hex: string; k_api_sha256_hex: string };
  positive: { name: string; file: string; plaintext_len: number; plaintext_sha256_hex: string }[];
  negative: { name: string; file: string; expect: string; chunk_index?: number }[];
}

export const toHex = (b: Uint8Array | ArrayBuffer): string =>
  Array.from(b instanceof Uint8Array ? b : new Uint8Array(b), (x) => x.toString(16).padStart(2, '0')).join('');

export const fromHex = (s: string): Uint8Array => Uint8Array.from(s.match(/../g) ?? [], (h) => parseInt(h, 16));

export function b64urlDecode(s: string): Uint8Array {
  const std = s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4);
  return Uint8Array.from(atob(std), (c) => c.charCodeAt(0));
}

export async function sha256Hex(b: Uint8Array | ArrayBuffer): Promise<string> {
  return toHex(await crypto.subtle.digest('SHA-256', b as BufferSource));
}

/** Builds the keyring a viewer would hold for the manifest's master key. */
export async function manifestKeyring(m: Manifest): Promise<Keyring> {
  const base = await importMasterKey(fromHex(m.master_key_hex));
  return new Map([[toHex(await deriveKeyId(base)), await deriveWrapKey(base)]]);
}

/** Derived key_id, K_wrap, K_api and SHA-256(K_api) against the manifest. */
export async function checkDerived(m: Manifest): Promise<string[]> {
  const fails: string[] = [];
  const base = await importMasterKey(fromHex(m.master_key_hex));
  if (toHex(await deriveKeyId(base)) !== m.derived.key_id_hex) fails.push('key_id mismatch');

  const raw = b64urlDecode(await deriveApiToken(base));
  if (toHex(raw) !== m.derived.k_api_hex) fails.push('k_api mismatch');
  if ((await sha256Hex(raw)) !== m.derived.k_api_sha256_hex) fails.push('k_api_sha256 mismatch');

  // K_wrap is non-extractable by design. Re-derive it through a raw HKDF
  // (test-only) to compare bytes; the positive vectors prove the real key.
  const hkdf = await crypto.subtle.importKey('raw', fromHex(m.master_key_hex) as BufferSource, 'HKDF', false, ['deriveBits']);
  const wrap = await crypto.subtle.deriveBits(
    { name: 'HKDF', hash: 'SHA-256', salt: new TextEncoder().encode('zrec/v1'), info: new TextEncoder().encode('wrap') },
    hkdf,
    256,
  );
  if (toHex(wrap) !== m.derived.k_wrap_hex) fails.push('k_wrap mismatch');
  return fails;
}

/** Reads every chunk in order, verify-then-release, and returns the plaintext. */
export async function readAll(bytes: Uint8Array, ring: Keyring, allowIncomplete = false): Promise<Uint8Array> {
  const header = parseHeader(bytes);
  const clip = await openClip(header, ring);
  const n = numChunks(header.headerLen, header.chunkSize, bytes.length);
  if (n === 0) throw new ErrTruncated('no chunks');
  const parts: Uint8Array[] = [];
  const s = header.chunkSize + 16;
  for (let i = 0; i < n; i++) {
    const start = header.headerLen + i * s;
    const ct = bytes.subarray(start, Math.min(start + s, bytes.length));
    const { plain } = await decryptChunk(clip, i, n, ct, { allowIncomplete });
    parts.push(new Uint8Array(plain));
  }
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

export interface CaseResult {
  name: string;
  ok: boolean;
  detail: string;
}

/** Runs the derived checks, every positive and every negative vector. */
export async function runAll(m: Manifest, load: (file: string) => Promise<Uint8Array>): Promise<CaseResult[]> {
  const out: CaseResult[] = [];
  const derived = await checkDerived(m);
  out.push({ name: 'derived', ok: derived.length === 0, detail: derived.join('; ') });
  const ring = await manifestKeyring(m);

  for (const c of m.positive) {
    try {
      const plain = await readAll(await load(c.file), ring);
      const sha = await sha256Hex(plain);
      const ok = plain.length === c.plaintext_len && sha === c.plaintext_sha256_hex;
      out.push({ name: c.name, ok, detail: ok ? '' : `len=${plain.length} sha=${sha}` });
    } catch (e) {
      out.push({ name: c.name, ok: false, detail: String(e) });
    }
  }

  for (const c of m.negative) {
    try {
      await readAll(await load(c.file), ring);
      out.push({ name: c.name, ok: false, detail: `expected ${c.expect}, got success` });
    } catch (e) {
      const err = e as Error;
      let ok = err.name === c.expect;
      if (ok && c.expect === 'ErrAuth') ok = err instanceof ErrAuth && err.chunk === c.chunk_index;
      out.push({ name: c.name, ok, detail: ok ? '' : `expected ${c.expect}, got ${err.name}: ${err.message}` });
    }
  }
  return out;
}
