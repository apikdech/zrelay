<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { browserExportEnv, canExport, exportClip, exportFilename, type ExportEnv } from '../export';
  import { BLOB_MAX, detectProfile, type Profile } from '../player/buffer';
  import { createPlayer, type Player, type PlayerState } from '../player/engine';
  import { describeIntegrity, initialState } from '../player/status';
  import type { WorkerClient } from '../workerClient';
  import ExportDialog from './ExportDialog.svelte';
  import IntegrityBadge from './IntegrityBadge.svelte';

  /** Where to play: a clip, and how far into it. A new object (`n`) seeks again even within the same clip. */
  export interface Target {
    clipId: string;
    offsetMs: number;
    n: number;
  }

  interface Props {
    client: Pick<WorkerClient, 'call'>;
    cue: Target | null;
    /** Display zone for the export filename. */
    zone: string;
    profile?: Profile;
    /** The playback engine; replaceable for tests. */
    factory?: (video: HTMLVideoElement, client: WorkerClient, opts: { profile: Profile }) => Player;
    env?: ExportEnv;
    /** Called with every state the engine reports. */
    onState?: (s: PlayerState) => void;
  }

  let { client, cue, zone, profile, factory = createPlayer, env, onState }: Props = $props();

  let video: HTMLVideoElement | undefined = $state();
  let player: Player | undefined = $state();
  let snap: PlayerState = $state(initialState());
  let loadError = $state('');
  let autoAdvance = $state(true);
  /** The clip the engine has been asked for (kept in step with auto-advance). */
  let current: string | null = null;
  let chain: Promise<void> = Promise.resolve();

  const view = $derived(describeIntegrity(snap));
  const mp4Profile = $derived(profile ?? detectProfile());

  onMount(() => {
    if (!video) return;
    const p = factory(video, client as WorkerClient, { profile: mp4Profile });
    const off = p.onState((s) => {
      snap = s;
      if (s.clipId !== null) current = s.clipId;
      onState?.(s);
    });
    player = p;
    return () => {
      off();
      p.destroy();
    };
  });

  $effect(() => {
    player?.setAutoAdvance(autoAdvance);
  });

  function play(v: HTMLVideoElement): void {
    v.play().catch((e: unknown) => {
      // Autoplay can be refused; the viewer then presses play.
      if (!(e instanceof DOMException && e.name === 'NotAllowedError')) loadError = 'Could not start playback.';
    });
  }

  async function go(p: Player, v: HTMLVideoElement, t: Target): Promise<void> {
    loadError = '';
    try {
      const sameClip = t.clipId === current;
      if (!sameClip) {
        current = t.clipId;
        await p.load(t.clipId);
      }
      if (sameClip || t.offsetMs > 0) await p.seek(t.offsetMs);
      play(v);
    } catch (e) {
      current = null;
      loadError = e instanceof Error && e.name === 'ErrNoKey' ? 'Import your key first.' : 'Could not open this recording.';
    }
  }

  $effect(() => {
    const t = cue;
    untrack(() => {
      const p = player;
      const v = video;
      if (!t || !p || !v) return;
      chain = chain.then(() => go(p, v, t));
    });
  });

  export function togglePlay(): void {
    if (!video) return;
    if (video.paused) play(video);
    else video.pause();
  }

  /** Moves the playhead by `deltaMs`, never before the start. */
  export function skip(deltaMs: number): void {
    if (!video || !player) return;
    const to = Math.max(0, video.currentTime * 1000 + deltaMs);
    void player.seek(to);
  }

  // ---- export --------------------------------------------------------------

  let exporting = $state(false);
  let exportBusy = $state(false);
  let exportBytes = $state(0);
  let exportError = $state('');

  const header = $derived(snap.header);
  const exportName = $derived(
    header ? exportFilename(header.cameraId, header.startUnixMs, zone, snap.integrity === 'incomplete') : '',
  );
  const exportable = $derived(canExport(snap.integrity) && snap.clipId !== null && header !== null);
  const exportHint = $derived(
    exportable
      ? 'Save an unencrypted copy'
      : snap.integrity === 'tampered'
        ? 'This recording failed verification and cannot be exported'
        : 'Export is available once the recording is verified',
  );

  function openExport(): void {
    video?.pause();
    exportError = '';
    exportBytes = 0;
    exporting = true;
  }

  async function confirmExport(): Promise<void> {
    const clipId = snap.clipId;
    if (!clipId || exportBusy) return;
    exportBusy = true;
    exportError = '';
    exportBytes = 0;
    const resumeAt = (video?.currentTime ?? 0) * 1000;
    let touched = false;
    try {
      // The save picker needs the click's user activation, so exportClip opens it before anything else.
      const res = await exportClip({
        client,
        clipId,
        filename: exportName,
        integrity: snap.integrity,
        blobMax: BLOB_MAX[mp4Profile],
        env: env ?? browserExportEnv(),
        onProgress: (n) => (exportBytes = n),
      });
      touched = res.kind === 'saved';
      if (touched) exporting = false;
    } catch (e) {
      touched = true;
      exportError = e instanceof Error && e.message ? e.message : 'Export failed.';
    } finally {
      exportBusy = false;
      // Exporting reopened the clip in the worker, which dropped the player's session: open it again where it was.
      if (touched && player && snap.clipId === clipId) {
        const p = player;
        chain = chain.then(async () => {
          try {
            await p.load(clipId);
            if (resumeAt > 0) await p.seek(resumeAt);
          } catch {
            loadError = 'Could not reopen this recording.';
          }
        });
      }
    }
  }
