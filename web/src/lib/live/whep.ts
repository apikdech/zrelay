// WHEP client for live view (design §7.2). The main thread owns the
// RTCPeerConnection (it does not exist in workers); the worker performs the
// signaling POST/DELETE with K_api, so this code sees only SDP and media.
import type { Request } from '../../worker/protocol';
import type { LiveOfferResult } from '../../worker/protocol';

/** The slice of the worker client live view needs. */
export interface WhepClient {
  call<T = unknown>(msg: Request): Promise<T>;
}

export type WhepState = 'connecting' | 'connected' | 'lost';

export interface WhepSession {
  /** Ends the session: closes the peer and DELETEs the session resource. Idempotent. */
  close(): Promise<void>;
  /** An audio track has arrived. */
  readonly hasAudio: boolean;
  readonly state: WhepState;
  /** Called on a change to `connected` or `lost`. Not called after close(). */
  onstate: ((s: WhepState) => void) | null;
  /** Called when the audio track arrives. */
  onaudio: (() => void) | null;
}

/** The trickle-free offer must carry every candidate; do not wait forever for it. */
const GATHER_TIMEOUT_MS = 4000;

function gatheringComplete(pc: RTCPeerConnection): Promise<void> {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, GATHER_TIMEOUT_MS);
    function done() {
      clearTimeout(timer);
      pc.onicegatheringstatechange = null;
      resolve();
    }
    pc.onicegatheringstatechange = () => {
      if (pc.iceGatheringState === 'complete') done();
    };
  });
}

/**
 * Starts a WHEP session for `cameraId` into `video` (muted: autoplay policies
 * require it). Resolves once the answer is applied; media may still be
 * connecting, which `session.state` and `onstate` report.
 */
export async function startWhep(video: HTMLVideoElement, cameraId: string, client: WhepClient): Promise<WhepSession> {
  const pc = new RTCPeerConnection();
  let sessionPath: string | null = null;
  let closed = false;
  let state: WhepState = 'connecting';
  let audio = false;
  const stream = new MediaStream();

  const session: WhepSession = {
    get hasAudio() {
      return audio;
    },
    get state() {
      return state;
    },
    onstate: null,
    onaudio: null,
    async close() {
      if (closed) return;
      closed = true;
      pc.onconnectionstatechange = null;
      pc.ontrack = null;
      pc.close();
      if (video.srcObject === stream) video.srcObject = null;
      if (sessionPath !== null) {
        try {
          await client.call({ type: 'live.close', sessionPath });
        } catch {
          // The server also ends a session whose peer is gone.
        }
      }
    },
  };

  video.muted = true;
  video.playsInline = true;
  pc.ontrack = (e) => {
    stream.addTrack(e.track);
    if (video.srcObject !== stream) {
      video.srcObject = stream;
      void video.play?.()?.catch?.(() => {});
    }
    if (e.track.kind === 'audio' && !audio) {
      audio = true;
      session.onaudio?.();
    }
  };
  pc.onconnectionstatechange = () => {
    const next: WhepState | null =
      pc.connectionState === 'connected' ? 'connected' : pc.connectionState === 'failed' ? 'lost' : null;
    if (next === null || next === state || closed) return;
    state = next;
    session.onstate?.(next);
  };

  try {
    pc.addTransceiver('video', { direction: 'recvonly' });
    pc.addTransceiver('audio', { direction: 'recvonly' });
    await pc.setLocalDescription(await pc.createOffer());
    await gatheringComplete(pc);
    const offer = pc.localDescription;
    if (!offer) throw new Error('no local description');
    const res = await client.call<LiveOfferResult>({ type: 'live.offer', cameraId, sdp: offer.sdp });
    sessionPath = res.sessionPath;
    await pc.setRemoteDescription({ type: 'answer', sdp: res.sdp });
  } catch (e) {
    await session.close();
    throw e;
  }
  return session;
}
