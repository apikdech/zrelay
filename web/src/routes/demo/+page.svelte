<script lang="ts">
  import { onMount } from 'svelte';
  import { detectProfile } from '../../lib/player/buffer';
  import { createPlayer, type Player, type PlayerState } from '../../lib/player/engine';
  import { describeIntegrity, initialState } from '../../lib/player/status';
  import { getWorkerClient, type WorkerClient } from '../../lib/workerClient';
  import { isUlid } from '../../worker/schemas';

  interface Props {
    /** The crypto worker; it decrypts and authenticates every chunk. */
    client?: WorkerClient;
    /** The clip to play; changes load the new clip. */
    clipId?: string;
  }

  let { client, clipId = '' }: Props = $props();

  let video: HTMLVideoElement | undefined = $state();
  let player: Player | undefined = $state();
  let snap: PlayerState = $state(initialState());
  let draft = $state('');
  let seekSeconds = $state('0');
  let autoAdvance = $state(false);
  let loadError = $state('');
  let profile = $state('');

  const view = $derived(describeIntegrity(snap));

  onMount(() => {
    if (!video) return;
    const detected = detectProfile();
    profile = detected;
    const p = createPlayer(video, client ?? getWorkerClient(), { profile: detected });
    const off = p.onState((s) => (snap = s));
    player = p;
    return () => {
      off();
      p.destroy();
    };
  });

  function open(id: string): void {
    loadError = '';
    if (!player || !video) return;
    const v = video;
    player
      .load(id)
      .then(() => v.play())
      .catch((e: unknown) => {
        // Autoplay can be refused; only real failures are shown.
        if (e instanceof DOMException && e.name === 'NotAllowedError') return;
        loadError = e instanceof Error && e.name === 'ErrNoKey' ? 'Import your key first.' : 'Could not open this recording.';
      });
  }

  $effect(() => {
    const id = clipId;
    if (player && isUlid(id)) {
      draft = id;
      open(id);
    }
  });

  function seek(): void {
    const s = Number(seekSeconds);
    if (player && Number.isFinite(s) && s >= 0) void player.seek(s * 1000);
  }

  $effect(() => {
    player?.setAutoAdvance(autoAdvance);
  });
</script>

<main
  class="mx-auto max-w-3xl p-4"
  data-testid="demo"
  data-clip={snap.clipId ?? ''}
  data-integrity={snap.integrity}
  data-engine={snap.engine ?? ''}
  data-profile={profile}
  data-failed-at-ms={snap.failedAtMs ?? ''}
>
  <h1 class="mb-3 text-xl font-semibold">Playback</h1>

  <form
    class="mb-3 flex gap-2 text-sm"
    onsubmit={(e) => {
      e.preventDefault();
      if (isUlid(draft)) open(draft);
      else loadError = 'Enter a clip id (26 characters).';
    }}
  >
    <label class="flex flex-1 items-center gap-2">
      <span class="text-neutral-400">Clip id</span>
      <input class="flex-1 rounded border border-neutral-700 bg-transparent px-2 py-1 font-mono" bind:value={draft} spellcheck="false" autocomplete="off" />
    </label>
    <button type="submit" class="rounded border border-neutral-600 px-3 py-1">Load</button>
  </form>

  <div class="relative overflow-hidden rounded-lg border border-neutral-800 bg-black">
    <video bind:this={video} data-testid="player" data-clip-player controls muted playsinline class="aspect-video w-full bg-black object-contain"></video>
    {#if view.spinner}
      <div class="absolute inset-0 flex items-center justify-center bg-black/50 text-sm" role="progressbar" aria-label="Verifying">
        <span class="animate-pulse">{view.text}</span>
      </div>
    {/if}
  </div>

  <div
    data-testid="integrity"
    data-kind={view.kind}
    title={view.title}
    role={view.kind === 'tampered' || view.kind === 'mismatch' || view.kind === 'unknown-key' ? 'alert' : 'status'}
    class="mt-2 rounded px-3 py-2 text-sm {view.kind === 'tampered'
      ? 'bg-red-900/60 text-red-100'
      : view.kind === 'verified'
        ? 'text-emerald-400'
        : view.kind === 'verifying'
          ? 'text-neutral-400'
          : 'bg-amber-900/50 text-amber-100'}"
  >
    {#if view.kind === 'verified'}<span aria-hidden="true">✓ </span>{/if}{view.text}
  </div>
  {#if snap.message}
    <p role="alert" data-testid="message" class="mt-2 text-sm text-amber-300">{snap.message}</p>
  {/if}
  {#if loadError}
    <p role="alert" class="mt-2 text-sm text-red-400">{loadError}</p>
  {/if}

  <div class="mt-3 flex flex-wrap items-center gap-4 text-sm">
    <label class="flex items-center gap-2">
      <span class="text-neutral-400">Seek to (s)</span>
      <input
        data-testid="seek-input"
        class="w-20 rounded border border-neutral-700 bg-transparent px-2 py-1"
        type="number"
        min="0"
        step="any"
        bind:value={seekSeconds}
      />
    </label>
    <button type="button" data-testid="seek-go" class="rounded border border-neutral-600 px-3 py-1" onclick={seek}>Seek</button>
    <label class="flex items-center gap-2">
      <input type="checkbox" bind:checked={autoAdvance} />
      <span>Autoplay next</span>
    </label>
  </div>
</main>