</script>

<section class="min-w-0" aria-label="Player">
  <div class="relative overflow-hidden rounded-lg border border-neutral-800 bg-black">
    <video bind:this={video} data-testid="player" controls muted playsinline class="aspect-video w-full bg-black object-contain"></video>
    {#if view.spinner}
      <div class="absolute inset-0 flex items-center justify-center bg-black/50 text-sm" role="progressbar" aria-label="Verifying">
        <span class="animate-pulse">{view.text}</span>
      </div>
    {/if}
  </div>

  <div class="mt-2 flex flex-wrap items-center gap-3 text-sm">
    <span data-testid="integrity" data-kind={view.kind} class="flex items-center gap-1" title={view.title}>
      <IntegrityBadge state={snap.integrity} />
      {#if snap.clipId === null}
        <span class="text-neutral-400">Select a recording</span>
      {/if}
    </span>
    <label class="ml-auto flex items-center gap-1 text-neutral-300">
      <input type="checkbox" bind:checked={autoAdvance} />
      <span>Autoplay next</span>
    </label>
    <button
      type="button"
      class="rounded border border-neutral-600 px-3 py-1 disabled:cursor-not-allowed disabled:opacity-40"
      disabled={!exportable}
      title={exportHint}
      onclick={openExport}>Export</button
    >
  </div>

  {#if view.kind !== 'verified' && snap.clipId !== null && !view.spinner}
    <p
      role={view.kind === 'incomplete' ? 'status' : 'alert'}
      class="mt-2 rounded px-3 py-2 text-sm {view.kind === 'tampered' ? 'bg-red-900/60 text-red-100' : 'bg-amber-900/50 text-amber-100'}"
    >
      {view.text}
    </p>
  {/if}
  {#if snap.message}
    <p role="status" class="mt-2 text-sm text-amber-300">{snap.message}</p>
  {/if}
  {#if loadError}
    <p role="alert" class="mt-2 text-sm text-red-400">{loadError}</p>
  {/if}
</section>

{#if exporting}
  <ExportDialog
    filename={exportName}
    busy={exportBusy}
    bytes={exportBytes}
    error={exportError}
    onConfirm={() => void confirmExport()}
    onCancel={() => (exporting = false)}
  />
{/if}
