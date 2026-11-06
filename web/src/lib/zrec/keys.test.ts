import { describe, expect, it } from 'vitest';
import {
  deriveApiToken,
  deriveKeyId,
  deriveWrapKey,
  formatKeyId,
  importMasterKey,
  parseMasterKey,
} from './keys';
import { KeyChecksumError, KeyFormatError } from './errors';

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
const fromHex = (s: string) => Uint8Array.from(s.match(/../g)!, (h) => parseInt(h, 16));

// Master key 00..1f, as in the cross-language vectors (Contract §4.5).
const RAW_HEX = Array.from({ length: 32 }, (_, i) => i.toString(16).padStart(2, '0')).join('');

async function canonical(rawHex: string): Promise<string> {
  const raw = fromHex(rawHex);
  const pre = new Uint8Array([...new TextEncoder().encode('zk1'), ...raw]);
  const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', pre));
  return `zk1-${rawHex}-${hex(sum.slice(0, 2))}`;
}

describe('parseMasterKey', () => {
  it('parses the canonical form to the raw 32 bytes', async () => {
    expect(hex(parseMasterKey(await canonical(RAW_HEX)))).toBe(RAW_HEX);
  });

  it('is case-insensitive (QR codes carry the uppercase form)', async () => {
    const up = (await canonical(RAW_HEX)).toUpperCase();
    expect(hex(parseMasterKey(up))).toBe(RAW_HEX);
  });

  it('strips all whitespace', async () => {
    const c = await canonical(RAW_HEX);
    const spaced = ` \n${c.slice(0, 10)} \t${c.slice(10, 40)}\r\n${c.slice(40)} \n`;
    expect(hex(parseMasterKey(spaced))).toBe(RAW_HEX);
  });

  it('reports a checksum typo as KeyChecksumError, not a format error', async () => {
    const c = await canonical(RAW_HEX);
    const badChecksum = c.slice(0, -1) + (c.endsWith('0') ? '1' : '0');
    expect(() => parseMasterKey(badChecksum)).toThrow(KeyChecksumError);

    // A typo inside the key body is also a checksum failure.
    const badBody = c.slice(0, 4) + (c[4] === 'f' ? 'e' : 'f') + c.slice(5);
    expect(() => parseMasterKey(badBody)).toThrow(KeyChecksumError);
  });

  it.each([
    ['empty', ''],
    ['too short', 'zk1-00ff-abcd'],
    ['wrong prefix', 'zk2-' + RAW_HEX + '-0000'],
    ['missing dash before checksum', 'zk1-' + RAW_HEX + '0000x'],
    ['non-hex body', 'zk1-' + 'zz'.repeat(32) + '-0000'],
    ['non-hex checksum', 'zk1-' + RAW_HEX + '-zzzz'],
  ])('reports %s as KeyFormatError', (_name, s) => {
    expect(() => parseMasterKey(s)).toThrow(KeyFormatError);
  });

  it('never puts key material in an error message', async () => {
    const c = await canonical(RAW_HEX);
    const bad = c.slice(0, -1) + (c.endsWith('0') ? '1' : '0');
    try {
      parseMasterKey(bad);
      expect.unreachable();
    } catch (e) {
      expect((e as Error).message).not.toContain(RAW_HEX.slice(0, 16));
    }
  });
});

describe('derivations', () => {
  const raw = fromHex(RAW_HEX);

  it('importMasterKey yields a non-extractable HKDF base key', async () => {
    const base = await importMasterKey(raw);
    expect(base.algorithm.name).toBe('HKDF');
    expect(base.extractable).toBe(false);
    await expect(crypto.subtle.exportKey('raw', base)).rejects.toThrow();
  });

  it('deriveKeyId matches the Go-generated vector', async () => {
    const id = await deriveKeyId(await importMasterKey(raw));
    expect(hex(id)).toBe('24f301fa8d9378d6');
  });

  it('formatKeyId renders the fingerprint', () => {
    expect(formatKeyId(fromHex('24f301fa8d9378d6'))).toBe('24f3-01fa-8d93-78d6');
  });

  it('deriveApiToken is the unpadded base64url of K_api', async () => {
    const tok = await deriveApiToken(await importMasterKey(raw));
    expect(tok).toMatch(/^[A-Za-z0-9_-]{43}$/);
    const pad = tok.replace(/-/g, '+').replace(/_/g, '/') + '=';
    expect(hex(Uint8Array.from(atob(pad), (c) => c.charCodeAt(0)))).toBe(
      '849cc3a8c4c6e5e13f9e99524fff4ace0ac5cfb9c02c7c1c9c519df39e846108',
    );
  });

  it('deriveWrapKey is a non-extractable AES-GCM key limited to unwrapKey', async () => {
    const k = await deriveWrapKey(await importMasterKey(raw));
    expect(k.algorithm.name).toBe('AES-GCM');
    expect(k.extractable).toBe(false);
    expect(k.usages).toEqual(['unwrapKey']);
  });
});
