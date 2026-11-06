// @vitest-environment jsdom
import qrcode from 'qrcode-generator';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createDecoder, openCamera, scanFrames, type Frame } from './scan';

/** Renders text as a QR code into an RGBA frame with a quiet zone. */
function render(text: string, scale = 4): Frame {
  const qr = qrcode(0, 'M');
  qr.addData(text);
  qr.make();
  const n = qr.getModuleCount();
  const quiet = 4;
  const size = (n + 2 * quiet) * scale;
  const data = new Uint8ClampedArray(size * size * 4).fill(255);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const mx = Math.floor(x / scale) - quiet;
      const my = Math.floor(y / scale) - quiet;
      if (mx >= 0 && my >= 0 && mx < n && my < n && qr.isDark(my, mx)) {
        const i = (y * size + x) * 4;
        data[i] = data[i + 1] = data[i + 2] = 0;
      }
    }
  }
  return { data, width: size, height: size };
}

const KEY = 'zk1-' + 'ab'.repeat(32) + '-1234';

describe('createDecoder', () => {
  it('uses BarcodeDetector when the browser has it', async () => {
    const detect = vi.fn(async () => [{ rawValue: KEY }]);
    const Detector = vi.fn(function () {
      return { detect };
    });
    const decode = createDecoder({ BarcodeDetector: Detector as never });
    expect(await decode(render(KEY))).toBe(KEY);
    expect(Detector).toHaveBeenCalledWith({ formats: ['qr_code'] });
    expect(detect).toHaveBeenCalledTimes(1);
  });

  it('returns null when the detector sees no code', async () => {
    const Detector = function () {
      return { detect: async () => [] };
    };
    expect(await createDecoder({ BarcodeDetector: Detector as never })(render(KEY))).toBeNull();
  });

  it('falls back to jsqr, which really decodes a rendered QR code', async () => {
    const decode = createDecoder({ BarcodeDetector: undefined });
    expect(await decode(render(KEY))).toBe(KEY);
  });

  it('falls back to jsqr when the detector throws', async () => {
    const Detector = function () {
      return {
        detect: async () => {
          throw new Error('unsupported');
        },
      };
    };
    expect(await createDecoder({ BarcodeDetector: Detector as never })(render(KEY))).toBe(KEY);
  });

  it('returns null for a frame with no code', async () => {
    const blank: Frame = { data: new Uint8ClampedArray(64 * 64 * 4).fill(255), width: 64, height: 64 };
    expect(await createDecoder({ BarcodeDetector: undefined })(blank)).toBeNull();
  });
});

describe('scanFrames', () => {
  it('keeps grabbing frames until one decodes, then resolves with its text', async () => {
    let grabs = 0;
    const waits: number[] = [];
    const result = await scanFrames({
      grab: () => ({ data: new Uint8ClampedArray(4), width: 1, height: 1 }),
      decode: async () => (++grabs === 3 ? KEY : null),
      wait: async (ms) => void waits.push(ms),
      signal: new AbortController().signal,
    });
    expect(result).toBe(KEY);
    expect(grabs).toBe(3);
    expect(waits).toHaveLength(2);
  });

  it('skips polls where the camera has no frame yet', async () => {
    const frames = [null, null, { data: new Uint8ClampedArray(4), width: 1, height: 1 }];
    const result = await scanFrames({
      grab: () => frames.shift() ?? null,
      decode: async () => KEY,
      wait: async () => {},
      signal: new AbortController().signal,
    });
    expect(result).toBe(KEY);
  });

  it('rejects with AbortError when cancelled', async () => {
    const ac = new AbortController();
    const p = scanFrames({
      grab: () => null,
      decode: async () => null,
      wait: async () => ac.abort(),
      signal: ac.signal,
    });
    await expect(p).rejects.toMatchObject({ name: 'AbortError' });
  });
});

describe('openCamera', () => {
  const origMedia = Object.getOwnPropertyDescriptor(navigator, 'mediaDevices');
  afterEach(() => {
    vi.restoreAllMocks();
    if (origMedia) Object.defineProperty(navigator, 'mediaDevices', origMedia);
    else delete (navigator as unknown as { mediaDevices?: unknown }).mediaDevices;
  });

  function stubStream() {
    const tracks = [{ stop: vi.fn() }, { stop: vi.fn() }];
    const stream = { getTracks: () => tracks } as unknown as MediaStream;
    const getUserMedia = vi.fn(async () => stream);
    Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: { getUserMedia } });
    return { tracks, getUserMedia };
  }

  it('asks for the rear camera, video only, and plays inline and muted', async () => {
    const { getUserMedia } = stubStream();
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue();
    const cam = await openCamera();
    expect(getUserMedia).toHaveBeenCalledWith({ video: { facingMode: { ideal: 'environment' } }, audio: false });
    expect(play).toHaveBeenCalledTimes(1);
    expect(cam.video.muted).toBe(true);
    expect(cam.video.playsInline).toBe(true);
  });

  it('stops every track and rethrows when play() is rejected', async () => {
    const { tracks } = stubStream();
    const boom = new Error('NotAllowedError');
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockRejectedValue(boom);
    await expect(openCamera()).rejects.toBe(boom);
    for (const t of tracks) expect(t.stop).toHaveBeenCalledTimes(1);
  });

  it('stop() releases every track', async () => {
    const { tracks } = stubStream();
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue();
    const cam = await openCamera();
    cam.stop();
    for (const t of tracks) expect(t.stop).toHaveBeenCalledTimes(1);
    expect(cam.video.srcObject).toBeNull();
  });

  it('grab() is null while the video has no dimensions, then returns the canvas pixels', async () => {
    stubStream();
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue();
    const image = { data: new Uint8ClampedArray(2 * 3 * 4), width: 2, height: 3 };
    const drawImage = vi.fn();
    const getImageData = vi.fn(() => image);
    const getContext = vi
      .spyOn(HTMLCanvasElement.prototype, 'getContext')
      .mockReturnValue({ drawImage, getImageData } as unknown as CanvasRenderingContext2D);
    const cam = await openCamera();

    expect(cam.grab()).toBeNull(); // jsdom: videoWidth/videoHeight are 0
    expect(getContext).not.toHaveBeenCalled();

    Object.defineProperty(cam.video, 'videoWidth', { configurable: true, value: 2 });
    Object.defineProperty(cam.video, 'videoHeight', { configurable: true, value: 3 });
    expect(cam.grab()).toBe(image);
    expect(drawImage).toHaveBeenCalledWith(cam.video, 0, 0, 2, 3);
    expect(getImageData).toHaveBeenCalledWith(0, 0, 2, 3);
  });

  it('grab() is null when the canvas has no 2d context', async () => {
    stubStream();
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue();
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null);
    const cam = await openCamera();
    Object.defineProperty(cam.video, 'videoWidth', { configurable: true, value: 2 });
    Object.defineProperty(cam.video, 'videoHeight', { configurable: true, value: 3 });
    expect(cam.grab()).toBeNull();
  });
});
