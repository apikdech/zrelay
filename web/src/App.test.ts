// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

// The pages need the worker and the browser APIs; only the routing is under test.
vi.mock('./routes/+page.svelte', () => import('../test-support/routes/Timeline.svelte'));
vi.mock('./routes/live/+page.svelte', () => import('../test-support/routes/Live.svelte'));
vi.mock('./routes/demo/+page.svelte', () => import('../test-support/routes/Demo.svelte'));
vi.mock('./routes/onboarding/Onboarding.svelte', () => import('../test-support/routes/Key.svelte'));

import App from './App.svelte';

afterEach(() => {
  cleanup();
  location.hash = '';
});

const shown = () =>
  ['timeline', 'live', 'demo', 'key'].filter((r) => screen.queryByTestId(`route-${r}`) !== null);

describe('App routing', () => {
  it('shows the timeline for an empty hash', () => {
    location.hash = '';
    render(App);
    expect(shown()).toEqual(['timeline']);
  });

  it('shows the timeline for an unknown hash', () => {
    location.hash = '#/nowhere';
    render(App);
    expect(shown()).toEqual(['timeline']);
  });

  it('shows the timeline for #/timeline', () => {
    location.hash = '#/timeline';
    render(App);
    expect(shown()).toEqual(['timeline']);
    expect(screen.getByRole('link', { name: 'Timeline' })).toHaveAttribute('aria-current', 'page');
  });

  it('shows the onboarding page for #/key', () => {
    location.hash = '#/key';
    render(App);
    expect(shown()).toEqual(['key']);
    expect(screen.getByRole('link', { name: 'Key' })).toHaveAttribute('aria-current', 'page');
  });

  it('shows live and demo for their routes, passing the demo clip', () => {
    location.hash = '#/live';
    const { unmount } = render(App);
    expect(shown()).toEqual(['live']);
    unmount();
    location.hash = '#/demo?clip=01M6RXRJAQRJY105FSDF0QRBH1';
    render(App);
    expect(shown()).toEqual(['demo']);
    expect(screen.getByTestId('route-demo')).toHaveAttribute('data-clip', '01M6RXRJAQRJY105FSDF0QRBH1');
  });

  it('follows hash changes', async () => {
    location.hash = '';
    render(App);
    expect(shown()).toEqual(['timeline']);
    location.hash = '#/key';
    await fireEvent(window, new HashChangeEvent('hashchange'));
    await waitFor(() => expect(shown()).toEqual(['key']));
    location.hash = '#/bogus';
    await fireEvent(window, new HashChangeEvent('hashchange'));
    await waitFor(() => expect(shown()).toEqual(['timeline']));
  });
});
