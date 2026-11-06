// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Camera } from '../../lib/qr/scan';
import Scanner from './Scanner.svelte';

afterEach(cleanup);

function fakeCamera() {
  const video = document.createElement('video');
  video.dataset.testid = 'camera';
  const stop = vi.fn();
  const camera: Camera = { video, grab: () => ({ data: new Uint8ClampedArray(4), width: 1, height: 1 }), stop };
  return { camera, stop };
}

describe('Scanner', () => {
  it('shows the camera, reports the decoded text and releases the camera', async () => {
    const { camera, stop } = fakeCamera();
    const onDecoded = vi.fn();
    let polls = 0;
    render(Scanner, {
      open: async () => camera,
      decode: async () => (++polls >= 2 ? 'zk1-decoded' : null),
      wait: async () => {},
      onDecoded,
      onClose: () => {},
    });
    await waitFor(() => expect(onDecoded).toHaveBeenCalledWith('zk1-decoded'));
    expect(document.querySelector('[data-testid="camera"]')).not.toBeNull();
    await waitFor(() => expect(stop).toHaveBeenCalled());
  });

  it('releases the camera when closed before anything is found', async () => {
    const { camera, stop } = fakeCamera();
    const { unmount } = render(Scanner, {
      open: async () => camera,
      decode: async () => null,
      wait: () => new Promise((r) => setTimeout(r, 1)),
      onDecoded: () => {},
      onClose: () => {},
    });
    await waitFor(() => expect(document.querySelector('[data-testid="camera"]')).not.toBeNull());
    unmount();
    await waitFor(() => expect(stop).toHaveBeenCalled());
  });

  it('explains when the camera cannot be opened, and offers to close', async () => {
    const onClose = vi.fn();
    render(Scanner, {
      open: async () => {
        throw Object.assign(new Error('denied'), { name: 'NotAllowedError' });
      },
      decode: async () => null,
      wait: async () => {},
      onDecoded: () => {},
      onClose,
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(/camera/i);
    screen.getByRole('button', { name: /close/i }).click();
    expect(onClose).toHaveBeenCalled();
  });
});
