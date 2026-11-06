import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeClient, FakePeer, FakeStream, fakeVideo } from '../../../test-support/live-fakes';
import { backoffDelay, createTile } from './tile';

class FakeDoc extends EventTarget {
  visibilityState: 'visible' | 'hidden' = 'visible';
  set(v: 'visible' | 'hidden') {
    this.visibilityState = v;
    this.dispatchEvent(new Event('visibilitychange'));
  }
}

let doc: FakeDoc;
let client: FakeClient;

function make(video = fakeVideo()) {
  return createTile({ video, cameraId: 'front', client, doc: doc as unknown as Document, onChange: () => {} });
}
const flush = async () => {
  for (let i = 0; i < 30; i++) await Promise.resolve();
};

beforeEach(() => {
  vi.useFakeTimers();
  FakePeer.instances = [];
  FakePeer.stallGathering = false;
  FakePeer.failSetRemote = false;
  doc = new FakeDoc();
  client = new FakeClient();
  vi.stubGlobal('RTCPeerConnection', FakePeer);
  vi.stubGlobal('MediaStream', FakeStream);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('backoffDelay', () => {
  it('doubles from 1 s and is bounded at 30 s', () => {
    expect([0, 1, 2, 3, 4, 5, 6, 50, 1000].map(backoffDelay)).toEqual([
      1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000, 30000,
    ]);
  });
});

describe('tile', () => {
  it('goes connecting -> live when the peer connects', async () => {
    const tile = make();
    tile.start();
    await flush();
    expect(tile.snapshot().state).toBe('connecting');
    FakePeer.instances[0]!.setState('connected');
    expect(tile.snapshot().state).toBe('live');
    tile.destroy();
  });

  it('closes the session when the tab is hidden and reopens it when visible', async () => {
    const tile = make();
    tile.start();
    await flush();
    FakePeer.instances[0]!.setState('connected');
    doc.set('hidden');
    await flush();
    expect(FakePeer.instances[0]!.closed).toBe(true);
    expect(client.of('live.close')).toHaveLength(1);
    expect(tile.snapshot().state).toBe('connecting');
    // Hidden: no timer opens a new session.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakePeer.instances).toHaveLength(1);
    doc.set('visible');
    await flush();
    expect(FakePeer.instances).toHaveLength(2);
    FakePeer.instances[1]!.setState('connected');
    expect(tile.snapshot().state).toBe('live');
    tile.destroy();
  });

  it('a tile started while the tab is hidden opens nothing until visible', async () => {
    doc.visibilityState = 'hidden';
    const tile = make();
    tile.start();
    await flush();
    expect(FakePeer.instances).toHaveLength(0);
    doc.set('visible');
    await flush();
    expect(FakePeer.instances).toHaveLength(1);
    tile.destroy();
  });

  it('a session that resolves after the tab was hidden is closed at once', async () => {
    FakePeer.stallGathering = true;
    const tile = make();
    tile.start();
    await flush();
    doc.set('hidden');
    FakePeer.stallGathering = false;
    FakePeer.instances[0]!.finishGathering();
    await flush();
    expect(FakePeer.instances[0]!.closed).toBe(true);
    expect(client.of('live.close')).toHaveLength(client.of('live.offer').length);
    tile.destroy();
  });

  it('503 from the offer shows limit and keeps retrying with backoff', async () => {
    client.offerFailures.push({ name: 'LiveBusy', status: 503 }, { name: 'LiveBusy', status: 503 });
    const tile = make();
    tile.start();
    await flush();
    expect(tile.snapshot().state).toBe('limit');
    await vi.advanceTimersByTimeAsync(999);
    expect(client.of('live.offer')).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(client.of('live.offer')).toHaveLength(2);
    expect(tile.snapshot().state).toBe('limit');
    // The second delay is 2 s.
    await vi.advanceTimersByTimeAsync(1999);
    expect(client.of('live.offer')).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(client.of('live.offer')).toHaveLength(3);
    FakePeer.instances.at(-1)!.setState('connected');
    expect(tile.snapshot().state).toBe('live');
    tile.destroy();
  });

  it('a lost connection reconnects with delays 1,2,4,... capped at 30 s, and a good connection resets them', async () => {
    const tile = make();
    tile.start();
    await flush();
    const delays: number[] = [];
    for (let i = 0; i < 8; i++) {
      const n = FakePeer.instances.length;
      FakePeer.instances[n - 1]!.setState('failed');
      expect(tile.snapshot().state).toBe('reconnecting');
      let waited = 0;
      while (FakePeer.instances.length === n) {
        await vi.advanceTimersByTimeAsync(1);
        waited++;
        if (waited > 40_000) throw new Error('no reconnect');
      }
      delays.push(waited);
    }
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]);
    // Connect, then fail again: the delay is back to 1 s.
    FakePeer.instances.at(-1)!.setState('connected');
    const n = FakePeer.instances.length;
    FakePeer.instances.at(-1)!.setState('failed');
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakePeer.instances).toHaveLength(n + 1);
    tile.destroy();
  });

  it('a non-503 offer failure shows reconnecting', async () => {
    client.offerFailures.push({ name: 'LiveError', status: 502 });
    const tile = make();
    tile.start();
    await flush();
    expect(tile.snapshot().state).toBe('reconnecting');
    tile.destroy();
  });

  it('tap-to-unmute works only when an audio track exists', async () => {
    const video = fakeVideo();
    const tile = make(video);
    tile.start();
    await flush();
    expect(tile.snapshot().canUnmute).toBe(false);
    tile.toggleMute();
    expect(video.muted).toBe(true);
    FakePeer.instances[0]!.addRemoteTrack('audio');
    expect(tile.snapshot().canUnmute).toBe(true);
    expect(tile.snapshot().muted).toBe(true);
    tile.toggleMute();
    expect(video.muted).toBe(false);
    expect(tile.snapshot().muted).toBe(false);
    tile.toggleMute();
    expect(video.muted).toBe(true);
    tile.destroy();
  });

  it('destroy closes the session, stops timers and stops reacting to visibility', async () => {
    const tile = make();
    tile.start();
    await flush();
    tile.destroy();
    await flush();
    expect(FakePeer.instances[0]!.closed).toBe(true);
    doc.set('hidden');
    doc.set('visible');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakePeer.instances).toHaveLength(1);
  });
});
