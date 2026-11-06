<script lang="ts">
  import { onMount } from 'svelte';
  import type { WhepClient } from './whep';
  import { createTile, type TileSnapshot } from './tile';

  interface Props {
    cameraId: string;
    name?: string;
    client: WhepClient;
  }

  let { cameraId, name = cameraId, client }: Props = $props();

  let video: HTMLVideoElement | undefined = $state();
  let snap: TileSnapshot = $state({ state: 'connecting', canUnmute: false, muted: true });

  let toggle: () => void = () => {};

  onMount(() => {
    if (!video) return;
    const tile = createTile({ video, cameraId, client, onChange: (s) => (snap = s) });
    toggle = () => tile.toggleMute();
    tile.start();
    return () => tile.destroy();
  });

  const label = $derived(
    snap.state === 'live'
      ? 'Live'
      : snap.state === 'connecting'
        ? 'Connecting…'
        : snap.state === 'reconnecting'
          ? 'Reconnecting…'
          : 'Viewer limit reached',
  );
</script>

<div role="group" aria-label={name} class="relative overflow-hidden rounded-lg border border-neutral-800 bg-black" data-testid="live-tile" data-camera={cameraId} data-state={snap.state}>
  <video bind:this={video} muted playsinline autoplay class="aspect-video w-full bg-black object-contain"></video>
  <div class="absolute left-2 top-2 flex items-center gap-2 text-xs">
    <span class="rounded bg-black/60 px-2 py-0.5">{name}</span>
    <span
      role="status"
      class="rounded px-2 py-0.5 {snap.state === 'live'
        ? 'bg-red-600 text-white'
        : snap.state === 'limit'
          ? 'bg-amber-600 text-white'
          : 'bg-neutral-700 text-neutral-100'}">{label}</span
    >
  </div>
  {#if snap.canUnmute}
    <button
      type="button"
      class="absolute bottom-2 right-2 rounded bg-black/60 px-3 py-1 text-xs"
      aria-pressed={!snap.muted}
      onclick={() => toggle()}>{snap.muted ? 'Unmute' : 'Mute'}</button
    >
  {/if}
</div>
