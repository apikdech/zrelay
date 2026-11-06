// Test doubles for live view: a fake RTCPeerConnection and a fake worker.
// Test-only; never imported by production code.
import type { Request } from '../src/worker/protocol';

export class FakeTrack {
  constructor(public kind: 'audio' | 'video') {}
}

export class FakePeer {
  static instances: FakePeer[] = [];
  /** When true, gathering never completes unless the test calls finishGathering(). */
  static stallGathering = false;
  static failSetRemote = false;

  iceGatheringState: 'new' | 'gathering' | 'complete' = 'new';
  connectionState: 'new' | 'connecting' | 'connected' | 'disconnected' | 'failed' | 'closed' = 'new';
  localDescription: { type: string; sdp: string } | null = null;
  remoteDescription: { type: string; sdp: string } | null = null;
  transceivers: { kind: string; init: { direction?: string } }[] = [];
  closed = false;
  onicegatheringstatechange: (() => void) | null = null;
  onconnectionstatechange: (() => void) | null = null;
  ontrack: ((e: { track: FakeTrack; streams: unknown[] }) => void) | null = null;

  constructor() {
    FakePeer.instances.push(this);
  }
  addTransceiver(kind: string, init: { direction?: string }) {
    this.transceivers.push({ kind, init });
  }
  async createOffer() {
    return { type: 'offer', sdp: 'v=0 offer-without-candidates' };
  }
  async setLocalDescription(d: { type: string; sdp: string }) {
    this.localDescription = { ...d };
    this.iceGatheringState = 'gathering';
    this.onicegatheringstatechange?.();
    if (!FakePeer.stallGathering) queueMicrotask(() => this.finishGathering());
  }
  finishGathering() {
    this.localDescription = { type: 'offer', sdp: 'v=0 offer-with-candidates' };
    this.iceGatheringState = 'complete';
    this.onicegatheringstatechange?.();
  }
  async setRemoteDescription(d: { type: string; sdp: string }) {
    if (FakePeer.failSetRemote) throw new Error('bad answer');
    this.remoteDescription = d;
  }
  setState(s: FakePeer['connectionState']) {
    this.connectionState = s;
    this.onconnectionstatechange?.();
  }
  addRemoteTrack(kind: 'audio' | 'video') {
    this.ontrack?.({ track: new FakeTrack(kind), streams: [] });
  }
  close() {
    this.closed = true;
    this.connectionState = 'closed';
  }
}

export class FakeStream {
  tracks: FakeTrack[] = [];
  addTrack(t: FakeTrack) {
    this.tracks.push(t);
  }
}

export interface Call {
  type: string;
  [k: string]: unknown;
}

/** A fake worker client: records calls, answers live.offer / live.close. */
export class FakeClient {
  calls: Call[] = [];
  /** Per-offer behaviour: an error to fail with, or nothing to succeed. */
  offerFailures: { name: string; status?: number }[] = [];
  private n = 0;
  async call<T = unknown>(msg: Request): Promise<T> {
    this.calls.push(msg as Call);
    if (msg.type === 'live.offer') {
      const f = this.offerFailures.shift();
      if (f) throw Object.assign(new Error(f.name), f);
      this.n++;
      return { sdp: 'v=0 answer', sessionPath: `/live/${msg.cameraId}/s${this.n}` } as T;
    }
    if (msg.type === 'live.close') return {} as T;
    throw new Error('unexpected ' + msg.type);
  }
  of(type: string): Call[] {
    return this.calls.filter((c) => c.type === type);
  }
}

export function fakeVideo() {
  return { srcObject: null as unknown, muted: false, playsInline: false, play: async () => {} } as unknown as HTMLVideoElement;
}
