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
