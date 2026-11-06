// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakePlayer, fakePlayerFactory, ids } from '../../../test-support/dashboard-fakes';
import type { PlayerState } from '../player/engine';
import { initialState } from '../player/status';
import EventList from './EventList.svelte';
import ExportDialog from './ExportDialog.svelte';
import IntegrityBadge from './IntegrityBadge.svelte';
import Player from './Player.svelte';

// jsdom has no ResizeObserver; `bind:clientHeight` needs one.
class FakeResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

beforeEach(() => {
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
  FakePlayer.instances = [];
  FakePlayer.nextIntegrity = 'verified';
  HTMLMediaElement.prototype.play = () => Promise.resolve();
  HTMLMediaElement.prototype.pause = () => {};
});
afterEach(cleanup);

const row = (i: number) => ({
  id: `id${i}`,
  cameraName: 'Front Door',
  startMs: Date.UTC(2026, 10, 6, 0, 0, 0) + i * 60_000,
  durationMs: 65_000 as number | null,
  triggers: ['person', 'vehicle'],
  status: 'complete' as const,
});

describe('EventList', () => {
  const many = Array.from({ length: 5000 }, (_, i) => row(5000 - i)); // newest first

  it('renders only a window of a very long list', () => {
    render(EventList, { clips: many, selectedId: null, zone: 'Asia/Jakarta', integrity: {}, onSelect: () => {} });
    const rows = screen.getAllByRole('option');
    expect(rows.length).toBeGreaterThan(0);
    expect(rows.length).toBeLessThan(60);
    expect(rows[0]).toHaveAttribute('aria-setsize', '5000');
  });

  it('renders further rows after scrolling, newest first', async () => {
    render(EventList, { clips: many, selectedId: null, zone: 'Asia/Jakarta', integrity: {}, onSelect: () => {} });
    const list = screen.getByRole('listbox');
    expect(within(list).getAllByRole('option')[0]).toHaveAttribute('data-clip', 'id5000');
    list.scrollTop = 56 * 2000;
    await fireEvent.scroll(list);
    const first = within(list).getAllByRole('option')[0]!;
    expect(Number(first.getAttribute('aria-posinset'))).toBeGreaterThan(1900);
    // The window starts a few rows above the viewport; rows stay in newest-first order.
    expect(first.dataset.clip).toBe(`id${5000 - (Number(first.getAttribute('aria-posinset')) - 1)}`);
  });

  it('shows local time in the zone, camera, trigger glyphs, duration and the integrity badge', () => {
    render(EventList, {
      clips: [row(0)],
      selectedId: null,
      zone: 'Asia/Jakarta',
      integrity: { id0: 'verified' },
      onSelect: () => {},
    });
    const r = screen.getByRole('option');
    // 2026-11-06T00:00Z is 07:00 in Jakarta.
    expect(r).toHaveTextContent('07:00:00');
    expect(r).toHaveTextContent('Front Door');
    expect(r).toHaveTextContent('1:05');
    expect(within(r).getByLabelText('Person')).toBeInTheDocument();
    expect(within(r).getByLabelText('Vehicle')).toBeInTheDocument();
    expect(within(r).getByLabelText(/verified/i)).toBeInTheDocument();
  });

  it('marks the selection and calls onSelect on click', async () => {
    const onSelect = vi.fn();
    render(EventList, { clips: [row(1), row(0)], selectedId: 'id0', zone: 'UTC', integrity: {}, onSelect });
    const [a, b] = screen.getAllByRole('option');
    expect(a).toHaveAttribute('aria-selected', 'false');
    expect(b).toHaveAttribute('aria-selected', 'true');
    await fireEvent.click(a!);
    expect(onSelect).toHaveBeenCalledWith('id1');
  });

  it('scrolls a selected clip that is far outside the window into view', async () => {
    const { rerender } = render(EventList, { clips: many, selectedId: null, zone: 'UTC', integrity: {}, onSelect: () => {} });
    await rerender({ clips: many, selectedId: 'id10', zone: 'UTC', integrity: {}, onSelect: () => {} });
    await waitFor(() => expect(screen.getByRole('option', { selected: true })).toBeInTheDocument());
  });

  it('says so when there are no recordings', () => {
    render(EventList, { clips: [], selectedId: null, zone: 'UTC', integrity: {}, onSelect: () => {} });
    expect(screen.getByText(/no recordings/i)).toBeInTheDocument();
  });
});

