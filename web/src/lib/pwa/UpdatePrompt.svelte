<script lang="ts">
  import { onMount } from 'svelte';
  import { anyVideoPlaying, type Updater } from './update';

  interface Props {
    /** Null where there is no service worker (development, unsupported browsers). */
    updater: Updater | null;
  }

  let { updater }: Props = $props();

  // svelte-ignore state_referenced_locally
  let available = $state(updater?.available ?? false);
  let playing = $state(false);

  onMount(() => {
    const sync = () => {
      available = updater?.available ?? false;
      playing = anyVideoPlaying(document);
    };
    sync();
    const off = updater?.onChange(sync);
    // Media events do not bubble: listen while they travel down to the video.
    const events = ['play', 'playing', 'pause', 'ended', 'emptied'];
    for (const e of events) document.addEventListener(e, sync, true);
    return () => {
      off?.();
      for (const e of events) document.removeEventListener(e, sync, true);
    };
  });
</script>

{#if available}
  <div role="status" class="fixed inset-x-3 top-3 z-50 mx-auto flex max-w-md flex-wrap items-center gap-3 rounded-lg border border-sky-700 bg-neutral-900 px-4 py-3 text-sm shadow-xl">
    <span>New version available — reload</span>
    <button
      type="button"
      class="ml-auto rounded bg-sky-600 px-3 py-1.5 text-white disabled:cursor-not-allowed disabled:opacity-50"
      disabled={playing}
      onclick={() => updater?.apply()}>Reload</button
    >
    {#if playing}
      <span class="w-full text-xs text-neutral-400">Pause the video to update.</span>
    {/if}
  </div>
{/if}
