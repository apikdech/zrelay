// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { untrack } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DashClient, FakePlayer, fakePlayerFactory, ids, sidecar } from '../../test-support/dashboard-fakes';
import type { Request } from '../worker/protocol';
import { drawTimeline } from '../lib/timeline/render';
import { localDayRange } from '../lib/timeline/timezone';
import Page from './+page.svelte';

// The viewer is in Berlin: any other zone must be chosen by hand.
vi.mock('../lib/timeline/timezone', async (orig) => ({
  ...(await orig<typeof import('../lib/timeline/timezone')>()),
  browserZone: () => 'Europe/Berlin',
}));

// Draw synchronously and record the layout, so the zoom the shortcuts reach
// can be read off the last frame. (untrack: the real scheduler draws outside effects.)
vi.mock('../lib/timeline/render', async (orig) => ({
  ...(await orig<typeof import('../lib/timeline/render')>()),
  createScheduler: (draw: () => void) => ({ invalidate: () => untrack(draw), dispose() {} }),
  drawTimeline: vi.fn(),
}));

// 2026-11-06 15:00 UTC, which is 22:00 on the 6th in Jakarta.
const NOW = Date.UTC(2026, 10, 6, 15, 0, 0);
const iso = (ms: number) => new Date(ms).toISOString().replace('.000Z', 'Z');

class FakeResizeObserver {
  observe() {}
  disconnect() {}
  unobserve() {}
}

beforeEach(() => {
  FakePlayer.instances = [];
  FakePlayer.nextIntegrity = 'verified';
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
  HTMLCanvasElement.prototype.getContext = (() => ({ setTransform() {} })) as never;
  Object.defineProperty(HTMLCanvasElement.prototype, 'clientWidth', { get: () => 1000, configurable: true });
  vi.mocked(drawTimeline).mockClear();
  HTMLMediaElement.prototype.play = () => Promise.resolve();
  HTMLMediaElement.prototype.pause = () => {};
});
afterEach(() => {
  cleanup();
  localStorage.clear();
});

function status(ageMs: number) {
  return {
    schema: 'zrec.status/1',
    relay_id: 'r',
    version: 'v',
    updated_at: iso(NOW - ageMs),
    key_id: '0123456789abcdef',
    cameras: [{ id: 'front-door', name: 'Front Door', rtsp: 'up', events: 'up', since: iso(NOW - 3_600_000), last_clip_at: null }],
  };
}

function client(): DashClient {
  const c = new DashClient();
  c.status = status(30_000);
  c.clips = [
    sidecar(ids.a, '2026-11-06T10:00:00Z'),
    sidecar(ids.b, '2026-11-06T11:00:00Z'),
    sidecar(ids.c, '2026-11-06T12:00:00Z', { triggers: [{ type: 'vehicle', first_at_ms: 0, last_at_ms: 5000 }] }),
  ];
  return c;
}

const mountPage = (c: DashClient) =>
  render(Page, { client: c, factory: fakePlayerFactory, zone: 'Asia/Jakarta', nowMs: NOW, profile: 'desktop' });

// The camera and trigger <select>s have <option>s too: look only inside the recordings list.
const list = () => within(screen.getByRole('listbox', { name: /recordings/i }));
const listRows = async () => within(await screen.findByRole('listbox', { name: /recordings/i })).findAllByRole('option');
const selected = () => list().queryByRole('option', { selected: true })?.getAttribute('data-clip') ?? null;
const getsFor = (c: DashClient, path: string) =>
  c.calls.filter((m): m is Extract<Request, { type: 'api.get' }> => m.type === 'api.get' && m.path === path);