describe('IntegrityBadge', () => {
  it.each([
    ['verified', /verified/i],
    ['mismatch', /mismatch/i],
    ['tampered', /failed verification|tampered/i],
    ['incomplete', /interrupted|incomplete/i],
    ['unknown-key', /key/i],
  ] as const)('labels %s', (state, label) => {
    render(IntegrityBadge, { state });
    expect(screen.getByLabelText(label)).toBeInTheDocument();
  });
  it('renders nothing while unknown', () => {
    const { container } = render(IntegrityBadge, { state: undefined });
    expect(container.textContent).toBe('');
  });
});

describe('ExportDialog', () => {
  it('states that the copy is unencrypted, with the word emphasised', () => {
    render(ExportDialog, { filename: 'a.mp4', busy: false, bytes: 0, error: '', onConfirm: () => {}, onCancel: () => {} });
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent('This saves an unencrypted copy on this device.');
    expect(dialog.querySelector('strong')?.textContent).toBe('unencrypted');
    expect(dialog).toHaveTextContent('a.mp4');
  });
  it('confirms and cancels', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(ExportDialog, { filename: 'a.mp4', busy: false, bytes: 0, error: '', onConfirm, onCancel });
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(onCancel).toHaveBeenCalledOnce();
  });
  it('cancels on Escape', async () => {
    const onCancel = vi.fn();
    render(ExportDialog, { filename: 'a.mp4', busy: false, bytes: 0, error: '', onConfirm: () => {}, onCancel });
    await fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledOnce();
  });
  it('disables Save while busy and shows the error', () => {
    render(ExportDialog, { filename: 'a.mp4', busy: true, bytes: 1_500_000, error: 'boom', onConfirm: () => {}, onCancel: () => {} });
    expect(screen.getByRole('button', { name: /sav/i })).toBeDisabled();
    expect(screen.getByRole('alert')).toHaveTextContent('boom');
    expect(screen.getByRole('dialog')).toHaveTextContent('1.4 MiB');
  });
});

