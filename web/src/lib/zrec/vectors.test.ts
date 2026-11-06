import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { runAll, readAll, manifestKeyring, checkDerived, sha256Hex, type Manifest } from '../../../test-support/vectors-runner';
import { ErrAuth } from './errors';

const dir = new URL('../../../../testdata/zrec/v1/', import.meta.url);
const manifest: Manifest = JSON.parse(readFileSync(new URL('vectors.json', dir), 'utf8'));
const load = async (file: string) => new Uint8Array(readFileSync(new URL(file, dir)));

describe('cross-language vectors (testdata/zrec/v1)', () => {
  it('derives key_id, K_wrap, K_api and SHA-256(K_api) as the Go side does', async () => {
    expect(await checkDerived(manifest)).toEqual([]);
  });

  it.each(manifest.positive)('decrypts positive vector $name', async (c) => {
    const plain = await readAll(await load(c.file), await manifestKeyring(manifest));
    expect(plain.length).toBe(c.plaintext_len);
    expect(await sha256Hex(plain)).toBe(c.plaintext_sha256_hex);
  });

  it.each(manifest.negative)('rejects negative vector $name with $expect', async (c) => {
    const ring = await manifestKeyring(manifest);
    const err = await readAll(await load(c.file), ring).then(
      () => null,
      (e: Error) => e,
    );
    expect(err, 'must fail').not.toBeNull();
    expect(err!.name).toBe(c.expect);
    if (c.expect === 'ErrAuth') {
      expect(err).toBeInstanceOf(ErrAuth);
      expect((err as ErrAuth).chunk).toBe(c.chunk_index);
    }
  });

  it('covers every case in the manifest through the shared runner', async () => {
    const res = await runAll(manifest, load);
    expect(res).toHaveLength(1 + manifest.positive.length + manifest.negative.length);
    expect(res.filter((r) => !r.ok)).toEqual([]);
  });
});
