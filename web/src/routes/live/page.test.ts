// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeClient, FakePeer, FakeStream } from '../../../test-support/live-fakes';
import type { Request } from '../../worker/protocol';
import LivePage from './+page.svelte';

class PageClient extends FakeClient {
  cameras: { id: string; name: string }[] = [
    { id: 'front', name: 'Front Door' },
    { id: 'yard', name: 'Yard' },
  ];
  override async call<T = unknown>(msg: Request): Promise<T> {
    if (msg.type === 'api.get') return this.cameras as T;
    return super.call<T>(msg);
  }
}

beforeEach(() => {
  FakePeer.instances = [];
  FakePeer.stallGathering = false;
  vi.stubGlobal('RTCPeerConnection', FakePeer);
  vi.stubGlobal('MediaStream', FakeStream);
  HTMLMediaElement.prototype.play = () => Promise.resolve();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('live page', () => {
  it('renders one muted tile per camera and marks it live once connected', async () => {
    render(LivePage, { client: new PageClient() });
    const tiles = await screen.findAllByTestId('live-tile');
    expect(tiles.map((t) => t.dataset.camera)).toEqual(['front', 'yard']);
    expect(tiles[0]!.querySelector('video')!.muted).toBe(true);
    await waitFor(() => expect(FakePeer.instances).toHaveLength(2));
    FakePeer.instances[0]!.setState('connected');
    await waitFor(() => expect(tiles[0]!.dataset.state).toBe('live'));
    expect(tiles[0]).toHaveTextContent('Live');
    expect(screen.queryByRole('button', { name: /unmute/i })).toBeNull();
  });

  it('offers unmute only after an audio track arrives', async () => {
    render(LivePage, { client: new PageClient() });
    await waitFor(() => expect(FakePeer.instances).toHaveLength(2));
    FakePeer.instances[0]!.addRemoteTrack('audio');
    const btn = await screen.findByRole('button', { name: 'Unmute' });
    await fireEvent.click(btn);
    expect(await screen.findByRole('button', { name: 'Mute' })).toBeInTheDocument();
  });

  it('shows the viewer-limit state when the server answers 503', async () => {
    const client = new PageClient();
    client.offerFailures.push({ name: 'LiveBusy', status: 503 });
    render(LivePage, { client });
    expect(await screen.findByText('Viewer limit reached')).toBeInTheDocument();
  });

  it('says so when no key is loaded', async () => {
    const client = new PageClient();
    client.call = async () => {
      throw Object.assign(new Error('no key'), { name: 'ErrNoKey' });
    };
    render(LivePage, { client });
    expect(await screen.findByRole('alert')).toHaveTextContent('Import your key first.');
  });
});
