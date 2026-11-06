// Test page for e2e/timeline.spec.ts. `?data=small` mounts one known clip;
// `?data=perf` mounts 4 cameras x 500 synthetic clips plus coverage gaps.
// The page publishes window.__tl for the spec.
import { mount } from 'svelte';
import Timeline from '../../src/lib/timeline/Timeline.svelte';
import { localDayRange } from '../../src/lib/timeline/timezone';
import type { Coverage, Layout, TimelineCamera, TimelineClip } from '../../src/lib/timeline/model';

const ZONE = 'Asia/Jakarta';
const DAY = '2026-11-06';
const [from, to] = localDayRange(DAY, ZONE);
const H = 3_600_000;
const TYPES = ['motion', 'person', 'vehicle', 'pet', 'other'];

// Small deterministic PRNG so every run draws the same data.
function rng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0;
    return s / 2 ** 32;
  };
}

function smallData() {
  const cameras: TimelineCamera[] = [{ id: 'front', name: 'Front Door' }];
  const clips: TimelineClip[] = [
    {
      id: 'c1',
      cameraId: 'front',
      startMs: from + 12 * H,
      endMs: from + 12 * H + 30 * 60_000,
      triggers: [{ type: 'person', firstAtMs: 4200, lastAtMs: 10 * 60_000 }],
    },
  ];
  const coverage: Coverage[] = [{ cameraId: 'front', fromMs: from, toMs: from + 10 * H }];
  return { cameras, clips, coverage };
}

function perfData() {
  const rand = rng(37);
  const cameras: TimelineCamera[] = ['front', 'back', 'garage', 'yard'].map((id) => ({ id, name: id }));
  const clips: TimelineClip[] = [];
  const coverage: Coverage[] = [];
  for (const cam of cameras) {
    for (let i = 0; i < 500; i++) {
      const startMs = from + Math.floor(rand() * 23.9 * H);
      const dur = 20_000 + Math.floor(rand() * 4 * 60_000);
      const n = 1 + Math.floor(rand() * 3);
      const triggers = Array.from({ length: n }, () => {
        const a = Math.floor(rand() * dur * 0.6);
        return { type: TYPES[Math.floor(rand() * TYPES.length)]!, firstAtMs: a, lastAtMs: a + Math.floor(rand() * (dur - a)) };
      });
      clips.push({ id: `${cam.id}-${i}`, cameraId: cam.id, startMs, endMs: startMs + dur, triggers });
    }
    clips.sort((a, b) => a.startMs - b.startMs);
    // Ready for most of the day with a dozen gaps.
    let t = from;
    for (let g = 0; g < 12; g++) {
      const gapAt = from + Math.floor(rand() * 23 * H);
      if (gapAt > t) coverage.push({ cameraId: cam.id, fromMs: t, toMs: gapAt });
      t = gapAt + 5 * 60_000 + Math.floor(rand() * 40 * 60_000);
    }
    coverage.push({ cameraId: cam.id, fromMs: t, toMs: to });
  }
  return { cameras, clips, coverage };
}

interface PerfResult {
  frames: number;
  avgDrawMs: number;
  maxDrawMs: number;
  avgIntervalMs: number;
}

declare global {
  interface Window {
    __tl: {
      selected: { clipId: string; offsetMs: number }[];
      draws: number;
      layout(): Layout;
      runPerf(ms: number): Promise<PerfResult>;
    };
  }
}

const data = new URLSearchParams(location.search).get('data') === 'perf' ? perfData() : smallData();
const costs: number[] = [];
const state: Window['__tl'] = {
  selected: [],
  draws: 0,
  layout: () => comp.getLayout(),
  runPerf,
};
window.__tl = state;

const comp = mount(Timeline, {
  target: document.getElementById('app')!,
  props: {
    ...data,
    day: DAY,
    zone: ZONE,
    nowMs: from + 15 * H,
    onSelect: (clipId: string, offsetMs: number) => state.selected.push({ clipId, offsetMs }),
    onFrame: (ms: number) => {
      state.draws++;
      costs.push(ms);
    },
  },
}) as { getLayout(): Layout };

// Scripted pan/zoom: every animation frame dispatches either a wheel zoom
// (alternating in and out around a moving anchor) or a drag step, through
// the same listeners a real pointer would hit.
function runPerf(durationMs: number): Promise<PerfResult> {
  const canvas = document.querySelector('canvas')!;
  const r = canvas.getBoundingClientRect();
  costs.length = 0;
  let last = performance.now();
  const start = last;
  const intervals: number[] = [];
  let frame = 0;
  let dragging = false;
  return new Promise((resolve) => {
    const step = (now: number) => {
      intervals.push(now - last);
      last = now;
      const phase = Math.floor(frame / 30) % 4;
      const x = r.left + r.width * (0.2 + 0.6 * ((frame % 90) / 90));
      const y = r.top + 40;
      if (phase === 0 || phase === 2) {
        canvas.dispatchEvent(
          new WheelEvent('wheel', { deltaY: phase === 0 ? -40 : 40, clientX: x, clientY: y, bubbles: true, cancelable: true }),
        );
      } else {
        const ev = (type: string, cx: number) =>
          new PointerEvent(type, { pointerId: 7, pointerType: 'mouse', clientX: cx, clientY: y, buttons: 1, bubbles: true });
        if (!dragging) {
          canvas.dispatchEvent(ev('pointerdown', x));
          dragging = true;
        }
        canvas.dispatchEvent(ev('pointermove', x + ((frame % 30) - 15) * 6));
        if (frame % 30 === 29) {
          canvas.dispatchEvent(ev('pointerup', x));
          dragging = false;
        }
      }
      frame++;
      if (now - start < durationMs) requestAnimationFrame(step);
      else {
        if (dragging) canvas.dispatchEvent(new PointerEvent('pointerup', { pointerId: 7, pointerType: 'mouse', bubbles: true }));
        const sum = costs.reduce((a, b) => a + b, 0);
        resolve({
          frames: costs.length,
          avgDrawMs: costs.length ? sum / costs.length : 0,
          maxDrawMs: costs.reduce((a, b) => Math.max(a, b), 0),
          avgIntervalMs: intervals.reduce((a, b) => a + b, 0) / intervals.length,
        });
      }
    };
    requestAnimationFrame(step);
  });
}