describe('timeline dashboard', () => {
  it('lists the day newest first, in local time', async () => {
    mountPage(client());
    const rows = await listRows();
    expect(rows.map((r) => r.getAttribute('data-clip'))).toEqual([ids.c, ids.b, ids.a]);
    // 12:00Z is 19:00 in Jakarta.
    expect(rows[0]).toHaveTextContent('19:00:00');
  });

  it('asks for the Jakarta day in UTC', async () => {
    const c = client();
    mountPage(c);
    await listRows();
    expect(getsFor(c, '/api/v1/clips')[0]!.query).toMatchObject({
      from: '2026-11-05T17:00:00.000Z',
      to: '2026-11-06T17:00:00.000Z',
    });
  });

  it('K selects the next clip and J the previous one', async () => {
    mountPage(client());
    await listRows();
    expect(selected()).toBeNull();
    await fireEvent.keyDown(window, { key: 'k' }); // nothing selected: the newest
    expect(selected()).toBe(ids.c);
    await fireEvent.keyDown(window, { key: 'J' });
    expect(selected()).toBe(ids.b);
    await fireEvent.keyDown(window, { key: 'j' });
    expect(selected()).toBe(ids.a);
    await fireEvent.keyDown(window, { key: 'j' });
    expect(selected()).toBe(ids.a);
    await fireEvent.keyDown(window, { key: 'K' });
    expect(selected()).toBe(ids.b);
    await waitFor(() => expect(FakePlayer.instances[0]?.loads.at(-1)).toBe(ids.b));
  });

  it('ignores the shortcuts while typing in a field', async () => {
    mountPage(client());
    await listRows();
    const date = screen.getByLabelText('Date');
    await fireEvent.keyDown(date, { key: 'k' });
    expect(selected()).toBeNull();
  });

  it('selecting a row loads that clip', async () => {
    mountPage(client());
    const rows = await listRows();
    await fireEvent.click(rows[1]!);
    expect(selected()).toBe(ids.b);
    await waitFor(() => expect(FakePlayer.instances[0]?.loads).toEqual([ids.b]));
  });

  it('shows a red relay dot when updated_at is 4 minutes old', async () => {
    const c = client();
    c.status = status(4 * 60_000);
    mountPage(c);
    await waitFor(() => expect(screen.getByRole('img', { name: /relay/i }).dataset.level).toBe('bad'));
  });

  it('shows a green relay dot for a fresh status', async () => {
    mountPage(client());
    await waitFor(() => expect(screen.getByRole('img', { name: /relay/i }).dataset.level).toBe('ok'));
  });

  it('shows an unknown dot, not a green one, when the status cannot be read', async () => {
    const c = client();
    c.failStatus = true;
    mountPage(c);
    await listRows();
    await waitFor(() => expect(screen.getByRole('img', { name: /relay/i }).dataset.level).toBe('unknown'));
  });

  it('re-queries with the camera and trigger filters', async () => {
    const c = client();
    c.cameras = [
      { id: 'front-door', name: 'Front Door' },
      { id: 'yard', name: 'Yard' },
    ];
    mountPage(c);
    await listRows();
    await fireEvent.change(screen.getByLabelText('Camera'), { target: { value: 'yard' } });
    await fireEvent.change(screen.getByLabelText('Trigger'), { target: { value: 'person' } });
    await waitFor(() => {
      const last = getsFor(c, '/api/v1/clips').at(-1)!;
      expect(last.query).toMatchObject({ camera: 'yard', trigger: 'person' });
    });
  });

  it('re-queries for another day', async () => {
    const c = client();
    mountPage(c);
    await listRows();
    await fireEvent.change(screen.getByLabelText('Date'), { target: { value: '2026-11-05' } });
    await waitFor(() => {
      expect(getsFor(c, '/api/v1/clips').at(-1)!.query).toMatchObject({
        from: '2026-11-04T17:00:00.000Z',
        to: '2026-11-05T17:00:00.000Z',
      });
    });
  });

  it('says when the key is missing', async () => {
    const c = client();
    c.call = async () => {
      throw Object.assign(new Error('no key loaded'), { name: 'ErrNoKey' });
    };
    mountPage(c);
    expect(await screen.findByText(/import your key/i)).toBeInTheDocument();
  });

  it('says how many recordings could not be read', async () => {
    const c = client();
    c.invalid = 2;
    mountPage(c);
    expect(await screen.findByText(/2 recordings could not be read/i)).toBeInTheDocument();
  });

  it('marks an incomplete clip in the list before it is opened', async () => {
    const c = client();
    c.clips = [sidecar(ids.a, '2026-11-06T10:00:00Z', { status: 'incomplete', end_time: null, duration_ms: null })];
    mountPage(c);
    const row = (await listRows())[0]!;
    expect(row.querySelector('[data-integrity="incomplete"]')).not.toBeNull();
  });

  it('records the integrity the player reports in the list', async () => {
    FakePlayer.nextIntegrity = 'tampered';
    mountPage(client());
    const rows = await listRows();
    await fireEvent.click(rows[0]!);
    await waitFor(() => expect(rows[0]!.querySelector('[data-integrity="tampered"]')).not.toBeNull());
  });

  it('locks the key from the menu', async () => {
    const c = client();
    mountPage(c);
    await listRows();
    await fireEvent.click(screen.getByRole('button', { name: 'Lock' }));
    expect(c.calls.some((m) => m.type === 'key.lock')).toBe(true);
  });
});

