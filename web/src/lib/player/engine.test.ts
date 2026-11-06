import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ErrAuth, ErrAuthHeader, ErrTrailingData, ErrTruncated, ErrUnknownKey } from '../zrec/errors';
import {
  CHUNK,
  FakeClient,
  FakeManagedMediaSource,
  FakeMediaSource,
  chunkBytes,
  installMedia,
  openResult,
  tick,
  until,
  type Env,
} from '../../../test-support/player-fakes';
import { createPlayer, type Player, type PlayerState } from './engine';
import type { WorkerClient } from '../workerClient';

let env: Env;
beforeEach(() => {
  env = installMedia({ mms: false });
});
afterEach(() => env.restore());

const asVideo = (e: Env) => e.video as unknown as HTMLVideoElement;
const asClient = (c: FakeClient) => c as unknown as WorkerClient;
const CLIP = '01M2NBMVS0K8EKRQKGGAJCDT0B';

function make(client: FakeClient, profile: 'mobile' | 'desktop' = 'desktop'): { p: Player; states: PlayerState[] } {
  const p = createPlayer(asVideo(env), asClient(client), { profile });
  const states: PlayerState[] = [];
  p.onState((s) => states.push(s));
  return { p, states };
}

const sb = () => env.mediaSources()[0]!.sb!;
const last = (s: PlayerState[]) => s[s.length - 1]!;

describe('capability order', () => {
  it('prefers ManagedMediaSource and disables remote playback', async () => {
    env.restore();
    env = installMedia({ mms: true });
    const { p } = make(new FakeClient());
    await p.load(CLIP);
    const ms = env.mediaSources()[0]!;
    expect(ms).toBeInstanceOf(FakeManagedMediaSource);
    expect(env.video.disableRemotePlayback).toBe(true);
    await until(() => ms.mime !== '', 'source buffer');
    expect(ms.mime).toBe('video/mp4; codecs="avc1.640016"');
    expect(sb().mode).toBe('segments');
  });

  it('uses MediaSource when ManagedMediaSource is absent', async () => {
    const { p, states } = make(new FakeClient());
    await p.load(CLIP);
    expect(env.mediaSources()[0]).toBeInstanceOf(FakeMediaSource);
    await until(() => last(states).engine === 'mse', 'engine mse');
    expect(env.video.disableRemotePlayback).toBe(false);
  });

  it('falls back to MediaSource when ManagedMediaSource does not support the codec', async () => {
    env.restore();
    env = installMedia({ mms: true });
    FakeManagedMediaSource.supported = false;
    const { p, states } = make(new FakeClient());
    await p.load(CLIP);
    await until(() => last(states).engine === 'mse', 'engine mse');
  });

  it('falls back to Blob when there is no MSE at all', async () => {
    env.restore();
    env = installMedia({ mse: false, mms: false });
    const { p, states } = make(new FakeClient({ open: openResult({ numChunks: 3 }) }));
    await p.load(CLIP);
    await until(() => last(states).engine === 'blob' && env.video.src !== '', 'blob src');
    expect(env.registry.get(env.video.src)).toBeInstanceOf(Blob);
  });

  it('shows "can\'t play" when the codec is unsupported everywhere', async () => {
    FakeMediaSource.supported = false;
    env.video.canPlay = '';
    const { p, states } = make(new FakeClient());
    const st = await p.load(CLIP);
    expect(st).toBe('verified');
    expect(last(states).message).toContain("can't play avc1.640016");
    expect(last(states).engine).toBeNull();
    expect(env.mediaSources()).toHaveLength(0);
  });
});

