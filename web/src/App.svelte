<script lang="ts">
  import Onboarding from './routes/onboarding/Onboarding.svelte';
  import LivePage from './routes/live/+page.svelte';

  // Top-level views (design §7.2). Timeline joins them when its page lands.
  const views = { '#/live': 'Live', '#/key': 'Key' } as const;
  type Hash = keyof typeof views;

  const current = (): Hash => (location.hash === '#/live' ? '#/live' : '#/key');
  let hash: Hash = $state(current());
</script>

<svelte:window onhashchange={() => (hash = current())} />

<nav aria-label="Views" class="flex gap-4 border-b border-neutral-800 px-4 py-2 text-sm">
  <strong>zview</strong>
  {#each Object.entries(views) as [h, label] (h)}
    <a href={h} aria-current={hash === h ? 'page' : undefined} class={hash === h ? 'underline' : 'text-neutral-400'}>{label}</a>
  {/each}
</nav>
{#if hash === '#/live'}
  <LivePage />
{:else}
  <Onboarding />
{/if}
