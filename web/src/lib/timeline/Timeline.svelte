<script lang="ts">
  import { onMount } from 'svelte';
  import {
    gapAt,
    hitTest,
    LANE_GAP,
    LANE_HEIGHT,
    layoutLanes,
    RULER_HEIGHT,
    xToTime,
    type Coverage,
    type GapRect,
    type Layout,
    type TimelineCamera,
    type TimelineClip,
  } from './model';
  import { backingSize, createScheduler, drawTimeline, type Scheduler } from './render';
  import { browserZone, localDayRange } from './timezone';
  import { fullView, panBy, stepPreset, zoomAround, type View } from './zoom';

  interface Props {
    cameras: TimelineCamera[];
    /** Sorted by start time; end times resolved by the caller. */
    clips: TimelineClip[];
    /** Ready intervals; the complement is drawn as not monitored. */
    coverage: Coverage[];
    /** Calendar day, YYYY-MM-DD, in `zone`. */
    day: string;
    /** Display zone; defaults to the viewer's own. */
    zone?: string;
    /** Test hook and "now" override; defaults to the wall clock. */
    nowMs?: number;
    onSelect: (clipId: string, offsetMs: number) => void;
    /** Reports the CPU cost (ms) of each drawn frame. */
    onFrame?: (ms: number) => void;
  }

  let { cameras, clips, coverage, day, zone = browserZone(), nowMs, onSelect, onFrame }: Props = $props();

  const range = $derived(localDayRange(day, zone));
  // Plain copies: reading a deep reactive proxy for every clip on every frame is slow.
  const data = $derived({
    cameras: $state.snapshot(cameras),
    clips: $state.snapshot(clips),
    coverage: $state.snapshot(coverage),
  });
  const cssHeight = $derived(RULER_HEIGHT + Math.max(cameras.length, 1) * (LANE_HEIGHT + LANE_GAP));

  let canvas: HTMLCanvasElement;
  let cssWidth = 0;
  let layout: Layout = { width: 0, height: 0, view: { startMs: 0, spanMs: 1 }, lanes: [] };

  // Imperative drawing state: changing it never re-renders markup.
  let view: View = fullView([0, 1]);
  let selectedId: string | null = null;
  let hoverGap: GapRect | null = null;
  let hoverX = 0;
  let hoverY = 0;
  let sched: Scheduler | undefined;

  export function getLayout(): Layout {
    return layout;
  }

  function currentNow(): number {
    return nowMs ?? Date.now();
  }

  function draw(): void {
    const ctx = canvas.getContext('2d');
    if (!ctx || cssWidth <= 0) return;
    const t0 = performance.now();
    layout = layoutLanes({ ...data, range, view, width: cssWidth, nowMs: currentNow() });
    drawTimeline(ctx, layout, {
      zone,
      nowMs: currentNow(),
      selectedClipId: selectedId,
      hoverGap,
      hoverX,
      hoverY,
    });
    onFrame?.(performance.now() - t0);
  }

  function resize(): void {
    const dpr = window.devicePixelRatio || 1;
    const size = backingSize(cssWidth, cssHeight, dpr);
    canvas.width = size.width;
    canvas.height = size.height;
    canvas.getContext('2d')?.setTransform(size.width / Math.max(cssWidth, 1), 0, 0, size.height / cssHeight, 0, 0);
    sched?.invalidate();
  }

  // A new day or zone shows the whole day again.
  $effect(() => {
    view = fullView(range);
    sched?.invalidate();
  });

  // Any change of data repaints.
  $effect(() => {
    void [data, nowMs, zone];
    sched?.invalidate();
  });

  $effect(() => {
    void cssHeight;
    if (canvas && cssWidth > 0) resize();
  });

  onMount(() => {
    sched = createScheduler(draw);
    const ro = new ResizeObserver(([entry]) => {
      cssWidth = entry?.contentRect.width ?? canvas.clientWidth;
      resize();
    });
    ro.observe(canvas);
    cssWidth = canvas.clientWidth;
    resize();
    // The now line moves; repaint it every 30 s while the day contains it.
    const tick = setInterval(() => {
      const n = currentNow();
      if (n >= range[0] && n < range[1]) sched?.invalidate();
    }, 30_000);
    return () => {
      clearInterval(tick);
      ro.disconnect();
      sched?.dispose();
    };
  });

  function setView(v: View): void {
    view = v;
    sched?.invalidate();
  }

  function local(e: { clientX: number; clientY: number }): { x: number; y: number } {
    const r = canvas.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  }

  // Imperative gesture bookkeeping, never rendered, so a plain Map is right.
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const pointers = new Map<number, { x: number; y: number }>();
  let dragMoved = 0;
  let pinchDist = 0;

  function dist(): number {
    const [a, b] = [...pointers.values()];
    return a && b ? Math.hypot(a.x - b.x, a.y - b.y) : 0;
  }

  function onpointerdown(e: PointerEvent): void {
    pointers.set(e.pointerId, local(e));
    try {
      canvas.setPointerCapture(e.pointerId);
    } catch {
      // Synthetic or already-released pointers cannot be captured; dragging still works.
    }
    if (pointers.size === 1) dragMoved = 0;
    if (pointers.size === 2) {
      pinchDist = dist();
      dragMoved = Infinity; // a pinch is never a click
    }
  }

  function onpointermove(e: PointerEvent): void {
    const prev = pointers.get(e.pointerId);
    const p = local(e);
    if (!prev) {
      const g = gapAt(layout, p.x, p.y);
      if (g !== hoverGap || g) {
        hoverGap = g;
        hoverX = p.x;
        hoverY = p.y;
        sched?.invalidate();
      }
      return;
    }
    pointers.set(e.pointerId, p);
    if (pointers.size === 2) {
      const d = dist();
      if (pinchDist > 0 && d > 0) {
        const [a, b] = [...pointers.values()];
        const mid = ((a?.x ?? 0) + (b?.x ?? 0)) / 2;
        setView(zoomAround(view, pinchDist / d, xToTime(view, cssWidth, mid), range));
      }
      pinchDist = d;
      return;
    }
    const dx = p.x - prev.x;
    dragMoved += Math.abs(dx) + Math.abs(p.y - prev.y);
    if (dx !== 0) setView(panBy(view, (-dx / cssWidth) * view.spanMs, range));
  }

  function onpointerup(e: PointerEvent): void {
    const had = pointers.delete(e.pointerId);
    if (!had) return;
    if (pointers.size === 0 && dragMoved <= 4) {
      const p = local(e);
      const hit = hitTest(layout, p.x, p.y);
      if (hit) {
        selectedId = hit.clipId;
        sched?.invalidate();
        onSelect(hit.clipId, hit.offsetMs);
      }
    }
    pinchDist = 0;
  }

  function onpointercancel(e: PointerEvent): void {
    pointers.delete(e.pointerId);
    pinchDist = 0;
  }

  function onpointerleave(): void {
    if (hoverGap) {
      hoverGap = null;
      sched?.invalidate();
    }
  }

  function onwheel(e: WheelEvent): void {
    e.preventDefault();
    const scale = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 400 : 1;
    if (Math.abs(e.deltaX) > Math.abs(e.deltaY) && !e.ctrlKey) {
      setView(panBy(view, ((e.deltaX * scale) / cssWidth) * view.spanMs, range));
      return;
    }
    // ctrlKey is a trackpad pinch: those deltas are small, so weight them more.
    const k = e.ctrlKey ? 0.01 : 0.0015;
    const factor = Math.exp(e.deltaY * scale * k);
    setView(zoomAround(view, factor, xToTime(view, cssWidth, local(e).x), range));
  }

  function ondblclick(e: MouseEvent): void {
    setView(stepPreset(view, 'in', xToTime(view, cssWidth, local(e).x), range));
  }

  function onkeydown(e: KeyboardEvent): void {
    const center = view.startMs + view.spanMs / 2;
    switch (e.key) {
      case 'ArrowLeft':
        setView(panBy(view, -view.spanMs * 0.1, range));
        break;
      case 'ArrowRight':
        setView(panBy(view, view.spanMs * 0.1, range));
        break;
      case '+':
      case '=':
        setView(stepPreset(view, 'in', center, range));
        break;
      case '-':
        setView(stepPreset(view, 'out', center, range));
        break;
      case 'Home':
        setView(fullView(range));
        break;
      default:
        return;
    }
    e.preventDefault();
  }

  // Wheel must be non-passive to stop the page scrolling while zooming.
  function wheelAction(node: HTMLCanvasElement) {
    node.addEventListener('wheel', onwheel, { passive: false });
    return { destroy: () => node.removeEventListener('wheel', onwheel) };
  }
</script>

<!-- A keyboard-operable canvas widget: role=application is the honest role. -->
<!-- svelte-ignore a11y_no_interactive_element_to_noninteractive_role -->
<canvas
  bind:this={canvas}
  use:wheelAction
  style:height="{cssHeight}px"
  tabindex="0"
  role="application"
  aria-label="Event timeline. Arrow keys pan, plus and minus zoom, Home shows the whole day."
  {onpointerdown}
  {onpointermove}
  {onpointerup}
  {onpointercancel}
  {onpointerleave}
  {ondblclick}
  {onkeydown}
></canvas>

<style>
  /* Self-contained: the component must size correctly without the app stylesheet. */
  canvas {
    display: block;
    width: 100%;
    touch-action: none;
    user-select: none;
    cursor: grab;
  }
  canvas:active {
    cursor: grabbing;
  }
  canvas:focus-visible {
    outline: 2px solid #38bdf8;
    outline-offset: -2px;
  }
</style>