describe('integrity states (ui-spec §4.4)', () => {
  it('starts verifying and resolves verified when the header and G3 check out', async () => {
    const { p, states } = make(new FakeClient());
    const pending = p.load(CLIP);
    expect(last(states).integrity).toBe('verifying');
    expect(await pending).toBe('verified');
    expect(last(states).integrity).toBe('verified');
  });

  it('a G3 mismatch resolves mismatch and exposes the authoritative header values', async () => {
    const open = openResult({ verification: { camera: 'mismatch', start: 'match', clipId: 'match' } });
    const { p, states } = make(new FakeClient({ open }));
    expect(await p.load(CLIP)).toBe('mismatch');
    expect(last(states).header).toEqual({ cameraId: 'front-door', startUnixMs: 1_700_000_000_000 });
    expect(last(states).verification?.camera).toBe('mismatch');
  });

  it('a sidecar status of incomplete resolves incomplete', async () => {
    const { p } = make(new FakeClient({ open: openResult({ status: 'incomplete', seekIndex: [] }) }));
    expect(await p.load(CLIP)).toBe('incomplete');
  });

  it('an unknown key resolves unknown-key with the sidecar key id hint', async () => {
    const c = new FakeClient({
      open: new ErrUnknownKey('key_id not in keyring'),
      sidecar: { video: { key_id: 'a1b2c3d4e5f60718' } },
    });
    const { p, states } = make(c);
    expect(await p.load(CLIP)).toBe('unknown-key');
    expect(last(states).keyId).toBe('a1b2-c3d4-e5f6-0718');
    expect(env.mediaSources()).toHaveLength(0);
  });

  it('a header that fails to authenticate resolves tampered', async () => {
    const { p } = make(new FakeClient({ open: new ErrAuthHeader('DEK unwrap failed') }));
    expect(await p.load(CLIP)).toBe('tampered');
  });

  it('other failures (network, auth) reject and surface a message', async () => {
    const { p, states } = make(new FakeClient({ open: new Error('boom') }));
    await expect(p.load(CLIP)).rejects.toThrow('boom');
    expect(last(states).message).toBeTruthy();
  });
});

describe('sequential play (ui-spec §4.3.1)', () => {
  it('appends chunks in order and ends the stream after the final chunk', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 5 }) });
    const { p } = make(c);
    await p.load(CLIP);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'end of stream');
    expect(sb().appends.map((a) => a[0])).toEqual([0, 1, 2, 3, 4]);
    expect(sb().appends.every((a) => a.length === CHUNK)).toBe(true);
  });

  it('stops fetching once ahead_seconds is buffered and resumes as the playhead moves', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile'); // ahead 30
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    await tick(50);
    expect(sb().appends).toHaveLength(30);

    env.video.currentTime = 10;
    env.video.fire('timeupdate');
    await until(() => sb().appends.length === 40, '40 chunks');
    await tick(50);
    expect(sb().appends).toHaveLength(40);
    p.destroy();
  });

  it('evicts what falls behind currentTime - behind_seconds after each append', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile'); // behind 15
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    env.video.currentTime = 25;
    env.video.fire('timeupdate');
    await until(() => sb().appends.length === 55, 'more chunks');
    expect(sb().removes.length).toBeGreaterThan(0);
    // Nothing left before 25 - 15 = 10.
    expect(sb().ranges[0]![0]).toBeGreaterThanOrEqual(10);
    p.destroy();
  });

  it('retries once after QuotaExceededError, evicting first', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    env.video.currentTime = 25;
    sb().quotaFailures = 1;
    env.video.fire('timeupdate');
    await until(() => sb().appends.length >= 31, 'append after quota');
    const quotaRemovals = sb().removes.length;
    expect(quotaRemovals).toBeGreaterThan(0);
    p.destroy();
  });

  it('a second QuotaExceededError is surfaced as an error message', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p, states } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    env.video.currentTime = 25;
    sb().quotaFailures = 2;
    env.video.fire('timeupdate');
    await until(() => last(states).message !== null, 'error message');
    p.destroy();
  });

  it('honours endstreaming and startstreaming on ManagedMediaSource', async () => {
    env.restore();
    env = installMedia({ mms: true });
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    c.hold.add(3);
    const { p } = make(c, 'desktop');
    await p.load(CLIP);
    await until(() => c.held.has(3), 'read of chunk 3 in flight');
    const ms = env.mediaSources()[0]!;
    ms.dispatchEvent(new Event('endstreaming'));
    c.held.get(3)!();
    await until(() => sb().appends.length === 4, 'chunk 3 appended');
    await tick(100);
    expect(sb().appends).toHaveLength(4);
    ms.dispatchEvent(new Event('startstreaming'));
    await until(() => sb().appends.length > 4, 'resume');
    p.destroy();
  });
});