describe('Player', () => {
  const props = (over: Record<string, unknown> = {}) => ({
    client: { call: async () => ({}) } as never,
    cue: { clipId: ids.a, offsetMs: 0, n: 1 },
    factory: fakePlayerFactory,
    zone: 'Asia/Jakarta',
    profile: 'desktop' as const,
    ...over,
  });

  it('loads the target clip', async () => {
    render(Player, props());
    await waitFor(() => expect(FakePlayer.instances[0]?.loads).toEqual([ids.a]));
  });

  it('seeks to the offset after loading', async () => {
    render(Player, props({ cue: { clipId: ids.a, offsetMs: 12_000, n: 1 } }));
    await waitFor(() => expect(FakePlayer.instances[0]?.seeks).toEqual([12_000]));
  });

  it('seeks within the loaded clip instead of reloading it', async () => {
    const { rerender } = render(Player, props());
    await waitFor(() => expect(FakePlayer.instances[0]?.loads).toHaveLength(1));
    await rerender(props({ cue: { clipId: ids.a, offsetMs: 30_000, n: 2 } }));
    await waitFor(() => expect(FakePlayer.instances[0]?.seeks).toEqual([30_000]));
    expect(FakePlayer.instances[0]?.loads).toHaveLength(1);
  });

  it('enables export for a verified clip', async () => {
    render(Player, props());
    await waitFor(() => expect(screen.getByRole('button', { name: 'Export' })).toBeEnabled());
  });

  it('disables export for a tampered clip', async () => {
    FakePlayer.nextIntegrity = 'tampered';
    render(Player, props());
    await waitFor(() => expect(FakePlayer.instances[0]?.loads).toHaveLength(1));
    const btn = screen.getByRole('button', { name: 'Export' });
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute('title', expect.stringMatching(/failed verification|cannot be exported/i));
  });

  it('keeps export disabled while verifying', () => {
    render(Player, props({ cue: null }));
    expect(screen.getByRole('button', { name: 'Export' })).toBeDisabled();
  });

  it('shows the confirmation before exporting', async () => {
    render(Player, props());
    const btn = await screen.findByRole('button', { name: 'Export' });
    await waitFor(() => expect(btn).toBeEnabled());
    await fireEvent.click(btn);
    expect(screen.getByRole('dialog')).toHaveTextContent('This saves an unencrypted copy on this device.');
  });

  it('proposes the filename <camera>_<local time>.mp4 from the verified header', async () => {
    render(Player, props());
    const btn = await screen.findByRole('button', { name: 'Export' });
    await waitFor(() => expect(btn).toBeEnabled());
    await fireEvent.click(btn);
    // Header start 2026-11-06T14:59:05Z, zone Asia/Jakarta.
    expect(screen.getByRole('dialog')).toHaveTextContent('front-door_20261106-215905.mp4');
  });

  it('adds -incomplete for an incomplete clip', async () => {
    FakePlayer.nextIntegrity = 'incomplete';
    render(Player, props());
    const btn = await screen.findByRole('button', { name: 'Export' });
    await waitFor(() => expect(btn).toBeEnabled());
    await fireEvent.click(btn);
    expect(screen.getByRole('dialog')).toHaveTextContent('front-door_20261106-215905-incomplete.mp4');
  });

  it('shows the integrity text from the player state', async () => {
    render(Player, props());
    await waitFor(() => expect(FakePlayer.instances).toHaveLength(1));
    const s: PlayerState = { ...initialState(), clipId: ids.a, integrity: 'tampered', failedAtMs: 83_000 };
    FakePlayer.instances[0]!.emit(s);
    expect(await screen.findByRole('alert')).toHaveTextContent('failed verification at ~01:23');
  });

  it('exports through the confirmation, then restores the playing clip', async () => {
    const written: number[] = [];
    const calls: string[] = [];
    const client = {
      call: async (m: { type: string; chunk?: number }) => {
        calls.push(m.type);
        if (m.type === 'clip.open') return { numChunks: 2, plaintextSizeBytes: 8 };
        if (m.type === 'clip.read') return { plaintext: new Uint8Array(4).fill((m.chunk ?? 0) + 1).buffer, final: m.chunk === 1 };
        return {};
      },
    };
    const env = {
      isIOS: false,
      showSaveFilePicker: async () => ({
        createWritable: async () => ({
          write: async (b: Uint8Array) => void written.push(b[0]!),
          close: async () => {},
          abort: async () => {},
        }),
      }),
      download: () => {},
    };
    render(Player, props({ client, env }));
    const btn = await screen.findByRole('button', { name: 'Export' });
    await waitFor(() => expect(btn).toBeEnabled());
    await fireEvent.click(btn);
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(written).toEqual([1, 2]);
    // The export replaced the worker's session for this clip, so the player opens it again.
    await waitFor(() => expect(FakePlayer.instances[0]!.loads).toEqual([ids.a, ids.a]));
  });

  it('keeps the dialog open with the reason when the export fails', async () => {
    const client = {
      call: async (m: { type: string }) => {
        if (m.type === 'clip.open') throw new Error('network down');
        return {};
      },
    };
    const env = { isIOS: false, download: () => {} };
    render(Player, props({ client, env }));
    const btn = await screen.findByRole('button', { name: 'Export' });
    await waitFor(() => expect(btn).toBeEnabled());
    await fireEvent.click(btn);
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('dialog')).toHaveTextContent(/network down/);
  });

  it('destroys the engine on unmount', async () => {
    const { unmount } = render(Player, props());
    await waitFor(() => expect(FakePlayer.instances).toHaveLength(1));
    unmount();
    expect(FakePlayer.instances[0]!.destroyed).toBe(true);
  });
});
