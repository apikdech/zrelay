// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { isValidZone, loadZonePref, saveZonePref, zoneChoices, ZONE_KEY } from './zonePref';

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe('isValidZone', () => {
  it('accepts IANA zones and rejects anything else', () => {
    expect(isValidZone('Asia/Jakarta')).toBe(true);
    expect(isValidZone('UTC')).toBe(true);
    expect(isValidZone('Mars/Olympus')).toBe(false);
    expect(isValidZone('')).toBe(false);
  });
});

describe('zone preference', () => {
  it('is null until one is saved', () => {
    expect(loadZonePref()).toBeNull();
  });

  it('round-trips a zone and clears it with null', () => {
    saveZonePref('Asia/Jakarta');
    expect(loadZonePref()).toBe('Asia/Jakarta');
    saveZonePref(null);
    expect(loadZonePref()).toBeNull();
    expect(localStorage.getItem(ZONE_KEY)).toBeNull();
  });

  it('ignores a stored value that is not a zone', () => {
    localStorage.setItem(ZONE_KEY, 'not/a-zone');
    expect(loadZonePref()).toBeNull();
  });

  it('does not save an invalid zone', () => {
    saveZonePref('not/a-zone');
    expect(localStorage.getItem(ZONE_KEY)).toBeNull();
  });

  it('survives storage that throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(loadZonePref()).toBeNull();
    expect(() => saveZonePref('Asia/Jakarta')).not.toThrow();
  });
});

describe('zoneChoices', () => {
  it('lists Asia/Jakarta and always includes the current zone', () => {
    expect(zoneChoices(null)).toContain('Asia/Jakarta');
    const withOdd = zoneChoices('Etc/GMT+5');
    expect(withOdd).toContain('Etc/GMT+5');
    expect(new Set(withOdd).size).toBe(withOdd.length);
  });
});