describe('failures while playing', () => {
  it('ErrAuth on chunk 4 -> tampered at the last verified fragment, stream stops', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 10 }),
      read: (i) => (i === 4 ? new ErrAuth(4) : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    expect(last(states).failedAtMs).toBe(4000);
    expect(sb().appends.map((a) => a[0])).toEqual([0, 1, 2, 3]);
    // The verified part is allowed to play out, then the stream is ended.
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'ended');
  });

  it('ErrTrailingData is tampered too', async () => {
    const c = new FakeClient({ read: (i) => (i === 2 ? new ErrTrailingData('x') : { plaintext: chunkBytes(i), final: false }) });
    const { p, states } = make(c);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
  });

  it('ErrTruncated on a complete clip is incomplete and the verified part still plays', async () => {
    const c = new FakeClient({ read: (i) => (i === 3 ? new ErrTruncated('final chunk missing') : { plaintext: chunkBytes(i), final: false }) });
    const { p, states } = make(c);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'incomplete', 'incomplete');
    expect(sb().appends.map((a) => a[0])).toEqual([0, 1, 2]);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'ended');
  });

  it('tampered is sticky: a later seek past the failure is clamped to it', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 10 }),
      read: (i) => (i === 4 ? new ErrAuth(4) : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    await p.seek(8000);
    expect(env.video.currentTime).toBeLessThanOrEqual(4);
    expect(last(states).integrity).toBe('tampered');
  });
});

describe('seeking after a tampered failure', () => {
  const tamperedAt4 = () =>
    new FakeClient({
      open: openResult({ numChunks: 100 }),
      read: (i) => (i === 4 ? new ErrAuth(4) : { plaintext: chunkBytes(i), final: false }),
    });

  it('fetches a target before the failure that was evicted, and clamps one past it', async () => {
    const c = tamperedAt4();
    const { p, states } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    // Everything is evicted (as if the viewer had played on elsewhere).
    sb().ranges = [];
    c.reads.length = 0;
    await p.seek(2000);
    expect(env.video.currentTime).toBe(2);
    expect(c.reads[0]).toBe(2);
    expect(sb().appends.some((a, i) => i >= 4 && a[0] === 2)).toBe(true);
    await p.seek(9000);
    expect(env.video.currentTime).toBeLessThanOrEqual(4);
    expect(last(states).integrity).toBe('tampered');
    expect(last(states).failedAtMs).toBe(4000);
    p.destroy();
  });
});

describe('seek (ui-spec §4.3.2)', () => {
  it('inside the buffer only sets currentTime', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 10 }) });
    const { p } = make(c);
    await p.load(CLIP);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'all buffered');
    const reads = c.reads.length;
    await p.seek(6500);
    expect(env.video.currentTime).toBe(6.5);
    expect(c.reads.length).toBe(reads);
    expect(sb().aborts).toBe(0);
  });

  it('outside the buffer aborts the parser, fetches the indexed chunk and continues sequentially', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.reads.length = 0;
    const done = p.seek(60_000);
    await done;
    expect(env.video.currentTime).toBe(60);
    expect(sb().aborts).toBe(1);
    expect(c.reads[0]).toBe(60);
    await until(() => sb().appends.length > 31, 'continues sequentially');
    expect(sb().appends[30]![0]).toBe(60);
    expect(c.reads.slice(0, 3)).toEqual([60, 61, 62]);
    p.destroy();
  });

  it('a backward seek outside the buffer keeps its fragment and stops at ahead_seconds of the target', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile'); // ahead 30, behind 15
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    for (const t of [25, 50]) {
      env.video.currentTime = t;
      env.video.fire('timeupdate');
      await until(() => Math.max(...sb().ranges.map((r) => r[1])) >= t + 30, `buffered to ${t + 30}`);
    }
    await tick(50);
    // The playhead is at 50 s; the first 35 s have been evicted.
    expect(sb().ranges[0]![0]).toBeGreaterThanOrEqual(35);

    const removed = sb().removes.length;
    await p.seek(10_000);
    expect(env.video.currentTime).toBe(10);
    await until(() => sb().ranges.some(([s, e]) => s <= 10 && e >= 40), '30 s ahead of 10 s buffered');
    await tick(50);
    const n = sb().appends.length;
    await tick(100);
    expect(sb().appends).toHaveLength(n); // appends stopped
    // The fragment at the target was never evicted as "behind", and nothing runs past the window.
    expect(sb().removes.slice(removed).some(([s, e]) => s <= 10 && e > 10)).toBe(false);
    const holding = sb().ranges.find(([s, e]) => s <= 10 && e > 10)!;
    expect(holding[1]).toBeLessThanOrEqual(41);
    // Far fewer appends than fetching to the end of the clip.
    expect(sb().appends.length).toBeLessThan(120);
    p.destroy();
  });

  it('appends from the intra-chunk offset of the fragment', async () => {
    const n = 100;
    const open = openResult({ numChunks: n });
    open.seekIndex[60] = [60_000, 60 * CHUNK + 1000];
    const c = new FakeClient({ open });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    await p.seek(60_500);
    expect(sb().appends[30]!.length).toBe(CHUNK - 1000);
    p.destroy();
  });

  it('does not append the init segment again', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    await p.seek(70_000);
    // First append stays the only one that starts at chunk 0.
    expect(sb().appends.filter((a) => a[0] === 0)).toHaveLength(1);
    p.destroy();
  });

  it('a stale read that resolves after a seek is dropped', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    // The fill prefetches one chunk ahead: with the playhead at 5 s it reads 30 and holds 31.
    c.hold.add(31);
    env.video.currentTime = 5;
    env.video.fire('timeupdate');
    await until(() => c.held.has(31), 'read of chunk 31 in flight');
    await p.seek(80_000);
    c.held.get(31)!();
    await tick(50);
    expect(sb().appends.some((a) => a[0] === 31)).toBe(false);
    p.destroy();
  });
});

