// One live tile's lifecycle: opens a WHEP session, reconnects with backoff,
// closes while the tab is hidden and reopens when it is visible again.
import { startWhep, type WhepClient, type WhepSession } from './whep';

export type TileState = 'connecting' | 'live' | 'reconnecting' | 'limit';

export interface TileSnapshot {
  state: TileState;
  /** An audio track exists, so unmuting is possible. */
  canUnmute: boolean;
  muted: boolean;
}

export interface TileOptions {
  video: HTMLVideoElement;
  cameraId: string;
  client: WhepClient;
  /** Visibility source; defaults to `document`. */
  doc?: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>;
  onChange(s: TileSnapshot): void;
}

export interface Tile {
  start(): void;
  destroy(): void;
  toggleMute(): void;
  snapshot(): TileSnapshot;
}

const BASE_DELAY_MS = 1000;
const MAX_DELAY_MS = 30_000;

/** Reconnect delay before attempt `n` (0-based): 1 s doubling up to 30 s. */
export function backoffDelay(n: number): number {
  return Math.min(MAX_DELAY_MS, BASE_DELAY_MS * 2 ** Math.min(n, 16));
}

export function createTile(opts: TileOptions): Tile {
  const { video, cameraId, client } = opts;
  const doc = opts.doc ?? document;
  let state: TileState = 'connecting';
  let canUnmute = false;
  let session: WhepSession | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let attempt = 0;
  // Bumped whenever the current attempt is superseded (hide, destroy, new attempt).
  let epoch = 0;
  let running = false;
  video.muted = true;

  const snapshot = (): TileSnapshot => ({ state, canUnmute, muted: video.muted });
  const emit = () => opts.onChange(snapshot());
  const setState = (s: TileState) => {
    if (s === state) return;
    state = s;
    emit();
  };
  const visible = () => doc.visibilityState !== 'hidden';

  function clearTimer() {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function schedule(next: TileState) {
    setState(next);
    clearTimer();
    const mine = epoch;
    timer = setTimeout(() => {
      timer = null;
      if (mine === epoch) void open();
    }, backoffDelay(attempt));
    attempt++;
  }

  async function open() {
    if (!running || !visible()) return;
    const mine = ++epoch;
    let s: WhepSession;
    try {
      s = await startWhep(video, cameraId, client);
    } catch (e) {
      if (mine !== epoch) return;
      const err = e as { name?: string; status?: number };
      schedule(err.name === 'LiveBusy' || err.status === 503 ? 'limit' : 'reconnecting');
      return;
    }
    if (mine !== epoch) {
      void s.close();
      return;
    }
    session = s;
    s.onaudio = () => {
      if (session !== s) return;
      canUnmute = true;
      emit();
    };
    s.onstate = (st) => {
      if (session !== s) return;
      if (st === 'connected') {
        attempt = 0;
        setState('live');
      } else if (st === 'lost') {
        session = null;
        void s.close();
        schedule('reconnecting');
      }
    };
    if (s.hasAudio) canUnmute = true;
    if (s.state === 'connected') {
      attempt = 0;
      setState('live');
    } else if (s.state === 'lost') {
      s.onstate('lost');
    }
    emit();
  }

  function teardown() {
    epoch++;
    clearTimer();
    const s = session;
    session = null;
    canUnmute = false;
    if (s) void s.close();
  }

  const onVisibility = () => {
    if (!running) return;
    if (!visible()) {
      teardown();
      attempt = 0;
      setState('connecting');
      emit();
    } else if (session === null && timer === null) {
      void open();
    }
  };

  return {
    start() {
      if (running) return;
      running = true;
      doc.addEventListener('visibilitychange', onVisibility);
      void open();
    },
    destroy() {
      running = false;
      doc.removeEventListener('visibilitychange', onVisibility);
      teardown();
    },
    toggleMute() {
      if (!canUnmute) return;
      video.muted = !video.muted;
      if (!video.muted) void video.play?.()?.catch?.(() => {});
      emit();
    },
    snapshot,
  };
}
