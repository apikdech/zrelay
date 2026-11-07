<script lang="ts">
  import Onboarding from './routes/onboarding/Onboarding.svelte';
  import TimelinePage from './routes/+page.svelte';
  import LivePage from './routes/live/+page.svelte';
  import DemoPage from './routes/demo/+page.svelte';
  import UpdatePrompt from './lib/pwa/UpdatePrompt.svelte';
  import type { Updater } from './lib/pwa/update';
  import { onMount } from 'svelte';
  import { workerClient } from './lib/workerClient';
  import { acquireIdleLock, type LockClient } from './routes/onboarding/idleService';

  /** The service worker's update state; null where there is no service worker. */
  let { updater = null, client = workerClient }: { updater?: Updater | null; client?: LockClient } = $props();

  // Idle auto-lock (ui-spec §4.1) applies on every view, not only the Key page.
  onMount(() => {
    const idle = acquireIdleLock(client);
    return () => idle.release();
  });

  // Top-level views (design §7.2). The timeline is the home view.
  // `#/demo?clip=<id>` is the playback engine's demo page.
  const views = { '#/timeline': 'Timeline', '#/live': 'Live', '#/demo': 'Demo', '#/key': 'Key' } as const;
  type Hash = keyof typeof views;

  /** The route part of the hash, without its query. */
  const route = (): Hash => {
    const r = location.hash.split('?')[0];
    return r === '#/live' || r === '#/demo' || r === '#/key' ? r : '#/timeline';
  };
  const clip = (): string => new URLSearchParams(location.hash.split('?')[1] ?? '').get('clip') ?? '';

  let hash: Hash = $state(route());
  let clipId = $state(clip());
</script>

<svelte:window
  onhashchange={() => {
    hash = route();
    clipId = clip();
  }}
/>

<UpdatePrompt {updater} />

<nav aria-label="Views" class="flex gap-4 border-b border-neutral-800 px-4 py-2 text-sm">
  <strong>zview</strong>
  {#each Object.entries(views) as [h, label] (h)}
    <a href={h} aria-current={hash === h ? 'page' : undefined} class={hash === h ? 'underline' : 'text-neutral-400'}>{label}</a>
  {/each}
</nav>
{#if hash === '#/live'}
  <LivePage />
{:else if hash === '#/demo'}
  <DemoPage {clipId} />
{:else if hash === '#/key'}
  <Onboarding />
{:else}
  <TimelinePage />
{/if}