describe('native seek bar scrubbing', () => {
  /** Mobile profile, played to 50 s: buffered is [35, 80]. */
  async function playedTo50() {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p, states } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    for (const t of [25, 50]) {
      env.video.currentTime = t;
      env.video.fire('timeupdate');
      await until(() => Math.max(...sb().ranges.map((r) => r[1])) >= t + 30, `buffered to ${t + 30}`);
    }
    await tick(50);
    expect(sb().ranges[0]![0]).toBeGreaterThanOrEqual(35);
    return { c, p, states };
  }

  it('a backward scrub outside the buffer is routed through the seek planner', async () => {
    const { c, p } = await playedTo50();
    c.reads.length = 0;
    const aborts = sb().aborts;
    env.video.currentTime = 10; // the viewer drags the native seek bar
    env.video.fire('seeking');
    await until(() => sb().ranges.some(([s, e]) => s <= 10 && e >= 40), '30 s ahead of 10 s buffered');
    // The first read is the seek_index chunk for 10 s, not a continuation of the old position.
    expect(c.reads[0]).toBe(10);
    expect(sb().aborts).toBe(aborts + 1);
    expect(env.video.currentTime).toBe(10);
    await tick(50);
    const n = sb().appends.length;
    await tick(100);
    expect(sb().appends).toHaveLength(n); // appends stopped about 30 s ahead of 10 s
    const holding = sb().ranges.find(([s, e]) => s <= 10 && e > 10)!;
    expect(holding[1]).toBeLessThanOrEqual(41);
    expect(c.reads.every((r) => r < 50)).toBe(true); // never fetched towards the end of the clip
    p.destroy();
  });

  it('a forward scrub past the buffer fetches the indexed fragment instead of everything in between', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 300 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.reads.length = 0;
    env.video.currentTime = 150;
    env.video.fire('seeking');
    await until(() => sb().ranges.some(([s, e]) => s <= 150 && e >= 180), '30 s ahead of 150 s buffered');
    expect(c.reads[0]).toBe(150);
    expect(c.reads.every((r) => r >= 150)).toBe(true);
    p.destroy();
  });

  it('a scrub inside the buffer does nothing', async () => {
    const { c, p } = await playedTo50();
    c.reads.length = 0;
    const aborts = sb().aborts;
    env.video.currentTime = 60;
    env.video.fire('seeking');
    await tick(50);
    expect(sb().aborts).toBe(aborts);
    expect(env.video.currentTime).toBe(60);
    p.destroy();
  });

  it('the engine\'s own assignment after the fragment is buffered does not re-enter', async () => {
    const { c, p } = await playedTo50();
    await p.seek(10_000);
    await until(() => sb().ranges.some(([s, e]) => s <= 10 && e >= 40), '30 s ahead of 10 s buffered');
    await tick(100);
    c.reads.length = 0;
    const aborts = sb().aborts;
    env.video.fire('seeking'); // the browser reports the engine's own currentTime assignment
    await tick(50);
    expect(sb().aborts).toBe(aborts);
    expect(c.reads).toHaveLength(0);
    p.destroy();
  });

  it('a scrub past a tampered failure is clamped without looping', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 100 }),
      read: (i) => (i === 4 ? new ErrAuth(4) : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    sb().ranges = [];
    c.reads.length = 0;
    env.video.currentTime = 9;
    env.video.fire('seeking');
    await tick(50);
    expect(env.video.currentTime).toBe(4);
    env.video.fire('seeking'); // the clamp's own seek
    await tick(50);
    expect(env.video.currentTime).toBe(4);
    expect(c.reads).toHaveLength(0);
    p.destroy();
  });

  it('does not restart a fetch seek whose fragment is still arriving (the browser clamps to the buffer end)', async () => {
    const { c, p } = await playedTo50();
    await p.seek(10_000);
    await until(() => sb().ranges.some(([s, e]) => s <= 10 && e >= 40), '30 s ahead of 10 s buffered');
    await tick(100);
    // Mid-fragment: the playhead sits where nothing is buffered yet, in the fragment the seek already fetches.
    sb().ranges = [];
    c.reads.length = 0;
    const aborts = sb().aborts;
    env.video.currentTime = 10.5;
    env.video.fire('seeking');
    await tick(50);
    expect(sb().aborts).toBe(aborts);
    // Once the playhead has been inside the buffer again, the same time is a fresh scrub.
    sb().ranges = [[10, 20]];
    env.video.fire('timeupdate');
    sb().ranges = [];
    env.video.fire('seeking');
    await until(() => sb().aborts === aborts + 1, 'scrub planned');
    p.destroy();
  });

  it('a scrub before anything has been appended does not restart the first fetch', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    c.hold.add(0);
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => c.held.has(0), 'read of chunk 0 in flight');
    env.video.fire('seeking'); // currentTime 0, nothing buffered yet
    await tick(50);
    c.held.get(0)!();
    await until(() => sb().appends.length === 30, '30 chunks');
    expect(sb().aborts).toBe(0);
    p.destroy();
  });
});

