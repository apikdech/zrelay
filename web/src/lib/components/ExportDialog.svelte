<script lang="ts">
  import { onMount } from 'svelte';

  interface Props {
    filename: string;
    busy: boolean;
    /** Plaintext bytes saved so far. */
    bytes: number;
    error: string;
    onConfirm: () => void;
    onCancel: () => void;
  }

  let { filename, busy, bytes, error, onConfirm, onCancel }: Props = $props();

  let dialog: HTMLDivElement;
  onMount(() => dialog.focus());

  const mib = (n: number): string => `${(n / (1 << 20)).toFixed(1)} MiB`;
</script>

<div class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
  <div
    bind:this={dialog}
    role="dialog"
    aria-modal="true"
    aria-labelledby="export-title"
    tabindex="-1"
    onkeydown={(e) => {
      if (e.key === 'Escape' && !busy) {
        e.stopPropagation();
        onCancel();
      }
    }}
    class="w-full max-w-md rounded-lg border border-neutral-700 bg-neutral-900 p-5 text-sm shadow-xl"
  >
    <h2 id="export-title" class="mb-2 text-base font-semibold">Export recording</h2>
    <p class="mb-2">This saves an <strong>unencrypted</strong> copy on this device.</p>
    <p class="mb-3 break-all font-mono text-xs text-neutral-400">{filename}</p>
    {#if busy}
      <p role="status" class="mb-3 text-neutral-300">Saving… {bytes > 0 ? mib(bytes) : ''}</p>
    {:else if bytes > 0 && !error}
      <p class="mb-3 text-neutral-400">{mib(bytes)}</p>
    {/if}
    {#if error}
      <p role="alert" class="mb-3 text-red-400">{error}</p>
    {/if}
    <div class="flex justify-end gap-2">
      <button type="button" class="rounded border border-neutral-600 px-3 py-1.5" disabled={busy} onclick={onCancel}>Cancel</button>
      <button type="button" class="rounded bg-sky-600 px-3 py-1.5 text-white disabled:opacity-50" disabled={busy} onclick={onConfirm}>
        {busy ? 'Saving…' : 'Save'}
      </button>
    </div>
  </div>
</div>
