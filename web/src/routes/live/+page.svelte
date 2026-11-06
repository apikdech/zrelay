<script lang="ts">
  import { onMount } from 'svelte';
  import LiveTile from '../../lib/live/LiveTile.svelte';
  import { workerClient } from '../../lib/workerClient';
  import type { Request } from '../../worker/protocol';
  import type { Camera } from '../../worker/schemas';

  interface Props {
    /** The crypto worker; it holds K_api and does the WHEP signaling. */
    client?: { call<T = unknown>(msg: Request): Promise<T> };
  }

  let { client = workerClient }: Props = $props();

  let cameras: Camera[] | null = $state(null);
  let error = $state('');

  onMount(() => {
    let alive = true;
    client
      .call<Camera[]>({ type: 'api.get', path: '/api/v1/cameras', query: {} })
      .then((c) => {
        if (alive) cameras = c;
      })
      .catch((e: unknown) => {
        if (alive) error = e instanceof Error && e.name === 'ErrNoKey' ? 'Import your key first.' : 'Could not load cameras.';
      });
    return () => {
      alive = false;
    };
  });
</script>

<main class="mx-auto max-w-6xl p-4">
  <h1 class="mb-4 text-xl font-semibold">Live</h1>
  {#if error}
    <p role="alert" class="text-sm text-red-400">{error}</p>
  {:else if cameras === null}
    <p class="text-sm text-neutral-400">Loading cameras…</p>
  {:else if cameras.length === 0}
    <p class="text-sm text-neutral-400">No cameras.</p>
  {:else}
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
      {#each cameras as cam (cam.id)}
        <LiveTile cameraId={cam.id} name={cam.name} {client} />
      {/each}
    </div>
  {/if}
</main>
