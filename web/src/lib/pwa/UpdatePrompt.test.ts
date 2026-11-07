// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Updater } from './update';
import UpdatePrompt from './UpdatePrompt.svelte';

afterEach(() => {
  cleanup();
  document.body.replaceChildren();
});

function fakeUpdater(available: boolean) {
  const subs = new Set<() => void>();
  let avail = available;
  const apply = vi.fn(() => true);
  const u: Updater = {
    get available() {
      return avail;
    },
    onChange: (cb) => {
      subs.add(cb);
      return () => subs.delete(cb);
    },
    apply,
  };
  return {
    u,
    apply,
    set(a: boolean) {
      avail = a;
      subs.forEach((cb) => cb());
    },
  };
}

/** A <video> in the document that reports itself as playing or not. */
function video(playing: boolean, kind: 'clip' | 'live' = 'clip'): HTMLVideoElement {
  const v = document.createElement('video');
  // Player.svelte marks its <video> as clip playback; live tiles carry no mark.
  if (kind === 'clip') v.setAttribute('data-clip-player', '');
  Object.defineProperty(v, 'paused', { value: !playing, configurable: true });
  Object.defineProperty(v, 'ended', { value: false, configurable: true });
  document.body.append(v);
  return v;
}

describe('UpdatePrompt', () => {
  it('shows nothing without an update', () => {
    render(UpdatePrompt, { updater: fakeUpdater(false).u });
    expect(screen.queryByText(/New version available/)).toBeNull();
  });

  it('shows nothing without an updater (development, or no service worker support)', () => {
    render(UpdatePrompt, { updater: null });
    expect(screen.queryByText(/New version available/)).toBeNull();
  });

  it('offers "New version available — reload" and applies it on click', async () => {
    const f = fakeUpdater(false);
    render(UpdatePrompt, { updater: f.u });
    f.set(true);
    expect(await screen.findByText('New version available — reload')).toBeInTheDocument();
    await fireEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(f.apply).toHaveBeenCalledTimes(1);
  });

  it('holds the button while a clip plays and releases it on pause', async () => {
    const f = fakeUpdater(true);
    const v = video(true);
    render(UpdatePrompt, { updater: f.u });
    const btn = screen.getByRole('button', { name: 'Reload' });
    expect(btn).toBeDisabled();
    expect(screen.getByText(/Pause the video to update/)).toBeInTheDocument();
    Object.defineProperty(v, 'paused', { value: true, configurable: true });
    v.dispatchEvent(new Event('pause'));
    await vi.waitFor(() => expect(btn).toBeEnabled());
  });

  it('does not count a playing live tile as a clip playing', () => {
    const f = fakeUpdater(true);
    video(true, 'live');
    render(UpdatePrompt, { updater: f.u });
    expect(screen.getByRole('button', { name: 'Reload' })).toBeEnabled();
    expect(screen.queryByText(/Pause the video to update/)).toBeNull();
  });

  it('still holds the button when a clip plays next to a live tile', () => {
    const f = fakeUpdater(true);
    video(true, 'live');
    video(true, 'clip');
    render(UpdatePrompt, { updater: f.u });
    expect(screen.getByRole('button', { name: 'Reload' })).toBeDisabled();
  });
});
