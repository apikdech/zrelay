import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { ErrAuth, ErrAuthHeader, ErrTruncated, ErrUnknownKey } from './errors';
import { parseHeader } from './header';
import { chunkRange, decryptChunk, numChunks, openClip } from './reader';
import { manifestKeyring, type Manifest } from '../../../test-support/vectors-runner';

const dir = new URL('../../../../testdata/zrec/v1/', import.meta.url);
const manifest: Manifest = JSON.parse(readFileSync(new URL('vectors.json', dir), 'utf8'));
const file = (n: string) => new Uint8Array(readFileSync(new URL(n, dir)));
const CS = 65536;
const S = CS + 16;

describe('numChunks and chunkRange (Contract §4.4)', () => {
  it('computes N = ceil(body_len / S)', () => {
    expect(numChunks(115, CS, 115)).toBe(0);
    expect(numChunks(115, CS, 115 + 17)).toBe(1);
    expect(numChunks(115, CS, 115 + S)).toBe(1);
    expect(numChunks(115, CS, 115 + S + 1)).toBe(2);
    expect(numChunks(115, CS, 115 + 3 * S)).toBe(3);
  });

  it('is 0 when the file is shorter than the header', () => {
    expect(numChunks(115, CS, 100)).toBe(0);
  });

  it('spans [header_len + i*S, min(header_len + (i+1)*S, file_len))', () => {
    const fileLen = 115 + 2 * S + 100;
    expect(chunkRange(0, 115, CS, fileLen)).toEqual([115, 115 + S]);
    expect(chunkRange(1, 115, CS, fileLen)).toEqual([115 + S, 115 + 2 * S]);
    expect(chunkRange(2, 115, CS, fileLen)).toEqual([115 + 2 * S, fileLen]);
  });

  it('rejects a chunk index outside [0, N)', () => {
    expect(() => chunkRange(3, 115, CS, 115 + 2 * S + 100)).toThrow(RangeError);
    expect(() => chunkRange(-1, 115, CS, 115 + S)).toThrow(RangeError);
  });

  it('agrees with the 2.5-chunk vector', () => {
    const b = file('pos-two-and-half-chunks.zrec');
    const h = parseHeader(b);
    expect(numChunks(h.headerLen, h.chunkSize, b.length)).toBe(3);
  });
});

describe('openClip', () => {
  it('unwraps with the keyring entry for the header key_id', async () => {
    const h = parseHeader(file('pos-one-byte.zrec'));
    const clip = await openClip(h, await manifestKeyring(manifest));
    expect(clip.header).toBe(h);
  });

  it('reports a key_id missing from the keyring as ErrUnknownKey', async () => {
    const h = parseHeader(file('pos-one-byte.zrec'));
    await expect(openClip(h, new Map())).rejects.toBeInstanceOf(ErrUnknownKey);
  });

  it('checks the keyring before unwrapping (corrupt key_id is not ErrAuthHeader)', async () => {
    const h = parseHeader(file('neg-key-id-bitflip.zrec'));
    await expect(openClip(h, await manifestKeyring(manifest))).rejects.toBeInstanceOf(ErrUnknownKey);
  });

  it('reports a failed unwrap as ErrAuthHeader', async () => {
    const h = parseHeader(file('neg-wrapped-dek-bitflip.zrec'));
    await expect(openClip(h, await manifestKeyring(manifest))).rejects.toBeInstanceOf(ErrAuthHeader);
  });

  it('never exposes the DEK to JS: the clip key is non-extractable', async () => {
    const h = parseHeader(file('pos-one-byte.zrec'));
    const clip = await openClip(h, await manifestKeyring(manifest));
    expect(clip.dek.extractable).toBe(false);
    expect(clip.dek.usages).toEqual(['decrypt']);
    await expect(crypto.subtle.exportKey('raw', clip.dek)).rejects.toThrow();
  });
});

describe('decryptChunk', () => {
  async function open(name: string) {
    const b = file(name);
    const h = parseHeader(b);
    const clip = await openClip(h, await manifestKeyring(manifest));
    const n = numChunks(h.headerLen, h.chunkSize, b.length);
    const ct = (i: number) => {
      const [a, z] = chunkRange(i, h.headerLen, h.chunkSize, b.length);
      return b.subarray(a, z);
    };
    return { b, h, clip, n, ct };
  }

  it('returns plaintext and final=true for the last chunk, false otherwise', async () => {
    const { clip, n, ct } = await open('pos-two-and-half-chunks.zrec');
    const c0 = await decryptChunk(clip, 0, n, ct(0), { allowIncomplete: false });
    expect(c0.final).toBe(false);
    expect(c0.plain.byteLength).toBe(CS);
    const c2 = await decryptChunk(clip, 2, n, ct(2), { allowIncomplete: false });
    expect(c2.final).toBe(true);
    expect(c2.plain.byteLength).toBe(163840 - 2 * CS);
  });

  it('supports random access in any order', async () => {
    const { clip, n, ct } = await open('pos-two-and-half-chunks.zrec');
    await decryptChunk(clip, 2, n, ct(2), { allowIncomplete: false });
    await decryptChunk(clip, 1, n, ct(1), { allowIncomplete: false });
  });

  it('classifies a bit flip as ErrAuth carrying the chunk index', async () => {
    const { clip, n, ct } = await open('neg-bitflip-chunk.zrec');
    const err = await decryptChunk(clip, 1, n, ct(1), { allowIncomplete: false }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ErrAuth);
    expect((err as ErrAuth).chunk).toBe(1);
  });

  it('reports a last chunk shorter than 17 bytes as ErrTruncated', async () => {
    const { clip, ct } = await open('pos-one-byte.zrec');
    await expect(decryptChunk(clip, 0, 1, ct(0).subarray(0, 16), { allowIncomplete: false })).rejects.toBeInstanceOf(
      ErrTruncated,
    );
  });

  it('reports a damaged last chunk as ErrAuth, or ErrTruncated with allowIncomplete', async () => {
    const { clip, n, ct } = await open('pos-two-and-half-chunks.zrec');
    const damaged = Uint8Array.from(ct(2));
    damaged[0] = damaged[0]! ^ 1;
    const strict = await decryptChunk(clip, 2, n, damaged, { allowIncomplete: false }).catch((e: unknown) => e);
    expect(strict).toBeInstanceOf(ErrAuth);
    expect((strict as ErrAuth).chunk).toBe(2);
    await expect(decryptChunk(clip, 2, n, damaged, { allowIncomplete: true })).rejects.toBeInstanceOf(ErrTruncated);
  });

  it('allowIncomplete does not relax failures on non-last chunks', async () => {
    const { clip, n, ct } = await open('neg-bitflip-chunk.zrec');
    await expect(decryptChunk(clip, 1, n, ct(1), { allowIncomplete: true })).rejects.toBeInstanceOf(ErrAuth);
  });

  it('rejects an index outside [0, n)', async () => {
    const { clip, n, ct } = await open('pos-one-byte.zrec');
    await expect(decryptChunk(clip, 1, n, ct(0), { allowIncomplete: false })).rejects.toBeInstanceOf(RangeError);
  });
});
