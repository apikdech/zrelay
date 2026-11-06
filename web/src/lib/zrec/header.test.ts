import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { ErrFormat, ErrTruncated } from './errors';
import { parseHeader } from './header';

const dir = new URL('../../../../testdata/zrec/v1/', import.meta.url);
const manifest = JSON.parse(readFileSync(new URL('vectors.json', dir), 'utf8'));
const clip = () => new Uint8Array(readFileSync(new URL('pos-one-byte.zrec', dir)));
const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

describe('parseHeader', () => {
  it('parses the descriptor and key block of a vector', () => {
    const h = parseHeader(clip());
    expect(h.formatVersion).toBe(1);
    expect(h.suite).toBe(1);
    expect(h.headerLen).toBe(105 + 'front-door'.length);
    expect(h.chunkSize).toBe(manifest.chunk_size);
    expect(h.cameraId).toBe(manifest.camera_id);
    expect(h.startUnixMs).toBe(manifest.start_unix_ms);
    expect(h.clipId).toBe(manifest.clip_id);
    expect(hex(h.keyId)).toBe(manifest.derived.key_id_hex);
    expect(h.wrapNonce).toHaveLength(12);
    expect(h.wrappedDek).toHaveLength(48);
    expect(h.descriptor).toHaveLength(37 + 'front-door'.length);
  });

  it('ignores bytes after header_len', () => {
    const b = clip();
    expect(parseHeader(b.subarray(0, 115)).cameraId).toBe('front-door');
  });

  it('reports input shorter than the fixed descriptor as ErrTruncated', () => {
    expect(() => parseHeader(clip().subarray(0, 36))).toThrow(ErrTruncated);
  });

  it('reports input shorter than header_len as ErrTruncated', () => {
    expect(() => parseHeader(clip().subarray(0, 114))).toThrow(ErrTruncated);
  });

  const mutated = (edit: (b: Uint8Array) => void) => () => {
    const b = clip();
    edit(b);
    return parseHeader(b);
  };

  it.each([
    ['bad magic', (b: Uint8Array) => void (b[0] = 0x58)],
    ['unsupported format_version', (b: Uint8Array) => void (b[4] = 2)],
    ['unsupported suite', (b: Uint8Array) => void (b[5] = 2)],
    ['header_len inconsistent with camera_id_len', (b: Uint8Array) => void (b[7] = 116)],
    ['chunk_size not a power of two', (b: Uint8Array) => void (b[9] = 0x01, b[10] = 0x80)],
    ['chunk_size below the minimum', (b: Uint8Array) => void (b.set([0, 0, 0x80, 0], 8))],
    ['chunk_size above the maximum', (b: Uint8Array) => void (b.set([0x02, 0, 0, 0], 8))],
    ['camera_id with an invalid character', (b: Uint8Array) => void (b[37] = 0x46 /* F */)],
    ['camera_id starting with a dash', (b: Uint8Array) => void (b[37] = 0x2d)],
    ['clip_id timestamp differing from start_unix_ms', (b: Uint8Array) => void (b[35] = b[35]! ^ 1)],
  ])('reports %s as ErrFormat', (_name, edit) => {
    expect(mutated(edit)).toThrow(ErrFormat);
  });

  it('reports a zero-length camera_id as ErrFormat', () => {
    // n = 0 with a consistent header_len of 105.
    const b = clip();
    b[7] = 105;
    b[36] = 0;
    expect(() => parseHeader(b)).toThrow(ErrFormat);
  });

  // Contract §4.1/§4.4: camera_id_len outside 1..64 is ErrFormat whatever the
  // buffer size; the worker fetches only bytes=0-168.
  it.each([37, 100, 169, 205])('reports camera_id_len 100 as ErrFormat given %i bytes', (len) => {
    const b = new Uint8Array(300);
    b.set(clip().subarray(0, 37));
    b[6] = 0;
    b[7] = 205; // header_len = 105 + 100, consistent
    b[36] = 100;
    expect(() => parseHeader(b.subarray(0, len))).toThrow(ErrFormat);
  });

  it('reports a zero camera_id_len as ErrFormat even when the input is short', () => {
    const b = clip().slice(0, 104);
    b[7] = 105;
    b[36] = 0;
    expect(() => parseHeader(b)).toThrow(ErrFormat);
  });
});
