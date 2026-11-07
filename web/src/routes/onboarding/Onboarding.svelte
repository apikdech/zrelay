<script lang="ts">
  import { onMount } from 'svelte';
  import type { Camera, Decoder } from '../../lib/qr/scan';
  import { workerClient } from '../../lib/workerClient';
  import type { ImportResult, KeyListResult, Request } from '../../worker/protocol';
  import { acquireIdleLock, IDLE_CHOICES, savedIdleMinutes, type IdleHandle } from './idleService';
  import { checkKeyInput, fingerprint } from './keyInput';
  import { keyMismatch, type ClipKeyRef, type Mismatch } from './mismatch';
  import Scanner from './Scanner.svelte';

  interface Props {
    /** The crypto worker; keys never leave it. */
    client?: { call<T = unknown>(msg: Request): Promise<T> };
    now?: () => number;
    /** Test hooks for the QR scanner. */
    scanner?: { open?: () => Promise<Camera>; decode?: Decoder; wait?: (ms: number) => Promise<void> };
  }

  let { client = workerClient, now = Date.now, scanner = {} }: Props = $props();

  const DAY_MS = 86_400_000;

  let text = $state('');
  let remember = $state(false);
  let keys: KeyListResult['keys'] = $state([]);
  let importError = $state('');
  let locked = $state(false);
  let scanning = $state(false);
  let mismatch: Mismatch | null = $state(null);
  let check: '' | 'checking' | 'failed' | 'rejected' = $state('');
  let actionError = $state('');
  let idleMinutes = $state(savedIdleMinutes());
  let idle: IdleHandle | undefined;
  let checkSeq = 0;

  const input = $derived(checkKeyInput(text));

  async function refresh(result?: KeyListResult) {
    keys = (result ?? (await client.call<KeyListResult>({ type: 'key.list' }))).keys;
  }

  async function checkRecordings(keyId: string) {
    const seq = ++checkSeq;
    mismatch = null;
    check = 'checking';
    try {
      const t = now();
      const res = await client.call<{ clips: ClipKeyRef[] }>({
        type: 'api.get',
        path: '/api/v1/clips',
        query: { from: new Date(t - 7 * DAY_MS).toISOString(), to: new Date(t + 3_600_000).toISOString() },
      });
      if (seq !== checkSeq) return;
      mismatch = keyMismatch(res.clips, keyId);
      check = '';
    } catch (e) {
      if (seq !== checkSeq) return;
      // 401: the server did not accept the Bearer derived from this key. 429
      // means the same: the server never refuses a valid credential, it only
      // rate-limits failed ones, so this key failed too.
      const status = (e as { status?: number } | null)?.status;
      check = status === 401 || status === 429 ? 'rejected' : 'failed';
    }
  }

  /** Drops any in-flight recent-recordings check and clears its result. */
  function cancelCheck() {
    checkSeq++;
    mismatch = null;
    check = '';
  }

  async function doImport() {
    if (input !== 'valid') return;
    importError = '';
    actionError = '';
    try {
      const res = await client.call<ImportResult>({ type: 'key.import', encoded: text, remember });
      if ('error' in res) {
        importError = res.error === 'checksum' ? 'typo in key' : 'Not a complete key.';
        return;
      }
      text = ''; // the key text must not linger in the page
      locked = false;
      await refresh();
      void checkRecordings(res.keyId);
    } catch {
      actionError = 'Could not import the key. Try again.'; // fixed text: never the key or the raw error
    }
  }

  async function doLock() {
    actionError = '';
    cancelCheck();
    try {
      await refresh(await client.call<KeyListResult>({ type: 'key.lock' }));
      locked = true;
    } catch {
      actionError = 'Could not lock. Try again.';
    }
  }

  async function doForget(keyId: string) {
    actionError = '';
    cancelCheck();
    try {
      await refresh(await client.call<KeyListResult>({ type: 'key.forget', keyId }));
      locked = true;
    } catch {
      actionError = 'Could not forget the key. Try again.';
    }
  }

  function setIdle(e: Event) {
    idleMinutes = Number((e.currentTarget as HTMLSelectElement).value);
    idle?.setMinutes(idleMinutes); // persists, and applies app-wide
  }

  function onScanned(scanned: string) {
    scanning = false;
    text = scanned;
  }

  onMount(() => {
    // The lock itself is app-level (App holds it); this page only changes the
    // setting and mirrors an idle lock that happens while it is open.
    idle = acquireIdleLock(client, {
      onLocked: (result) => {
        actionError = '';
        cancelCheck();
        void refresh(result);
        locked = true;
      },
      onError: () => {
        actionError = 'Could not lock. Try again.';
      },
    });
    void refresh();
    return () => idle?.release();
  });
