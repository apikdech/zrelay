import { describe, expect, it } from 'vitest';
import { masterText } from '../../../test-support/worker-fixtures';
import { checkKeyInput, fingerprint } from './keyInput';

describe('checkKeyInput (live feedback while typing)', () => {
  it('is empty for blank input', () => {
    expect(checkKeyInput('')).toBe('empty');
    expect(checkKeyInput('  \n ')).toBe('empty');
  });

  it('accepts a well-formed key, ignoring case and whitespace', async () => {
    const text = await masterText();
    expect(checkKeyInput(text)).toBe('valid');
    expect(checkKeyInput(`  ${text.toUpperCase()}\n`)).toBe('valid');
    expect(checkKeyInput(text.replace(/-/g, '- '))).toBe('valid');
  });

  it('reports a typo when only the checksum disagrees', async () => {
    const text = await masterText();
    const flipped = text.slice(0, -1) + (text.endsWith('0') ? '1' : '0');
    expect(checkKeyInput(flipped)).toBe('typo');
  });

  it('reports a typo when a key digit changed under an unchanged checksum', async () => {
    const text = await masterText();
    const at = 4; // first hex digit of the key body
    const swapped = text.slice(0, at) + (text[at] === 'a' ? 'b' : 'a') + text.slice(at + 1);
    expect(checkKeyInput(swapped)).toBe('typo');
  });

  it('reports a format problem for partial or foreign input', () => {
    expect(checkKeyInput('zk1-abc')).toBe('format');
    expect(checkKeyInput('hello')).toBe('format');
  });
});

describe('fingerprint', () => {
  it('groups a hex key_id in fours', () => {
    expect(fingerprint('a1b2c3d4e5f60718')).toBe('a1b2-c3d4-e5f6-0718');
  });
});