describe('overlapping seeks', () => {
  it('a seek inside the buffer supersedes an earlier fetch seek that is still settling', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.hold.add(70);
    const a = p.seek(70_000); // unbuffered: waits for chunk 70
    await until(() => c.held.has(70), 'read of chunk 70 in flight');
    await p.seek(5000); // buffered
    expect(env.video.currentTime).toBe(5);
    c.held.get(70)!();
    await a;
    await tick(50);
    expect(env.video.currentTime).toBe(5);
    p.destroy();
  });

  it('a native scrub inside the buffer fully supersedes an earlier native scrub that is still settling', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile'); // buffered [0, 30]
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.hold.add(70);
    env.video.currentTime = 70; // first drag: unbuffered, starts a fetch seek
    env.video.fire('seeking');
    await until(() => c.held.has(70), 'read of chunk 70 in flight');
    env.video.currentTime = 5; // second drag: back inside the buffer
    env.video.fire('seeking');
    await tick(50);
    const appended = sb().appends.length;
    const reads = c.reads.length;
    c.held.get(70)!();
    await tick(50);
    await new Promise<void>((r) => setTimeout(r, 20));
    expect(env.video.currentTime).toBe(5);
    expect(sb().ranges.some(([s, e]) => s <= 0 && e >= 30)).toBe(true); // [0, 30] was not evicted
    expect(sb().appends.slice(appended).every((a) => (a[0] ?? 0) < 70)).toBe(true);
    expect(sb().appends.slice(0, appended).every((a) => (a[0] ?? 0) < 70)).toBe(true);
    expect(c.reads.slice(reads).every((r) => r < 70)).toBe(true);
    p.destroy();
  });

  it('a direct seek inside the buffer stops the earlier fetch seek\'s fill and resumes after the buffered range', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile'); // buffered [0, 30]
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.hold.add(70);
    const a = p.seek(70_000);
    await until(() => c.held.has(70), 'read of chunk 70 in flight');
    await p.seek(5000); // buffered
    const reads = c.reads.length;
    c.held.get(70)!();
    await a;
    await tick(50);
    expect(env.video.currentTime).toBe(5);
    env.video.currentTime = 29;
    env.video.fire('timeupdate');
    await until(() => sb().ranges.some(([s, e]) => s <= 29 && e >= 59), '30 s ahead of 29 s buffered, contiguous');
    await tick(50);
    await new Promise<void>((r) => setTimeout(r, 20));
    const holding = sb().ranges.find(([s, e]) => s <= 29 && e > 29)!;
    expect(holding[1]).toBeLessThanOrEqual(61); // stopped about 30 s ahead of the playhead
    expect(sb().ranges.every(([s]) => s < 70)).toBe(true);
    expect(c.reads.slice(reads).every((r) => r < 70)).toBe(true);
    expect(sb().appends.every((a) => (a[0] ?? 0) < 70)).toBe(true);
    p.destroy();
  });

  it('a later fetch seek supersedes an earlier one', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    c.hold.add(70);
    const a = p.seek(70_000);
    await until(() => c.held.has(70), 'read of chunk 70 in flight');
    await p.seek(90_000);
    expect(env.video.currentTime).toBe(90);
    c.held.get(70)!();
    await a;
    await tick(50);
    expect(env.video.currentTime).toBe(90);
    p.destroy();
  });
});