// ---- shortcuts and transport reach the player and the timeline -----------------

const HOUR = 3_600_000;
const MIN = 60_000;
const [FROM, TO] = localDayRange('2026-11-06', 'Asia/Jakarta');

describe('shortcuts and transport', () => {
  const video = () => screen.getByTestId('player') as HTMLVideoElement;
  const view = () => vi.mocked(drawTimeline).mock.calls.at(-1)![1].view;
  const canvas = () => screen.getByRole('application');
  const keys = (key: string, times = 1) => {
    for (let i = 0; i < times; i++) fireEvent.keyDown(window, { key });
  };

  /** Controls what the page sees of the video: its play state and clock. */
  function stubVideo(over: { paused?: boolean; currentTimeSec?: number } = {}) {
    const v = video();
    let paused = over.paused ?? true;
    Object.defineProperty(v, 'paused', { get: () => paused, configurable: true });
    Object.defineProperty(v, 'currentTime', { get: () => over.currentTimeSec ?? 0, configurable: true });
    const play = vi.fn(() => {
      paused = false;
      return Promise.resolve();
    });
    const pause = vi.fn(() => {
      paused = true;
    });
    v.play = play;
    v.pause = pause;
    return { play, pause };
  }

  async function ready() {
    const c = client();
    mountPage(c);
    await listRows();
    await waitFor(() => expect(FakePlayer.instances).toHaveLength(1));
    return c;
  }

  describe('keyboard', () => {
    it('Space plays a paused video and pauses a playing one', async () => {
      await ready();
      const { play, pause } = stubVideo({ paused: true });
      keys(' ');
      expect(play).toHaveBeenCalledTimes(1);
      keys(' ');
      expect(pause).toHaveBeenCalledTimes(1);
    });

    it('ArrowRight and ArrowLeft seek the player 10 s from the playhead', async () => {
      await ready();
      stubVideo({ currentTimeSec: 30 });
      const engine = FakePlayer.instances[0]!;
      keys('ArrowRight');
      keys('ArrowLeft');
      expect(engine.seeks).toEqual([40_000, 20_000]);
    });

    it('ArrowLeft stops at the start of the clip', async () => {
      await ready();
      stubVideo({ currentTimeSec: 3 });
      keys('ArrowLeft');
      expect(FakePlayer.instances[0]!.seeks).toEqual([0]);
    });

    it('] zooms the timeline in and [ zooms it out', async () => {
      await ready();
      expect(view().spanMs).toBe(TO - FROM);
      keys(']');
      expect(view().spanMs).toBe(6 * HOUR);
      keys(']');
      expect(view().spanMs).toBe(HOUR);
      keys('[');
      expect(view().spanMs).toBe(6 * HOUR);
      keys('[', 2);
      expect(view().spanMs).toBe(TO - FROM);
    });

    it('T centres the timeline on now', async () => {
      await ready();
      keys(']', 3); // 10 min around now
      // The focused timeline pans on ArrowLeft by itself, and the page leaves that key to it.
      for (let i = 0; i < 30; i++) fireEvent.keyDown(canvas(), { key: 'ArrowLeft' });
      expect(NOW).toBeGreaterThan(view().startMs + view().spanMs);
      keys('t');
      expect(view().spanMs).toBe(10 * MIN);
      expect(view().startMs + view().spanMs / 2).toBe(NOW);
      expect(FakePlayer.instances[0]!.seeks).toEqual([]); // the arrows moved the timeline, not the player
    });

    it('T on another day switches to today, reloads it and shows it', async () => {
      const c = await ready();
      await fireEvent.change(screen.getByLabelText('Date'), { target: { value: '2026-11-05' } });
      await waitFor(() => expect(view().startMs).toBe(FROM - 24 * HOUR));
      keys(']');
      expect(view().spanMs).toBe(6 * HOUR);
      keys('T');
      await waitFor(() => expect(screen.getByLabelText('Date')).toHaveValue('2026-11-06'));
      await waitFor(() => expect(view().startMs).toBe(FROM));
      expect(getsFor(c, '/api/v1/clips').at(-1)!.query).toMatchObject({
        from: '2026-11-05T17:00:00.000Z',
        to: '2026-11-06T17:00:00.000Z',
      });
      // Centring on now within the freshly shown whole day leaves the whole day in view.
      await waitFor(() => expect(view()).toEqual({ startMs: FROM, spanMs: TO - FROM }));
    });

    it('highlights the selected clip in the timeline', async () => {
      await ready();
      keys('k');
      await waitFor(() => expect(vi.mocked(drawTimeline).mock.calls.at(-1)![2].selectedClipId).toBe(ids.c));
    });
  });

  describe('transport buttons', () => {
    const press = (name: string) => fireEvent.click(screen.getByRole('button', { name }));

    it('Play/Pause toggles playback', async () => {
      await ready();
      const { play, pause } = stubVideo({ paused: true });
      await press('Play/Pause');
      expect(play).toHaveBeenCalledTimes(1);
      await press('Play/Pause');
      expect(pause).toHaveBeenCalledTimes(1);
    });

    it('-10 s and +10 s seek the player', async () => {
      await ready();
      stubVideo({ currentTimeSec: 30 });
      await press('+10 s');
      await press('-10 s');
      expect(FakePlayer.instances[0]!.seeks).toEqual([40_000, 20_000]);
    });

    it('Prev and Next select the neighbouring clips', async () => {
      await ready();
      await press('Next');
      expect(selected()).toBe(ids.c);
      await press('Prev');
      expect(selected()).toBe(ids.b);
    });

    it('Zoom in and Zoom out step the timeline', async () => {
      await ready();
      await press('Zoom in');
      expect(view().spanMs).toBe(6 * HOUR);
      await press('Zoom in');
      expect(view().spanMs).toBe(HOUR);
      await press('Zoom out');
      expect(view().spanMs).toBe(6 * HOUR);
    });

    it('Now centres the timeline on now', async () => {
      await ready();
      await press('Zoom in');
      await press('Zoom in');
      await press('Zoom in');
      for (let i = 0; i < 30; i++) fireEvent.keyDown(canvas(), { key: 'ArrowRight' });
      await press('Now');
      expect(view().startMs + view().spanMs / 2).toBe(NOW);
    });

    it('Now on another day returns to today', async () => {
      await ready();
      await fireEvent.change(screen.getByLabelText('Date'), { target: { value: '2026-11-04' } });
      await waitFor(() => expect(view().startMs).toBe(FROM - 2 * 24 * HOUR));
      await press('Now');
      await waitFor(() => expect(screen.getByLabelText('Date')).toHaveValue('2026-11-06'));
      await waitFor(() => expect(view().startMs).toBe(FROM));
    });
  });

  describe('while the export confirmation is open', () => {
    async function openExport() {
      await ready();
      const rows = await listRows();
      await fireEvent.click(rows[1]!); // b
      const btn = await screen.findByRole('button', { name: 'Export' });
      await waitFor(() => expect(btn).toBeEnabled());
      const vid = stubVideo({ paused: true });
      await fireEvent.click(btn);
      expect(screen.getByRole('dialog')).toBeInTheDocument();
      return vid;
    }

    it('J and K leave the selection and the loaded clip alone', async () => {
      await openExport();
      const dialog = screen.getByRole('dialog');
      await fireEvent.keyDown(dialog, { key: 'k' });
      await fireEvent.keyDown(dialog, { key: 'J' });
      await fireEvent.keyDown(window, { key: 'k' });
      expect(selected()).toBe(ids.b);
      expect(FakePlayer.instances[0]!.loads).toEqual([ids.b]);
    });

    it('Space and the arrows do not control playback behind the dialog', async () => {
      const { play } = await openExport();
      const engine = FakePlayer.instances[0]!;
      const dialog = screen.getByRole('dialog');
      for (const key of [' ', 'ArrowLeft', 'ArrowRight']) await fireEvent.keyDown(dialog, { key });
      await fireEvent.keyDown(window, { key: ' ' });
      expect(play).not.toHaveBeenCalled();
      expect(engine.seeks).toEqual([]);
    });

    it('the zoom and now shortcuts are ignored too', async () => {
      await openExport();
      const before = view().spanMs;
      keys(']');
      keys('t');
      expect(view().spanMs).toBe(before);
    });

    it('works again once the dialog is closed', async () => {
      await openExport();
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).toBeNull();
      keys('j');
      expect(selected()).toBe(ids.a);
    });
  });
});