</script>

<main class="mx-auto max-w-xl space-y-6 p-4">
  <h1 class="text-xl font-semibold">Key</h1>

  <form
    class="space-y-3"
    onsubmit={(e) => {
      e.preventDefault(); // a native submit would put the key in the URL
      void doImport();
    }}
  >
    <label class="block text-sm" for="master-key">Master key</label>
    <input
      id="master-key"
      type="text"
      class="w-full rounded border border-neutral-700 bg-neutral-900 p-2 font-mono text-sm"
      autocomplete="off"
      autocapitalize="off"
      spellcheck="false"
      placeholder="zk1-…"
      bind:value={text}
    />
    {#if input === 'typo'}
      <p class="text-sm text-red-400">typo in key</p>
    {:else if input === 'format'}
      <p class="text-sm text-neutral-400">Not a complete key yet (zk1-… with 64 hex digits and a 4-digit check).</p>
    {:else if input === 'valid'}
      <p class="text-sm text-green-400">Key looks valid.</p>
    {/if}
    {#if importError}
      <p role="alert" class="text-sm text-red-400">{importError}</p>
    {/if}

    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" bind:checked={remember} />
      Remember this device
    </label>

    <div class="flex gap-2">
      <button
        type="submit"
        class="rounded bg-blue-600 px-3 py-1 text-sm disabled:opacity-40"
        disabled={input !== 'valid'}>Import key</button
      >
      <button type="button" class="rounded bg-neutral-800 px-3 py-1 text-sm" onclick={() => (scanning = true)}
        >Scan QR code</button
      >
    </div>
  </form>

  {#if scanning}
    <Scanner open={scanner.open} decode={scanner.decode} wait={scanner.wait} onDecoded={onScanned} onClose={() => (scanning = false)} />
  {/if}

  {#if check === 'checking'}
    <p class="text-sm text-neutral-400">Checking recent recordings…</p>
  {:else if check === 'failed'}
    <p class="text-sm text-neutral-400">Could not check recent recordings.</p>
  {:else if check === 'rejected'}
    <p role="status" class="text-sm text-amber-300">The server did not accept this key. Check that it is the right one.</p>
  {/if}
  {#if actionError}
    <p role="alert" class="text-sm text-red-400">{actionError}</p>
  {/if}
  {#if mismatch}
    <div role="alert" class="space-y-1 rounded border border-amber-500 p-3 text-sm text-amber-300">
      <p class="font-medium">This key doesn't match recent recordings</p>
      <p>Imported key: <code>{fingerprint(mismatch.keyId)}</code></p>
      <p>
        Recent clips use:
        {#each mismatch.clipKeyIds as id, i (id)}{i ? ', ' : ''}<code>{fingerprint(id)}</code>{/each}
      </p>
    </div>
  {/if}

  {#if locked}
    <p class="text-sm text-neutral-400">Locked: session keys were discarded.</p>
  {/if}

  {#if keys.length > 0}
    <ul aria-label="Keyring" class="space-y-2">
      {#each keys as k (k.keyId)}
        <li class="flex items-center justify-between rounded border border-neutral-800 p-2">
          <span><code>{fingerprint(k.keyId)}</code>{#if k.remembered} <small class="text-neutral-400">remembered</small>{/if}</span>
          <button
            type="button"
            class="rounded bg-neutral-800 px-2 py-1 text-xs"
            aria-label="Forget key {fingerprint(k.keyId)} on this device"
            onclick={() => void doForget(k.keyId)}>Forget</button
          >
        </li>
      {/each}
    </ul>
  {/if}

  <div class="flex items-center gap-4">
    <button type="button" class="rounded bg-neutral-800 px-3 py-1 text-sm" onclick={() => void doLock()}>Lock</button>
    <label class="flex items-center gap-2 text-sm">
      Auto-lock when tab is hidden
      <select class="rounded bg-neutral-900 p-1" value={String(idleMinutes)} onchange={setIdle}>
        {#each IDLE_CHOICES as m (m)}
          <option value={String(m)}>{m === 0 ? 'Off' : `${m} min`}</option>
        {/each}
      </select>
    </label>
  </div>
</main>
