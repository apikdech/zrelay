<script lang="ts" module>
  export interface EventRow {
    id: string;
    cameraName: string;
    startMs: number;
    /** null while unknown (an interrupted clip). */
    durationMs: number | null;
    /** Distinct trigger types in firing order. */
    triggers: string[];
    status: 'complete' | 'incomplete';
  }

  /** Row pitch in CSS px; the list positions rows by it. */
  export const ROW_HEIGHT = 56;
  const OVERSCAN = 6;
  /** Viewport height assumed until the element reports its own. */
  const FALLBACK_VIEW = 480;

  export function formatDuration(ms: number | null): string {
    if (ms === null) return '—';
    const total = Math.round(ms / 1000);
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    const ss = String(s).padStart(2, '0');
    return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
  }
</script>

<script lang="ts">
  import { formatClock } from '../timeline/timezone';
  import { triggerStyle } from '../timeline/model';
  import type { IntegrityState } from '../player/engine';
  import IntegrityBadge from './IntegrityBadge.svelte';

  interface Props {
    /** Newest first. */
    clips: readonly EventRow[];
    selectedId: string | null;
    zone: string;
    /** What is known about each clip's integrity; absent means not known yet. */
    integrity: Readonly<Record<string, IntegrityState | undefined>>;
    onSelect: (clipId: string) => void;
  }

  let { clips, selectedId, zone, integrity, onSelect }: Props = $props();

  let scroller: HTMLDivElement;
  let scrollTop = $state(0);
  let viewHeight = $state(0);

  const view = $derived(viewHeight || FALLBACK_VIEW);
  const first = $derived(Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN));
  const last = $derived(Math.min(clips.length, Math.ceil((scrollTop + view) / ROW_HEIGHT) + OVERSCAN));
  const visible = $derived(clips.slice(first, last));

  // Bring a selection made elsewhere (J/K, the timeline) into view.
  $effect(() => {
    if (selectedId === null || !scroller) return;
    const i = clips.findIndex((c) => c.id === selectedId);
    if (i < 0) return;
    const top = i * ROW_HEIGHT;
    const st = scroller.scrollTop;
    if (top >= st && top + ROW_HEIGHT <= st + view) return;
    const target = Math.max(0, top - (view - ROW_HEIGHT) / 2);
    scroller.scrollTop = target;
    scrollTop = target;
  });
</script>

<div
  bind:this={scroller}
  bind:clientHeight={viewHeight}
  role="listbox"
  aria-label="Recordings, newest first"
  tabindex="-1"
  onscroll={() => (scrollTop = scroller.scrollTop)}
  class="relative h-full overflow-y-auto overscroll-contain"
>
  {#if clips.length === 0}
    <p class="p-4 text-sm text-neutral-400">No recordings in this view.</p>
  {:else}
    <div style:height="{clips.length * ROW_HEIGHT}px" class="relative">
      {#each visible as c, k (c.id)}
        {@const i = first + k}
        {@const sel = c.id === selectedId}
        <div
          role="option"
          aria-selected={sel}
          aria-posinset={i + 1}
          aria-setsize={clips.length}
          data-clip={c.id}
          tabindex={sel ? 0 : -1}
          style:top="{i * ROW_HEIGHT}px"
          style:height="{ROW_HEIGHT}px"
          onclick={() => onSelect(c.id)}
          onkeydown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              onSelect(c.id);
            }
          }}
          class="absolute inset-x-0 flex cursor-pointer items-center gap-3 border-b border-neutral-800 px-3 text-sm hover:bg-neutral-800/60 {sel
            ? 'bg-sky-900/40'
            : ''}"
        >
          <div class="min-w-0 flex-1">
            <div class="flex items-baseline gap-2">
              <span class="font-mono tabular-nums">{formatClock(c.startMs, zone, true)}</span>
              <span class="text-xs text-neutral-400">{formatDuration(c.durationMs)}</span>
            </div>
            <div class="truncate text-xs text-neutral-400">{c.cameraName}</div>
          </div>
          <div class="flex items-center gap-1">
            {#each c.triggers as t (t)}
              {@const st = triggerStyle(t)}
              <span role="img" aria-label={st.label} title={st.label} style:color={st.color} class="w-4 text-center text-xs font-bold">{st.glyph}</span>
            {/each}
          </div>
          <IntegrityBadge state={integrity[c.id] ?? (c.status === 'incomplete' ? 'incomplete' : undefined)} />
        </div>
      {/each}
    </div>
  {/if}
</div>