describe('destroy', () => {
  it('closes the clip, revokes the object URL and stops fetching', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 100 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => sb().appends.length === 30, '30 chunks');
    const url = env.video.src;
    p.destroy();
    expect(c.calls.some((m) => m.type === 'clip.close')).toBe(true);
    expect(env.revoked).toContain(url);
    expect(env.video.src).toBe('');
    const n = c.reads.length;
    env.video.fire('timeupdate');
    await tick(50);
    expect(c.reads.length).toBe(n);
  });

  it('loading another clip closes the first', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 3 }) });
    const { p } = make(c);
    await p.load(CLIP);
    await p.load('01M2NBMVS0K8EKRQKGGAJCDT0C');
    expect(c.calls.filter((m) => m.type === 'clip.close')).toHaveLength(1);
    expect(env.mediaSources()).toHaveLength(2);
  });
});

describe('blob fallback (ui-spec §4.3.3)', () => {
  beforeEach(() => {
    env.restore();
    env = installMedia({ mse: false, mms: false });
  });

  it('refuses a clip over the profile limit with "Too large to play"', async () => {
    const open = openResult({ numChunks: 3, plaintextSizeBytes: (48 << 20) + 1 });
    const { p, states } = make(new FakeClient({ open }), 'mobile');
    await p.load(CLIP);
    expect(last(states).message).toContain('Too large to play');
    expect(env.video.src).toBe('');
  });

  it('a desktop accepts the same clip', async () => {
    const open = openResult({ numChunks: 3, plaintextSizeBytes: (48 << 20) + 1 });
    const { p } = make(new FakeClient({ open }), 'desktop');
    await p.load(CLIP);
    await until(() => env.video.src !== '', 'src');
  });

  it('decrypts every chunk into a Blob without concatenating', async () => {
    const { p } = make(new FakeClient({ open: openResult({ numChunks: 3 }) }));
    await p.load(CLIP);
    await until(() => env.video.src !== '', 'src');
    const blob = env.registry.get(env.video.src) as Blob;
    expect(blob.type).toBe('video/mp4');
    expect(blob.size).toBe(3 * CHUNK);
  });

  it('keeps one Blob URL on mobile and revokes the previous one', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 2 }) });
    const { p } = make(c, 'mobile');
    await p.load(CLIP);
    await until(() => env.video.src !== '', 'first src');
    const first = env.video.src;
    await p.load('01M2NBMVS0K8EKRQKGGAJCDT0C');
    await until(() => env.video.src !== '' && env.video.src !== first, 'second src');
    expect(env.revoked).toContain(first);
    p.destroy();
  });

  it('a tampered chunk plays the verified part and flags tampered', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 6 }),
      read: (i) => (i === 3 ? new ErrAuth(3) : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    const blob = env.registry.get(env.video.src) as Blob;
    expect(blob.size).toBe(3 * CHUNK);
    expect(last(states).failedAtMs).toBe(3000);
  });

  it('reloading a cached clip that was tampered resolves tampered, not verified', async () => {
    const OTHER = '01M2NBMVS0K8EKRQKGGAJCDT0C';
    const c = new FakeClient({
      open: openResult({ numChunks: 6 }),
      read: (i) => (i === 3 ? new ErrAuth(3) : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c, 'desktop');
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    await p.load(OTHER);
    await until(() => last(states).clipId === OTHER && last(states).integrity === 'tampered', 'second load');
    c.reads.length = 0;
    expect(await p.load(CLIP)).toBe('tampered'); // served from the Blob LRU
    expect(last(states).failedAtMs).toBe(3000);
    expect(c.reads).toHaveLength(0);
  });

  it('reloading a cached incomplete clip resolves incomplete', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 6 }),
      read: (i) => (i === 3 ? new ErrTruncated('cut') : { plaintext: chunkBytes(i), final: false }),
    });
    const { p, states } = make(c, 'desktop');
    await p.load(CLIP);
    await until(() => last(states).integrity === 'incomplete', 'incomplete');
    await p.load('01M2NBMVS0K8EKRQKGGAJCDT0C');
    expect(await p.load(CLIP)).toBe('incomplete');
  });

  it('seek just sets currentTime', async () => {
    const { p } = make(new FakeClient({ open: openResult({ numChunks: 3 }) }));
    await p.load(CLIP);
    await until(() => env.video.src !== '', 'src');
    await p.seek(1500);
    expect(env.video.currentTime).toBe(1.5);
  });
});

