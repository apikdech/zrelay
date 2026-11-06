import { describe, expect, it } from 'vitest';
import { sha256 } from './sha256';

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

describe('sha256 (synchronous, for the key checksum only)', () => {
  it.each([0, 1, 3, 55, 56, 63, 64, 65, 119, 120, 128, 1000])('matches Web Crypto for %d bytes', async (len) => {
    const msg = Uint8Array.from({ length: len }, (_, i) => (i * 31 + 7) & 0xff);
    const want = new Uint8Array(await crypto.subtle.digest('SHA-256', msg));
    expect(hex(sha256(msg))).toBe(hex(want));
  });

  it('hashes "abc" to the FIPS 180-4 value', () => {
    expect(hex(sha256(new TextEncoder().encode('abc')))).toBe(
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
    );
  });
});
