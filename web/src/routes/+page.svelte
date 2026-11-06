<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import EventList, { type EventRow } from '../lib/components/EventList.svelte';
  import Header from '../lib/components/Header.svelte';
  import Player, { type Target } from '../lib/components/Player.svelte';
  import { loadDay, localDayOf, toTimelineClip } from '../lib/dashboard/data';
  import type { Profile } from '../lib/player/buffer';
  import type { createPlayer, IntegrityState, PlayerState } from '../lib/player/engine';
  import { adjacentClip, commandFor, type Command } from '../lib/shortcuts';
  import Timeline from '../lib/timeline/Timeline.svelte';
  import type { Coverage } from '../lib/timeline/model';
  import { browserZone } from '../lib/timeline/timezone';
  import { loadZonePref, saveZonePref } from '../lib/timeline/zonePref';
  import { getWorkerClient, type WorkerClient } from '../lib/workerClient';
  import type { ExportEnv } from '../lib/export';
  import type { Camera, Sidecar, Status } from '../worker/schemas';

  interface Props {
    /** The crypto/data worker; it holds K_api and does every authenticated request. */
    client?: Pick<WorkerClient, 'call'>;
    /** Display zone to start in; otherwise the viewer's remembered choice, else their own zone. */
    zone?: string;
    /** Fixed "now" for tests; otherwise the wall clock, refreshed every 30 s. */
    nowMs?: number;
    profile?: Profile;
    factory?: typeof createPlayer;
    exportEnv?: ExportEnv;
  }

  let { client = getWorkerClient(), zone: initialZone, nowMs, profile, factory, exportEnv }: Props = $props();

  // The forced display zone (e.g. Asia/Jakarta for relatives abroad); null follows the viewer's own.
  let forcedZone = $state<string | null>(untrack(() => initialZone ?? loadZonePref()));
  const ownZone = browserZone();
  const zone = $derived(forcedZone ?? ownZone);

  let wall = $state(Date.now());
  const now = $derived(nowMs ?? wall);

  let cameras = $state<Camera[] | null>(null);
  let clips = $state<Sidecar[]>([]);
  let coverage = $state<Coverage[]>([]);
  let invalid = $state(0);
  let status = $state<Status | null>(null);
  let error = $state('');
  let loading = $state(false);

  let camera = $state('');
  let trigger = $state('');
  let day = $state(untrack(() => localDayOf(nowMs ?? Date.now(), zone)));

  let selectedId = $state<string | null>(null);
  let cue = $state<Target | null>(null);
  let cueN = 0;
  let integrity = $state<Record<string, IntegrityState | undefined>>({});
  let sheetOpen = $state(false);

  let playerView: { togglePlay(): void; skip(ms: number): void } | undefined = $state();
  let timeline: { stepZoom(dir: 'in' | 'out'): void; jumpToNow(): void } | undefined = $state();

  const noKey = (e: unknown): boolean => e instanceof Error && e.name === 'ErrNoKey';
  const failure = (e: unknown, what: string): string => (noKey(e) ? 'Import your key first.' : `Could not load ${what}.`);

  // ---- data ------------------------------------------------------------------

  async function refreshStatus(): Promise<void> {
    try {
      status = await client.call<Status>({ type: 'api.get', path: '/api/v1/status', query: {} });
    } catch {
      status = null;
    }
  }

  onMount(() => {
    let alive = true;
    client
      .call<Camera[]>({ type: 'api.get', path: '/api/v1/cameras', query: {} })
      .then((c) => {
        if (alive) cameras = c;
      })
      .catch((e: unknown) => {
        if (alive) error = failure(e, 'cameras');
      });
    void refreshStatus();
    const poll = setInterval(() => void refreshStatus(), 60_000);
    const clock = setInterval(() => (wall = Date.now()), 30_000);
    return () => {
      alive = false;
      clearInterval(poll);
      clearInterval(clock);
    };
  });

  let loadGen = 0;
  $effect(() => {
    if (cameras === null) return;
    const q = { day, zone, camera, trigger, cameras: cameras.map((c) => c.id) };
    const gen = ++loadGen;
    loading = true;
    loadDay(client, q)
      .then((d) => {
        if (gen !== loadGen) return;
        clips = d.clips;
        coverage = d.coverage;
        invalid = d.invalid;
        error = '';
      })
      .catch((e: unknown) => {
        if (gen === loadGen) error = failure(e, 'recordings');
      })
      .finally(() => {
        if (gen === loadGen) loading = false;
      });
  });

  /** Pick the display zone; the view moves to "today" there, which can be another date. */
  function setZone(z: string | null): void {
    forcedZone = z;
    saveZonePref(z);
    day = localDayOf(now, z ?? ownZone);
  }

  // ---- derived views -----------------------------------------------------------

  const timelineClips = $derived(clips.map(toTimelineClip).sort((a, b) => a.startMs - b.startMs));
  const rows: EventRow[] = $derived(
    [...clips]
      .sort((a, b) => Date.parse(b.start_time) - Date.parse(a.start_time))
      .map((c) => ({
        id: c.clip_id,
        cameraName: c.camera.name,
        startMs: Date.parse(c.start_time),
        durationMs: c.duration_ms,
        triggers: [...new Set(c.triggers.map((t) => t.type))],
        status: c.status,
      })),
  );
  const lanes = $derived((cameras ?? []).filter((c) => !camera || c.id === camera));

  // ---- selection and playback ------------------------------------------------------

  function select(clipId: string, offsetMs = 0): void {
    selectedId = clipId;
    cue = { clipId, offsetMs, n: ++cueN };
  }

  function onPlayerState(s: PlayerState): void {
    if (s.clipId === null) return;
    // Auto-advance moves the player on by itself: the selection follows it.
    if (s.clipId !== selectedId) selectedId = s.clipId;
    if (s.integrity !== 'verifying' && integrity[s.clipId] !== s.integrity) integrity[s.clipId] = s.integrity;
  }

  async function run(cmd: Command): Promise<void> {
    switch (cmd) {
      case 'toggle':
        playerView?.togglePlay();
        break;
      case 'back10':
        playerView?.skip(-10_000);
        break;
      case 'fwd10':
        playerView?.skip(10_000);
        break;
      case 'prev':
      case 'next': {
        const id = adjacentClip(timelineClips, selectedId, cmd);
        if (id !== null && id !== selectedId) select(id);
        break;
      }
      case 'zoomIn':
        timeline?.stepZoom('in');
        break;
      case 'zoomOut':
        timeline?.stepZoom('out');
        break;
      case 'now': {
        const today = localDayOf(now, zone);
        if (day !== today) {
          day = today;
          // The timeline shows the whole new day first; centre on now after it has.
          await tick();
        }
        timeline?.jumpToNow();
        break;
      }
    }
  }

  function onkeydown(e: KeyboardEvent): void {
    // A modal dialog (the export confirmation) owns the keyboard: nothing behind it may react.
    if (document.querySelector('[aria-modal="true"]')) return;
    const cmd = commandFor(e);
    if (cmd === null) return;
    e.preventDefault();
    void run(cmd);
  }

  function lock(): void {
    void client.call({ type: 'key.lock' }).catch(() => {});
  }

  const btn = 'rounded border border-neutral-600 px-3 py-2 text-sm md:py-1.5';
