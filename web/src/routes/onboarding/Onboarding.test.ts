// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { IDBFactory } from 'fake-indexeddb';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { manifest, masterText } from '../../../test-support/worker-fixtures';
import type { Request } from '../../worker/protocol';
import { openKeystore } from '../../worker/keystore';
import { createWorker } from '../../worker/worker';
import Onboarding from './Onboarding.svelte';

const KEY_ID = manifest.derived.key_id_hex;
const OTHER_ID = '0123456789abcdef';
const FP = KEY_ID.match(/.{4}/g)!.join('-');
const OTHER_FP = OTHER_ID.match(/.{4}/g)!.join('-');

beforeEach(() => {
  localStorage.clear();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const clipsAt = (...keyIds: string[]) => ({
  clips: keyIds.map((k, i) => ({
    start_time: new Date(Date.UTC(2026, 10, 5, 0, 0, i)).toISOString(),
    video: { key_id: k },
  })),
  invalid: 0,
});

/** A scripted worker client that records every call. */
function fakeClient(opts: { clips?: unknown; clipsError?: Error } = {}) {
  const calls: Request[] = [];
  let keys: { keyId: string; remembered: boolean }[] = [];
  const client = {
    async call<T>(msg: Request): Promise<T> {
      calls.push(msg);
      switch (msg.type) {
        case 'key.import':
          keys = [...keys.filter((k) => k.keyId !== KEY_ID), { keyId: KEY_ID, remembered: msg.remember }];
          return { keyId: KEY_ID } as T;
        case 'key.list':
          return { keys } as T;
        case 'key.forget':
          keys = keys.filter((k) => k.keyId !== msg.keyId);
          return { keys } as T;
        case 'key.lock':
          keys = keys.filter((k) => k.remembered);
          return { keys } as T;
        case 'api.get':
          if (opts.clipsError) throw opts.clipsError;
          return (opts.clips ?? clipsAt()) as T;
        default:
          throw new Error(`unexpected ${msg.type}`);
      }
    },
  };
  return { client, calls };
}

const NOW = Date.UTC(2026, 10, 6, 12, 0, 0);

async function typeKey(text: string) {
  const box = screen.getByLabelText(/master key/i);
  await fireEvent.input(box, { target: { value: text } });
  return box as HTMLInputElement;
}

describe('paste field with live checksum feedback', () => {
  it('shows nothing for an empty field and keeps Import disabled', () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    expect(screen.queryByText(/typo in key/i)).toBeNull();
    expect(screen.getByRole('button', { name: /import key/i })).toBeDisabled();
  });

  it('says "typo in key" when the checksum is wrong', async () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    const text = await masterText();
    await typeKey(text.slice(0, -1) + (text.endsWith('0') ? '1' : '0'));
    expect(screen.getByText(/typo in key/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /import key/i })).toBeDisabled();
  });

  it('accepts a valid key and enables Import', async () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    expect(screen.queryByText(/typo in key/i)).toBeNull();
    expect(screen.getByText(/key looks valid/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /import key/i })).toBeEnabled();
  });

  it('explains a malformed key differently from a typo', async () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    await typeKey('zk1-abc');
    expect(screen.queryByText(/typo in key/i)).toBeNull();
    expect(screen.getByText(/not a complete key/i)).toBeInTheDocument();
  });
});

describe('importing', () => {
  it('imports through the worker, shows the fingerprint and clears the field', async () => {
    const { client, calls } = fakeClient({ clips: clipsAt(KEY_ID) });
    render(Onboarding, { client, now: () => NOW });
    const text = await masterText();
    const box = await typeKey(text);
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    const ring = await screen.findByRole('list', { name: /keyring/i });
    expect(within(ring).getByText(FP)).toBeInTheDocument();
    expect(calls).toContainEqual({ type: 'key.import', encoded: text, remember: false });
    await waitFor(() => expect(box.value).toBe(''));
    // Neither the field nor any other node keeps the key text.
    expect(document.body.textContent).not.toContain('zk1-');
  });

  it('never puts the key in the URL, not even after a form submit', async () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    const before = location.href;
    await typeKey(await masterText());
    const form = screen.getByLabelText(/master key/i).closest('form');
    expect(form).not.toBeNull();
    expect(form!.getAttribute('action')).toBeNull();
    await fireEvent.submit(form!);
    await screen.findByRole('list', { name: /keyring/i });
    expect(location.href).toBe(before);
    expect(location.href).not.toContain('zk1');
    expect(location.hash).not.toContain('zk1');
  });

  it('passes the remember toggle to the worker', async () => {
    const { client, calls } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('checkbox', { name: /remember this device/i }));
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    await screen.findByRole('list', { name: /keyring/i });
    expect(calls.find((c) => c.type === 'key.import')).toMatchObject({ remember: true });
  });

  it('shows the worker error without leaking the key', async () => {
    const { client } = fakeClient();
    client.call = async () => ({ error: 'checksum' }) as never;
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/typo in key/i);
  });
});

