// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { browserExportEnv } from './export';

const IPHONE = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';
const MAC = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';
const ANDROID = 'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36';

const touched = new Set<string>();

/** Replaces a navigator property for one test (jsdom keeps them on the prototype). */
function setNav(name: string, value: unknown): void {
  touched.add(name);
  Object.defineProperty(navigator, name, { value, configurable: true });
}

beforeEach(() => {
  setNav('userAgent', ANDROID);
  setNav('platform', 'Linux armv8l');
  setNav('maxTouchPoints', 5);
  setNav('share', undefined);
  setNav('canShare', undefined);
});

afterEach(() => {
  // Drop the own properties so the prototype's values show again.
  for (const name of touched) delete (navigator as unknown as Record<string, unknown>)[name];
  touched.clear();
  delete (window as unknown as { showSaveFilePicker?: unknown }).showSaveFilePicker;
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('browserExportEnv', () => {
  describe('iOS detection', () => {
    it('is true for an iPhone user agent', () => {
      setNav('userAgent', IPHONE);
      expect(browserExportEnv().isIOS).toBe(true);
    });
    it('is true for an iPad that reports itself as a Mac with a touch screen', () => {
      setNav('userAgent', MAC);
      setNav('platform', 'MacIntel');
      setNav('maxTouchPoints', 5);
      expect(browserExportEnv().isIOS).toBe(true);
    });
    it('is false for a real Mac, which has no touch points', () => {
      setNav('userAgent', MAC);
      setNav('platform', 'MacIntel');
      setNav('maxTouchPoints', 0);
      expect(browserExportEnv().isIOS).toBe(false);
    });
    it('is false for Android', () => {
      expect(browserExportEnv().isIOS).toBe(false);
    });
  });

  describe('feature presence', () => {
    it('has none of the optional features when the browser lacks them', () => {
      const e = browserExportEnv();
      expect(e.showSaveFilePicker).toBeUndefined();
      expect(e.share).toBeUndefined();
      expect(e.canShare).toBeUndefined();
      expect(typeof e.download).toBe('function');
    });

    it('binds showSaveFilePicker to window', async () => {
      const handle = { createWritable: async () => ({}) };
      const picker = vi.fn(async () => handle);
      (window as unknown as { showSaveFilePicker: unknown }).showSaveFilePicker = picker;
      const e = browserExportEnv();
      expect(await e.showSaveFilePicker!({ suggestedName: 'a.mp4', types: [] })).toBe(handle);
      expect(picker.mock.contexts).toEqual([window]);
    });

    it('wraps share and canShare so they keep their navigator receiver', async () => {
      const share = vi.fn(async () => {});
      const canShare = vi.fn(() => true);
      setNav('share', share);
      setNav('canShare', canShare);
      const e = browserExportEnv();
      const data = { files: [] as File[] };
      await e.share!(data);
      expect(e.canShare!(data)).toBe(true);
      expect(share.mock.calls).toEqual([[data]]);
      expect(canShare.mock.calls).toEqual([[data]]);
      expect(share.mock.contexts).toEqual([navigator]);
      expect(canShare.mock.contexts).toEqual([navigator]);
    });
  });

  describe('download', () => {
    it('clicks an anchor with the download name and revokes the object URL later', () => {
      vi.useFakeTimers();
      const create = vi.fn(() => 'blob:zview/1');
      const revoke = vi.fn();
      Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke });
      const anchors: HTMLAnchorElement[] = [];
      const attached: boolean[] = [];
      vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
        anchors.push(this);
        attached.push(document.body.contains(this));
      });
      const blob = new Blob([new Uint8Array(4)], { type: 'video/mp4' });

      browserExportEnv().download(blob, 'front-door_20261106-215905.mp4');

      expect(create).toHaveBeenCalledWith(blob);
      expect(anchors).toHaveLength(1);
      expect(anchors[0]!.download).toBe('front-door_20261106-215905.mp4');
      expect(anchors[0]!.getAttribute('href')).toBe('blob:zview/1');
      // The anchor is in the document while it is clicked (Firefox needs that) and gone again afterwards.
      expect(attached).toEqual([true]);
      expect(document.body.contains(anchors[0]!)).toBe(false);
      // The URL stays alive for the browser to read, then is revoked.
      expect(revoke).not.toHaveBeenCalled();
      vi.advanceTimersByTime(60_000);
      expect(revoke).toHaveBeenCalledWith('blob:zview/1');
    });
  });
});
