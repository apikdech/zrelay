// WHEP signaling for live view. The worker does the POST/DELETE with K_api;
// RTCPeerConnection runs on the main thread, which sees only SDP.
import { readBounded, type AuthFetch } from './api';
import type { LiveOfferResult } from './protocol';
import { SchemaError, isCameraId } from './schemas';

/** zview-server caps the offer body at 64 KiB. */
const MAX_SDP_BYTES = 64 * 1024;
const SESSION_RE = /^\/live\/([a-z0-9][a-z0-9-]{0,63})\/((?!\.+$)[A-Za-z0-9._~-]{1,128})$/;

/** The live proxy answered 503: the camera's viewer limit is reached. */
export class LiveBusyError extends Error {
  override name = 'LiveBusy';
}

export class LiveError extends Error {
  override name = 'LiveError';
  readonly status?: number;
  constructor(message: string, status?: number) {
    super(message);
    this.status = status;
  }
}

export async function liveOffer(authFetch: AuthFetch, cameraId: string, sdp: string): Promise<LiveOfferResult> {
  if (!isCameraId(cameraId)) throw new SchemaError('invalid camera id');
  if (typeof sdp !== 'string' || new TextEncoder().encode(sdp).length > MAX_SDP_BYTES) {
    throw new SchemaError('offer too large');
  }
  const res = await authFetch(`/live/${cameraId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/sdp', Accept: 'application/sdp' },
    body: sdp,
  });
  if (res.status === 503) {
    void res.body?.cancel();
    throw new LiveBusyError('viewer limit reached');
  }
  if (res.status !== 201 && res.status !== 200) {
    void res.body?.cancel();
    throw new LiveError(`live offer failed with status ${res.status}`, res.status);
  }
  const location = res.headers.get('Location') ?? '';
  let path: string;
  try {
    // Only the path is kept: the DELETE always goes to our own origin.
    path = new URL(location, 'http://same-origin.invalid').pathname;
  } catch {
    throw new LiveError('live offer returned an unusable Location');
  }
  const m = SESSION_RE.exec(path);
  if (!m || m[1] !== cameraId) throw new LiveError('live offer returned a Location outside the camera session space');
  const answer = new TextDecoder().decode(
    await readBounded(res, MAX_SDP_BYTES, () => new LiveError('answer too large')),
  );
  return { sdp: answer, sessionPath: path };
}

export async function liveClose(authFetch: AuthFetch, sessionPath: string): Promise<Record<string, never>> {
  if (typeof sessionPath !== 'string' || !SESSION_RE.test(sessionPath)) throw new SchemaError('invalid session path');
  const res = await authFetch(sessionPath, { method: 'DELETE' });
  void res.body?.cancel();
  // 404: the session already ended, which is what the caller wanted.
  if (!res.ok && res.status !== 404) throw new LiveError(`live close failed with status ${res.status}`, res.status);
  return {};
}
