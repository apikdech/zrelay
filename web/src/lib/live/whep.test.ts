import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeClient, FakePeer, FakeStream, fakeVideo } from '../../../test-support/live-fakes';
import { startWhep } from './whep';

beforeEach(() => {
  FakePeer.instances = [];
  FakePeer.stallGathering = false;
  FakePeer.failSetRemote = false;
  vi.stubGlobal('RTCPeerConnection', FakePeer);
  vi.stubGlobal('MediaStream', FakeStream);
});
afterEach(() => vi.unstubAllGlobals());

describe('startWhep', () => {
  it('offers recvonly video and audio, sends the gathered offer and applies the answer', async () => {
    const client = new FakeClient();
    const video = fakeVideo();
    const s = await startWhep(video, 'front', client);
    const pc = FakePeer.instances[0]!;
    expect(pc.transceivers).toEqual([
      { kind: 'video', init: { direction: 'recvonly' } },
      { kind: 'audio', init: { direction: 'recvonly' } },
    ]);
    expect(client.of('live.offer')).toEqual([
      { type: 'live.offer', cameraId: 'front', sdp: 'v=0 offer-with-candidates' },
    ]);
    expect(pc.remoteDescription).toEqual({ type: 'answer', sdp: 'v=0 answer' });
    expect(video.muted).toBe(true);
    expect(s.hasAudio).toBe(false);
  });

  it('does not send the offer until ICE gathering completes', async () => {
    const client = new FakeClient();
    FakePeer.stallGathering = true;
    const p = startWhep(fakeVideo(), 'front', client);
    await vi.waitFor(() => expect(FakePeer.instances[0]?.iceGatheringState).toBe('gathering'));
    await Promise.resolve();
    expect(client.of('live.offer')).toHaveLength(0);
    FakePeer.instances[0]!.finishGathering();
    await p;
    expect(client.of('live.offer')).toHaveLength(1);
  });

  it('attaches remote tracks to the video and reports audio only when an audio track exists', async () => {
    const video = fakeVideo();
    const s = await startWhep(video, 'front', new FakeClient());
    const pc = FakePeer.instances[0]!;
    pc.addRemoteTrack('video');
    expect(s.hasAudio).toBe(false);
    expect((video.srcObject as unknown as FakeStream).tracks.map((t) => t.kind)).toEqual(['video']);
    const seen: boolean[] = [];
    s.onaudio = () => seen.push(s.hasAudio);
    pc.addRemoteTrack('audio');
    expect(s.hasAudio).toBe(true);
    expect(seen).toEqual([true]);
  });

  it('tracks connection state and reports loss', async () => {
    const s = await startWhep(fakeVideo(), 'front', new FakeClient());
    const pc = FakePeer.instances[0]!;
    const states: string[] = [];
    s.onstate = (st) => states.push(st);
    pc.setState('connecting');
    pc.setState('connected');
    pc.setState('failed');
    expect(states).toEqual(['connected', 'lost']);
    expect(s.state).toBe('lost');
  });

  it('close() closes the peer, DELETEs the session via the worker and does not report loss', async () => {
    const client = new FakeClient();
    const video = fakeVideo();
    const s = await startWhep(video, 'front', client);
    const pc = FakePeer.instances[0]!;
    const states: string[] = [];
    s.onstate = (st) => states.push(st);
    await s.close();
    expect(pc.closed).toBe(true);
    expect(client.of('live.close')).toEqual([{ type: 'live.close', sessionPath: '/live/front/s1' }]);
    expect(video.srcObject).toBeNull();
    expect(states).toEqual([]);
    await s.close(); // idempotent
    expect(client.of('live.close')).toHaveLength(1);
  });

  it('closes the peer and rethrows when the worker rejects the offer', async () => {
    const client = new FakeClient();
    client.offerFailures.push({ name: 'LiveBusy' });
    await expect(startWhep(fakeVideo(), 'front', client)).rejects.toMatchObject({ name: 'LiveBusy' });
    expect(FakePeer.instances[0]!.closed).toBe(true);
  });

  it('closes the remote session when applying the answer fails', async () => {
    const client = new FakeClient();
    FakePeer.failSetRemote = true;
    await expect(startWhep(fakeVideo(), 'front', client)).rejects.toThrow('bad answer');
    expect(FakePeer.instances[0]!.closed).toBe(true);
    expect(client.of('live.close')).toHaveLength(1);
  });
});
