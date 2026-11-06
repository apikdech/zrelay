<script lang="ts">
  import type { IntegrityState } from '../player/engine';

  interface Props {
    /** Undefined or 'verifying' while nothing is known yet: nothing is drawn. */
    state?: IntegrityState;
  }

  let { state }: Props = $props();

  // ui-spec §4.4 wording; the glyphs are text, so they follow the theme.
  const BADGES: Record<Exclude<IntegrityState, 'verifying'>, { glyph: string; label: string; cls: string }> = {
    verified: { glyph: '✓', label: 'Encrypted recording verified', cls: 'text-emerald-400' },
    mismatch: { glyph: '⚠', label: 'Metadata mismatch', cls: 'text-amber-400' },
    tampered: { glyph: '⛔', label: 'Failed verification', cls: 'text-red-400' },
    incomplete: { glyph: '◐', label: 'Recording was interrupted', cls: 'text-amber-300' },
    'unknown-key': { glyph: '⚿', label: 'Unknown key', cls: 'text-sky-300' },
  };

  const badge = $derived(state && state !== 'verifying' ? BADGES[state] : null);
</script>

{#if badge && state}
  <span role="img" data-integrity={state} aria-label={badge.label} title={badge.label} class="inline-block w-4 text-center {badge.cls}">{badge.glyph}</span>
{/if}
