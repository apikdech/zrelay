<script lang="ts">
  import { relayHealth } from '../relayStatus';
  import type { Status } from '../../worker/schemas';

  interface Props {
    /** The last `/api/v1/status` answer; null when it could not be read. */
    status: Status | null;
    /** "Now" for the staleness rule; defaults to the wall clock. */
    nowMs?: number;
  }

  let { status, nowMs }: Props = $props();

  const health = $derived(relayHealth(status, nowMs ?? Date.now()));
  const color = $derived(
    { ok: 'bg-emerald-500', warn: 'bg-amber-400', bad: 'bg-red-500', unknown: 'bg-neutral-500' }[health.level],
  );
</script>

<span
  role="img"
  data-testid="relay-dot"
  data-level={health.level}
  title={health.tooltip}
  aria-label={`Relay: ${health.summary}`}
  class="inline-block h-3 w-3 shrink-0 rounded-full {color}"
></span>