describe('recent recordings check', () => {
  it('warns, with both fingerprints, when the latest clips carry another key', async () => {
    const { client, calls } = fakeClient({ clips: clipsAt(OTHER_ID, OTHER_ID) });
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    const warn = await screen.findByText(/this key doesn't match recent recordings/i);
    const box = warn.closest('[role="alert"]') as HTMLElement;
    expect(box).toHaveTextContent(FP);
    expect(box).toHaveTextContent(OTHER_FP);
    const get = calls.find((c) => c.type === 'api.get');
    expect(get).toMatchObject({ path: '/api/v1/clips' });
  });

  it('stays quiet when a recent clip carries the key', async () => {
    const { client } = fakeClient({ clips: clipsAt(OTHER_ID, KEY_ID) });
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    await screen.findByRole('list', { name: /keyring/i });
    await waitFor(() => expect(screen.queryByText(/checking recent recordings/i)).toBeNull());
    expect(screen.queryByText(/doesn't match/i)).toBeNull();
  });

  it('stays quiet when there are no clips yet', async () => {
    const { client } = fakeClient({ clips: clipsAt() });
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    await screen.findByRole('list', { name: /keyring/i });
    await waitFor(() => expect(screen.queryByText(/checking recent recordings/i)).toBeNull());
    expect(screen.queryByText(/doesn't match/i)).toBeNull();
  });

  it('keeps the import and says the check failed when the clips cannot be fetched', async () => {
    const { client } = fakeClient({ clipsError: new Error('boom') });
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    expect(await screen.findByText(/could not check recent recordings/i)).toBeInTheDocument();
    expect(screen.getByRole('list', { name: /keyring/i })).toBeInTheDocument();
  });
});

describe('lock and forget', () => {
  it('Lock asks the worker to discard derived keys', async () => {
    const { client, calls } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    await screen.findByRole('list', { name: /keyring/i });
    await fireEvent.click(screen.getByRole('button', { name: /^lock$/i }));
    await waitFor(() => expect(calls.some((c) => c.type === 'key.lock')).toBe(true));
    expect(await screen.findByText(/locked/i)).toBeInTheDocument();
  });

  it('Forget removes the key from the device: the IndexedDB keystore ends up empty', async () => {
    const idb = new IDBFactory();
    const worker = createWorker({ fetch: (async () => new Response('[]')) as typeof fetch, indexedDB: idb });
    const client = {
      call: async <T,>(msg: Request): Promise<T> => structuredClone((await worker.handle(msg)).result) as T,
    };
    render(Onboarding, { client, now: () => NOW });
    await typeKey(await masterText());
    await fireEvent.click(screen.getByRole('checkbox', { name: /remember this device/i }));
    await fireEvent.click(screen.getByRole('button', { name: /import key/i }));
    const ring = await screen.findByRole('list', { name: /keyring/i });
    expect(within(ring).getByText(FP)).toBeInTheDocument();

    const store = await openKeystore(idb);
    expect((await store.all()).map(([id]) => id)).toEqual([KEY_ID]);

    await fireEvent.click(screen.getByRole('button', { name: /forget key/i }));
    await waitFor(() => expect(screen.queryByRole('list', { name: /keyring/i })).toBeNull());
    expect(await store.all()).toEqual([]);
    store.close();
  });

  it('lists remembered keys found on the device at startup', async () => {
    const { client, calls } = fakeClient();
    await client.call({ type: 'key.import', encoded: 'x', remember: true });
    calls.length = 0;
    render(Onboarding, { client, now: () => NOW });
    const ring = await screen.findByRole('list', { name: /keyring/i });
    expect(within(ring).getByText(FP)).toBeInTheDocument();
    expect(within(ring).getByText(/remembered/i)).toBeInTheDocument();
    expect(calls[0]).toEqual({ type: 'key.list' });
  });
});

describe('idle auto-lock setting', () => {
  function setVisibility(hidden: boolean) {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
    document.dispatchEvent(new Event('visibilitychange'));
  }
  afterEach(() => setVisibility(false));

  it('defaults to off and persists the choice', async () => {
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    const select = screen.getByLabelText(/auto-lock/i) as HTMLSelectElement;
    expect(select.value).toBe('0');
    await fireEvent.change(select, { target: { value: '5' } });
    expect(localStorage.getItem('zview.idleLockMinutes')).toBe('5');
  });

  it('restores a saved setting', () => {
    localStorage.setItem('zview.idleLockMinutes', '15');
    const { client } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    expect((screen.getByLabelText(/auto-lock/i) as HTMLSelectElement).value).toBe('15');
  });

  it('locks the worker after the idle time with the tab hidden', async () => {
    vi.useFakeTimers();
    const { client, calls } = fakeClient();
    render(Onboarding, { client, now: () => NOW });
    await fireEvent.change(screen.getByLabelText(/auto-lock/i), { target: { value: '1' } });
    setVisibility(true);
    await vi.advanceTimersByTimeAsync(59_000);
    expect(calls.some((c) => c.type === 'key.lock')).toBe(false);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(calls.some((c) => c.type === 'key.lock')).toBe(true);
  });
});

describe('QR scanning', () => {
  it('puts the scanned key into the field for the same live check, without importing', async () => {
    const { client, calls } = fakeClient();
    const text = await masterText();
    render(Onboarding, {
      client,
      now: () => NOW,
      scanner: {
        open: async () => ({
          video: document.createElement('video'),
          grab: () => ({ data: new Uint8ClampedArray(4), width: 1, height: 1 }),
          stop: () => {},
        }),
        decode: async () => text,
        wait: async () => {},
      },
    });
    await fireEvent.click(screen.getByRole('button', { name: /scan qr/i }));
    await waitFor(() => expect((screen.getByLabelText(/master key/i) as HTMLInputElement).value).toBe(text));
    expect(screen.getByText(/key looks valid/i)).toBeInTheDocument();
    expect(calls.some((c) => c.type === 'key.import')).toBe(false);
    expect(location.href).not.toContain('zk1');
  });
});