describe('forced display zone', () => {
  // 20:00 UTC: 21:00 on the 6th in Berlin, but already 03:00 on the 7th at home in Jakarta.
  const EVENING = Date.UTC(2026, 10, 6, 20, 0, 0);
  const mountAbroad = (c: DashClient) => render(Page, { client: c, factory: fakePlayerFactory, nowMs: EVENING, profile: 'desktop' });
  const firstRange = (c: DashClient) => getsFor(c, '/api/v1/clips')[0]!.query;
  const lastRange = (c: DashClient) => getsFor(c, '/api/v1/clips').at(-1)!.query;
  const dayRange = (day: string, zone: string) => {
    const [from, to] = localDayRange(day, zone);
    return { from: new Date(from).toISOString(), to: new Date(to).toISOString() };
  };

  it("follows the viewer's own zone until one is forced", async () => {
    const c = client();
    mountAbroad(c);
    await listRows();
    expect(firstRange(c)).toMatchObject(dayRange('2026-11-06', 'Europe/Berlin'));
    expect(screen.getByLabelText('Time zone')).toHaveValue('');
  });

  it("lets a relative abroad see the house's day in Asia/Jakarta", async () => {
    const c = client();
    mountAbroad(c);
    await listRows();
    await fireEvent.change(screen.getByLabelText('Time zone'), { target: { value: 'Asia/Jakarta' } });
    // "Today" is now Jakarta's today (the 7th) and the range is that Jakarta day.
    await waitFor(() => expect(lastRange(c)).toMatchObject(dayRange('2026-11-07', 'Asia/Jakarta')));
    expect(screen.getByLabelText('Date')).toHaveValue('2026-11-07');
    // The evening of the 6th at home reads 19:00 for a 12:00Z clip, not 13:00 as in Berlin.
    await fireEvent.change(screen.getByLabelText('Date'), { target: { value: '2026-11-06' } });
    await waitFor(() => expect(lastRange(c)).toMatchObject(dayRange('2026-11-06', 'Asia/Jakarta')));
    const rows = await listRows();
    expect(rows[0]).toHaveTextContent('19:00:00');
  });

  it('remembers the choice for the next visit', async () => {
    const { unmount } = mountAbroad(client());
    await listRows();
    await fireEvent.change(screen.getByLabelText('Time zone'), { target: { value: 'Asia/Jakarta' } });
    unmount();
    const c = client();
    mountAbroad(c);
    await listRows();
    expect(firstRange(c)).toMatchObject(dayRange('2026-11-07', 'Asia/Jakarta'));
    expect(screen.getByLabelText('Time zone')).toHaveValue('Asia/Jakarta');
  });

  it("goes back to the viewer's zone when the choice is cleared", async () => {
    localStorage.setItem('zview.zone', 'Asia/Jakarta');
    const c = client();
    mountAbroad(c);
    await listRows();
    await fireEvent.change(screen.getByLabelText('Time zone'), { target: { value: '' } });
    await waitFor(() => expect(lastRange(c)).toMatchObject(dayRange('2026-11-06', 'Europe/Berlin')));
    expect(localStorage.getItem('zview.zone')).toBeNull();
  });

  it('ignores a remembered zone that is not valid', async () => {
    localStorage.setItem('zview.zone', 'Mars/Olympus');
    const c = client();
    mountAbroad(c);
    await listRows();
    expect(firstRange(c)).toMatchObject(dayRange('2026-11-06', 'Europe/Berlin'));
  });
});
