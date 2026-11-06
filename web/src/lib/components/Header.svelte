<script lang="ts">
  import type { Status } from '../../worker/schemas';
  import { zoneChoices } from '../timeline/zonePref';
  import StatusDot from './StatusDot.svelte';

  interface Props {
    cameras: readonly { id: string; name: string }[];
    /** '' means all cameras. */
    camera: string;
    /** YYYY-MM-DD in the display zone. */
    day: string;
    /** '' means all triggers. */
    trigger: string;
    status: Status | null;
    /** The viewer's own zone, offered as the default choice. */
    browserZone: string;
    /** The forced display zone; null follows the viewer's own. */
    forcedZone: string | null;
    nowMs?: number;
    onCamera: (id: string) => void;
    onDay: (day: string) => void;
    onTrigger: (type: string) => void;
    /** A zone name, or null to follow the viewer's own zone. */
    onZone: (zone: string | null) => void;
    onLock: () => void;
  }

  let { cameras, camera, day, trigger, status, browserZone, forcedZone, nowMs, onCamera, onDay, onTrigger, onZone, onLock }: Props = $props();

  const zones = $derived(zoneChoices(forcedZone));

  // On a phone the controls fold into a menu.
  let menuOpen = $state(false);

  const TRIGGERS = [
    ['', 'All triggers'],
    ['person', 'Person'],
    ['vehicle', 'Vehicle'],
    ['pet', 'Pet'],
    ['motion', 'Motion'],
    ['other', 'Other'],
  ] as const;

  const field = 'rounded border border-neutral-700 bg-neutral-900 px-2 py-1.5 text-sm';
</script>

<header class="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-neutral-800 px-3 py-2">
  <h1 class="text-base font-semibold">Timeline</h1>
  <StatusDot {status} {nowMs} />
  <button
    type="button"
    class="ml-auto rounded border border-neutral-600 px-3 py-1.5 text-sm md:hidden"
    aria-expanded={menuOpen}
    aria-controls="header-controls"
    onclick={() => (menuOpen = !menuOpen)}>Menu</button
  >
  <div id="header-controls" class="w-full flex-wrap items-center gap-3 md:ml-4 md:flex md:w-auto md:flex-1 {menuOpen ? 'flex' : 'hidden'}">
    <label class="flex items-center gap-2 text-sm">
      <span class="text-neutral-400">Camera</span>
      <select class={field} value={camera} onchange={(e) => onCamera(e.currentTarget.value)}>
        <option value="">All cameras</option>
        {#each cameras as c (c.id)}
          <option value={c.id}>{c.name}</option>
        {/each}
      </select>
    </label>
    <label class="flex items-center gap-2 text-sm">
      <span class="text-neutral-400">Date</span>
      <input
        type="date"
        class={field}
        value={day}
        onchange={(e) => {
          // A cleared date field gives '': keep the day that is showing.
          if (e.currentTarget.value) onDay(e.currentTarget.value);
        }}
      />
    </label>
    <label class="flex items-center gap-2 text-sm">
      <span class="text-neutral-400">Trigger</span>
      <select class={field} value={trigger} onchange={(e) => onTrigger(e.currentTarget.value)}>
        {#each TRIGGERS as [value, label] (value)}
          <option {value}>{label}</option>
        {/each}
      </select>
    </label>
    <label class="flex items-center gap-2 text-sm">
      <span class="text-neutral-400">Time zone</span>
      <select class={field} value={forcedZone ?? ''} onchange={(e) => onZone(e.currentTarget.value || null)}>
        <option value="">My time ({browserZone})</option>
        {#each zones as z (z)}
          <option value={z}>{z}</option>
        {/each}
      </select>
    </label>
    <div class="flex items-center gap-2 md:ml-auto">
      <a href="#/key" class="rounded border border-neutral-600 px-3 py-1.5 text-sm">Keys</a>
      <button type="button" class="rounded border border-neutral-600 px-3 py-1.5 text-sm" onclick={onLock}>Lock</button>
    </div>
  </div>
</header>