</script>

<svelte:window {onkeydown} />

<div data-testid="timeline-page" class="flex min-h-dvh flex-col">
  <Header
    cameras={cameras ?? []}
    {camera}
    {day}
    {trigger}
    {status}
    browserZone={ownZone}
    {forcedZone}
    nowMs={now}
    onCamera={(id) => (camera = id)}
    onDay={(d) => (day = d)}
    onTrigger={(t) => (trigger = t)}
    onZone={setZone}
    onLock={lock}
  />

  {#if error}
    <p role="alert" class="px-4 py-3 text-sm text-red-400">
      {error}
      {#if error.startsWith('Import')}<a href="#/key" class="underline">Open keys</a>{/if}
    </p>
  {/if}
  {#if invalid > 0}
    <p role="status" class="px-4 py-1 text-xs text-amber-300">
      {invalid} {invalid === 1 ? 'recording' : 'recordings'} could not be read and {invalid === 1 ? 'is' : 'are'} not shown.
    </p>
  {/if}

  <div class="grid flex-1 gap-3 p-3 pb-16 md:grid-cols-[minmax(0,1fr)_22rem] md:pb-3">
    <div class="min-w-0">
      <!-- On a phone the player stays pinned at the top while the page scrolls. -->
      <div class="sticky top-0 z-10 bg-[var(--bg,#0a0a0a)] pb-2 md:static">
        <Player bind:this={playerView} {client} {cue} {zone} {profile} {factory} env={exportEnv} onState={onPlayerState} />
      </div>

      <div class="mt-2 flex flex-wrap items-center gap-2" role="group" aria-label="Transport">
        <button type="button" class={btn} title="Previous clip (J)" onclick={() => run('prev')}>Prev</button>
        <button type="button" class={btn} title="Play or pause (Space)" onclick={() => run('toggle')}>Play/Pause</button>
        <button type="button" class={btn} title="Next clip (K)" onclick={() => run('next')}>Next</button>
        <button type="button" class={btn} title="Back 10 seconds (Left)" onclick={() => run('back10')}>-10 s</button>
        <button type="button" class={btn} title="Forward 10 seconds (Right)" onclick={() => run('fwd10')}>+10 s</button>
        <span class="mx-1 hidden h-5 border-l border-neutral-700 md:inline"></span>
        <button type="button" class={btn} title="Zoom out ([)" onclick={() => run('zoomOut')}>Zoom out</button>
        <button type="button" class={btn} title="Zoom in (])" onclick={() => run('zoomIn')}>Zoom in</button>
        <button type="button" class={btn} title="Jump to now (T)" onclick={() => run('now')}>Now</button>
        {#if loading}<span class="text-xs text-neutral-400" role="status">Loading…</span>{/if}
      </div>

      <!-- Bigger touch targets on a phone: the canvas sits in a padded, taller block. -->
      <div class="mt-3 rounded-lg border border-neutral-800 p-2 max-md:py-4">
        <Timeline
          bind:this={timeline}
          cameras={lanes.map((c) => ({ id: c.id, name: c.name }))}
          clips={timelineClips}
          {coverage}
          {day}
          {zone}
          nowMs={now}
          selectedClipId={selectedId}
          onSelect={(id, offset) => select(id, offset)}
        />
      </div>
    </div>

    <!-- A sidebar on desktop, a bottom sheet on a phone. -->
    <aside
      aria-label="Recordings"
      class="fixed inset-x-0 bottom-0 z-20 flex flex-col border-t border-neutral-700 bg-neutral-950 md:static md:z-auto md:h-[calc(100dvh-7rem)] md:rounded-lg md:border md:border-neutral-800"
    >
      <button
        type="button"
        class="px-4 py-3 text-left text-sm font-medium md:hidden"
        aria-expanded={sheetOpen}
        onclick={() => (sheetOpen = !sheetOpen)}>Events ({rows.length}) {sheetOpen ? '▾' : '▴'}</button
      >
      <div class="min-h-0 flex-1 max-md:h-[45dvh] {sheetOpen ? '' : 'max-md:hidden'}">
        <EventList clips={rows} {selectedId} {zone} {integrity} onSelect={(id) => select(id)} />
      </div>
    </aside>
  </div>
</div>
