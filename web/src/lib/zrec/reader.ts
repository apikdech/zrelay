import { ErrAuth, ErrAuthHeader, ErrTrailingData, ErrTruncated, ErrUnknownKey } from './errors';
import type { Header } from './header';

/** Maps hex key_id to the wrap key derived from that master key (Contract §3.4). */
export type Keyring = Map<string, CryptoKey>;

/** An opened clip: the validated header plus its non-extractable DEK. */
export interface ClipKey {
  header: Header;
  /** AES-GCM, non-extractable, `decrypt` only: the DEK never enters JS. */
  dek: CryptoKey;
  /** SHA-256(D), the AAD of every chunk. */
  hd: Uint8Array;
}

const toHex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/**
 * Finishes Contract §4.4 header validation after parseHeader: keyring lookup
 * (ErrUnknownKey), then DEK unwrap (ErrAuthHeader). The DEK is unwrapped
 * straight into a non-extractable CryptoKey.
 */
export async function openClip(header: Header, ring: Keyring): Promise<ClipKey> {
  const wrapKey = ring.get(toHex(header.keyId));
  if (!wrapKey) throw new ErrUnknownKey('key_id not in keyring');

  const hd = new Uint8Array(await crypto.subtle.digest('SHA-256', header.descriptor as BufferSource));
  const aad = new Uint8Array(hd.length + header.keyId.length);
  aad.set(hd);
  aad.set(header.keyId, hd.length);
  let dek: CryptoKey;
  try {
    dek = await crypto.subtle.unwrapKey(
      'raw',
      header.wrappedDek as BufferSource,
      wrapKey,
      { name: 'AES-GCM', iv: header.wrapNonce as BufferSource, additionalData: aad as BufferSource },
      { name: 'AES-GCM' },
      false,
      ['decrypt'],
    );
  } catch {
    throw new ErrAuthHeader('DEK unwrap failed');
  }
  return { header, dek, hd };
}

/** N = ceil(body_len / (chunk_size + 16)) from the object's actual length. */
export function numChunks(headerLen: number, chunkSize: number, fileLen: number): number {
  const body = fileLen - headerLen;
  if (body <= 0) return 0;
  return Math.ceil(body / (chunkSize + 16));
}

/** Byte range [start, end) of chunk i within the stored object. */
export function chunkRange(i: number, headerLen: number, chunkSize: number, fileLen: number): [number, number] {
  const n = numChunks(headerLen, chunkSize, fileLen);
  if (!Number.isInteger(i) || i < 0 || i >= n) throw new RangeError(`chunk ${i} out of range [0, ${n})`);
  const s = chunkSize + 16;
  const start = headerLen + i * s;
  return [start, Math.min(start + s, fileLen)];
}

/** nonce_i = uint88_be(i) || final_flag. */
function nonce(i: number, final: boolean): Uint8Array {
  const out = new Uint8Array(12);
  new DataView(out.buffer).setBigUint64(3, BigInt(i));
  out[11] = final ? 1 : 0;
  return out;
}

async function open(clip: ClipKey, i: number, ct: Uint8Array, final: boolean): Promise<ArrayBuffer | null> {
  try {
    return await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: nonce(i, final) as BufferSource, additionalData: clip.hd as BufferSource },
      clip.dek,
      ct as BufferSource,
    );
  } catch {
    return null;
  }
}

/**
 * Authenticates and decrypts chunk i of n (Contract §4.4). Plaintext is
 * returned only after its tag verifies. `ciphertext` is exactly the bytes of
 * chunkRange(i, ...). Throws ErrTruncated, ErrTrailingData or ErrAuth(i);
 * with allowIncomplete a failure on the last chunk is ErrTruncated.
 */
export async function decryptChunk(
  clip: ClipKey,
  i: number,
  n: number,
  ciphertext: Uint8Array,
  opts: { allowIncomplete: boolean },
): Promise<{ plain: ArrayBuffer; final: boolean }> {
  if (!Number.isInteger(i) || i < 0 || i >= n) throw new RangeError(`chunk ${i} out of range [0, ${n})`);
  const last = i === n - 1;
  if (last && ciphertext.length < 17) {
    throw new ErrTruncated(`last chunk is ${ciphertext.length} bytes`);
  }

  // Try the expected flag first, then the other one to classify the failure.
  const plain = await open(clip, i, ciphertext, last);
  if (plain) return { plain, final: last };

  const other = await open(clip, i, ciphertext, !last);
  if (other) {
    // Never release it: the flag is wrong for this position.
    new Uint8Array(other).fill(0);
    if (last) throw new ErrTruncated(`final chunk missing after chunk ${i}`);
    throw new ErrTrailingData(`chunk ${i} is final but more data follows`);
  }
  if (last && opts.allowIncomplete) throw new ErrTruncated(`incomplete last chunk ${i}`);
  throw new ErrAuth(i);
}
