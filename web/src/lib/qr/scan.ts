// In-app QR scanning (ui-spec §4.1): `BarcodeDetector` where the browser has it,
// otherwise the small `jsqr` decoder. The camera stream stays on this device;
// nothing is uploaded and the decoded text is never put in a URL.

/** Raw RGBA pixels, the shape both `ImageData` and jsqr use. */
export interface Frame {
  data: Uint8ClampedArray;
  width: number;
  height: number;
}

export type Decoder = (frame: Frame) => Promise<string | null>;

interface DetectorLike {
  detect(image: unknown): Promise<{ rawValue: string }[]>;
}
type DetectorCtor = new (opts: { formats: string[] }) => DetectorLike;

export interface DecoderEnv {
  /** Defaults to the global `BarcodeDetector`. */
  BarcodeDetector?: DetectorCtor;
}

type JsQr = (data: Uint8ClampedArray, width: number, height: number) => { data: string } | null;
let jsqrLoad: Promise<JsQr> | null = null;
/** jsqr is loaded only when the browser has no BarcodeDetector (or it failed). */
const loadJsQr = (): Promise<JsQr> => (jsqrLoad ??= import('jsqr').then((m) => m.default as JsQr));

export function createDecoder(env: DecoderEnv = { BarcodeDetector: (globalThis as DecoderEnv).BarcodeDetector }): Decoder {
  let detector: DetectorLike | null | undefined;
  const getDetector = (): DetectorLike | null => {
    if (detector === undefined) {
      try {
        detector = env.BarcodeDetector ? new env.BarcodeDetector({ formats: ['qr_code'] }) : null;
      } catch {
        detector = null;
      }
    }
    return detector;
  };
  return async (frame) => {
    const d = getDetector();
    if (d) {
      try {
        // BarcodeDetector.detect takes ImageData; the Frame shape is structurally the same.
        const found = await d.detect(frame);
        return found[0]?.rawValue ?? null;
      } catch {
        detector = null; // unsupported in practice: use jsqr from now on
      }
    }
    const jsqr = await loadJsQr();
    return jsqr(frame.data, frame.width, frame.height)?.data ?? null;
  };
}

export interface ScanLoop {
  /** The current camera frame, or null while the camera has none yet. */
  grab: () => Frame | null;
  decode: Decoder;
  /** Pause between polls. */
  wait: (ms: number) => Promise<void>;
  signal: AbortSignal;
  intervalMs?: number;
}

const abortError = (): Error => Object.assign(new Error('scan aborted'), { name: 'AbortError' });

/** Polls frames until one decodes. Rejects with an AbortError when the signal fires. */
export async function scanFrames(loop: ScanLoop): Promise<string> {
  const { grab, decode, wait, signal, intervalMs = 150 } = loop;
  for (;;) {
    if (signal.aborted) throw abortError();
    const frame = grab();
    if (frame) {
      const text = await decode(frame);
      if (text) return text;
    }
    await wait(intervalMs);
  }
}

/** A running camera: a video element to show, a frame grabber and a release. */
export interface Camera {
  video: HTMLVideoElement;
  grab: () => Frame | null;
  stop: () => void;
}

/** Opens the rear camera with getUserMedia. Needs a secure context and permission. */
export async function openCamera(): Promise<Camera> {
  const stream = await navigator.mediaDevices.getUserMedia({
    video: { facingMode: { ideal: 'environment' } },
    audio: false,
  });
  const video = document.createElement('video');
  video.muted = true;
  video.playsInline = true; // iOS: keep it inline in the page
  video.srcObject = stream;
  const canvas = document.createElement('canvas');
  const stop = () => {
    for (const t of stream.getTracks()) t.stop();
    video.srcObject = null;
  };
  try {
    await video.play();
  } catch (e) {
    stop();
    throw e;
  }
  return {
    video,
    stop,
    grab() {
      const w = video.videoWidth;
      const h = video.videoHeight;
      if (!w || !h) return null;
      canvas.width = w;
      canvas.height = h;
      const ctx = canvas.getContext('2d', { willReadFrequently: true });
      if (!ctx) return null;
      ctx.drawImage(video, 0, 0, w, h);
      return ctx.getImageData(0, 0, w, h);
    },
  };
}
