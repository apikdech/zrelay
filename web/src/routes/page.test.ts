// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DashClient, FakePlayer, fakePlayerFactory, ids, sidecar } from '../../test-support/dashboard-fakes';
import type { Request } from '../worker/protocol';
import Page from './+page.svelte';

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
  HTMLCanvasElement.prototype.getContext = (() => null) as never;
  HTMLMediaElement.prototype.play = () => Promise.resolve();
  HTMLMediaElement.prototype.pause = () => {};
});
afterEach(cleanup);

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
