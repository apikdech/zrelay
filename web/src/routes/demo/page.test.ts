// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ErrAuth, ErrUnknownKey } from '../../lib/zrec/errors';
import {
  FakeClient,
  FakeMediaSource,
  chunkBytes,
  installMedia,
  openResult,
  type Env,
} from '../../../test-support/player-fakes';
import type { WorkerClient } from '../../lib/workerClient';
import DemoPage from './+page.svelte';

const CLIP = '01M2NBMVS0K8EKRQKGGAJCDT0B';
let env: Env;

beforeEach(() => {
  env = installMedia({ mms: false });
  // jsdom's <video> has no media stack: make the engine's reads of it harmless.
  HTMLMediaElement.prototype.play = () => Promise.resolve();
  HTMLMediaElement.prototype.load = () => undefined;
  HTMLMediaElement.prototype.canPlayType = () => 'probably';
  Object.defineProperty(HTMLMediaElement.prototype, 'src', {
    configurable: true,
    get() {
      return this.getAttribute('src') ?? '';
    },
    set(v: string) {
      this.setAttribute('src', v);
      const ms = env.registry.get(v);
      if (ms instanceof FakeMediaSource) {
        queueMicrotask(() => {
          ms.readyState = 'open';
          ms.dispatchEvent(new Event('sourceopen'));
        });
      }
    },
  });
});
afterEach(() => {
  cleanup();
  env.restore();
});

const asClient = (c: FakeClient) => c as unknown as WorkerClient;

describe('demo page', () => {
  it('shows the verifying spinner, then the verified badge for a good clip', async () => {
    render(DemoPage, { client: asClient(new FakeClient({ open: openResult({ numChunks: 3 }) })), clipId: CLIP });
    const root = screen.getByTestId('demo');
    await waitFor(() => expect(root.dataset.integrity).toBe('verified'));
    expect(root.dataset.clip).toBe(CLIP);
    expect(screen.getByTestId('integrity')).toHaveTextContent('Verified');
    expect(screen.getByTestId('integrity')).toHaveAttribute('title', 'Encrypted recording verified');
    await waitFor(() => expect(root.dataset.engine).toBe('mse'));
  });

  it('shows where a tampered recording failed and offers no export', async () => {
    const c = new FakeClient({
      open: openResult({ numChunks: 6 }),
      read: (i) => (i === 3 ? new ErrAuth(3) : { plaintext: chunkBytes(i), final: false }),
    });
    render(DemoPage, { client: asClient(c), clipId: CLIP });
    const root = screen.getByTestId('demo');
    await waitFor(() => expect(root.dataset.integrity).toBe('tampered'));
    expect(root.dataset.failedAtMs).toBe('3000');
    expect(screen.getByTestId('integrity')).toHaveTextContent('This recording failed verification at ~00:03');
    expect(screen.queryByRole('button', { name: /export/i })).toBeNull();
  });

  it('names the key of a clip recorded with an unknown key', async () => {
    const c = new FakeClient({ open: new ErrUnknownKey('x'), sidecar: { video: { key_id: 'a1b2c3d4e5f60718' } } });
    render(DemoPage, { client: asClient(c), clipId: CLIP });
    await waitFor(() =>
      expect(screen.getByTestId('integrity')).toHaveTextContent('Recorded with key a1b2-c3d4-e5f6-0718; add that key to play'),
    );
  });

  it('loads the id typed into the form and seeks from the seek box', async () => {
    const c = new FakeClient({ open: openResult({ numChunks: 3 }) });
    render(DemoPage, { client: asClient(c) });
    await fireEvent.input(screen.getByLabelText('Clip id'), { target: { value: CLIP } });
    await fireEvent.click(screen.getByRole('button', { name: 'Load' }));
    await waitFor(() => expect(screen.getByTestId('demo').dataset.integrity).toBe('verified'));
    expect(c.calls.filter((m) => m.type === 'clip.open')).toEqual([{ type: 'clip.open', clipId: CLIP }]);
    await fireEvent.input(screen.getByTestId('seek-input'), { target: { value: '1' } });
    await fireEvent.click(screen.getByTestId('seek-go'));
    await waitFor(() => expect(screen.getByTestId('player') as HTMLVideoElement).toBeTruthy());
  });

  it('rejects a malformed clip id without calling the worker', async () => {
    const c = new FakeClient();
    render(DemoPage, { client: asClient(c) });
    await fireEvent.input(screen.getByLabelText('Clip id'), { target: { value: 'nope' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Load' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter a clip id');
    expect(c.calls).toHaveLength(0);
  });
});