describe('auto-advance', () => {
  const NEXT = '01M2NBMVS0K8EKRQKGGAJCDT0C';
  const OTHER = '01M2NBMVS0K8EKRQKGGAJCDT0D';
  const sc = (id: string, series: string, seg: number, start: string) => ({
    clip_id: id,
    series_id: series,
    segment_index: seg,
    camera: { id: 'front-door' },
    start_time: start,
  });

  it('on ended loads the next segment of the same series first', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 2 }),
      sidecar: sc(CLIP, 'S1', 0, '2026-11-06T23:59:00.000Z'),
      clips: [
        sc(OTHER, 'S2', 0, '2026-11-06T23:59:30.000Z'),
        sc(NEXT, 'S1', 1, '2026-11-07T00:00:10.000Z'),
      ],
    });
    const { p, states } = make(c);
    p.setAutoAdvance(true);
    await p.load(CLIP);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'stream end');
    env.video.fire('ended');
    await until(() => c.calls.filter((m) => m.type === 'clip.open').length === 2, 'second open');
    const opens = c.calls.filter((m) => m.type === 'clip.open');
    expect(opens[1]).toEqual({ type: 'clip.open', clipId: NEXT });
    const q = c.calls.find((m) => m.type === 'api.get' && m.path === '/api/v1/clips');
    expect(q).toMatchObject({ query: { camera: 'front-door' } });
    // The lookup spans the clip's UTC day and the next.
    expect((q as { query: Record<string, string> }).query.to).toBe('2026-11-08T00:00:00.000Z');
    await until(() => last(states).clipId === NEXT, 'state clip id');
  });

  it('falls back to the next clip of the camera by start time when the series is over', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 2 }),
      sidecar: sc(CLIP, 'S1', 0, '2026-11-06T10:00:00.000Z'),
      clips: [sc(CLIP, 'S1', 0, '2026-11-06T10:00:00.000Z'), sc(OTHER, 'S2', 0, '2026-11-06T10:05:00.000Z')],
    });
    const { p } = make(c);
    p.setAutoAdvance(true);
    await p.load(CLIP);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'stream end');
    env.video.fire('ended');
    await until(() => c.calls.filter((m) => m.type === 'clip.open').length === 2, 'second open');
    expect(c.calls.filter((m) => m.type === 'clip.open')[1]).toEqual({ type: 'clip.open', clipId: OTHER });
  });

  it('does nothing when autoplay-next is off', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 2 }), sidecar: sc(CLIP, 'S1', 0, '2026-11-06T10:00:00.000Z') });
    const { p } = make(c);
    await p.load(CLIP);
    await until(() => env.mediaSources()[0]!.readyState === 'ended', 'stream end');
    env.video.fire('ended');
    await tick(50);
    expect(c.calls.filter((m) => m.type === 'clip.open')).toHaveLength(1);
  });

  it('does not advance past a tampered clip', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 4 }),
      read: (i) => (i === 2 ? new ErrAuth(2) : { plaintext: chunkBytes(i), final: false }),
      sidecar: sc(CLIP, 'S1', 0, '2026-11-06T10:00:00.000Z'),
      clips: [sc(NEXT, 'S1', 1, '2026-11-06T10:01:00.000Z')],
    });
    const { p, states } = make(c);
    p.setAutoAdvance(true);
    await p.load(CLIP);
    await until(() => last(states).integrity === 'tampered', 'tampered');
    env.video.fire('ended');
    await tick(50);
    expect(c.calls.filter((m) => m.type === 'clip.open')).toHaveLength(1);
  });
});
