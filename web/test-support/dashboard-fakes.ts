// Test doubles for the timeline dashboard: a scriptable Player and a worker
// client that serves canned API answers. Test-only.
import type { Player, PlayerState } from '../src/lib/player/engine';
import { initialState } from '../src/lib/player/status';
import type { Request } from '../src/worker/protocol';

export class FakePlayer implements Player {
  static instances: FakePlayer[] = [];
  /** What load() resolves the next clip to. */
  static nextIntegrity: PlayerState['integrity'] = 'verified';
  loads: string[] = [];
  seeks: number[] = [];
  autoAdvance = false;
  destroyed = false;
  state: PlayerState = initialState();
  #listeners = new Set<(s: PlayerState) => void>();

  constructor(
    public video: HTMLVideoElement,
    public opts: { profile: string },
  ) {
    FakePlayer.instances.push(this);
  }

  async load(clipId: string) {
    this.loads.push(clipId);
    this.emit({
      ...initialState(),
      clipId,
      integrity: FakePlayer.nextIntegrity,
      header: { cameraId: 'front-door', startUnixMs: Date.UTC(2026, 10, 6, 14, 59, 5) },
    });
    return FakePlayer.nextIntegrity;
  }
  async seek(tMs: number) {
    this.seeks.push(tMs);
  }
  onState(cb: (s: PlayerState) => void) {
    this.#listeners.add(cb);
    cb(this.state);
    return () => this.#listeners.delete(cb);
  }
  setAutoAdvance(on: boolean) {
    this.autoAdvance = on;
  }
  destroy() {
    this.destroyed = true;
  }
  emit(s: PlayerState) {
    this.state = s;
    for (const cb of [...this.#listeners]) cb(s);
  }
}

export const fakePlayerFactory = (video: HTMLVideoElement, _client: unknown, opts: { profile: 'mobile' | 'desktop' }): Player =>
  new FakePlayer(video, opts);

export const ids = {
  a: '01M6RXRJAQRJY105FSDF0QRBH1',
  b: '01M6RXRJAQRJY105FSDF0QRBH2',
  c: '01M6RXRJAQRJY105FSDF0QRBH3',
};

export function sidecar(id: string, startIso: string, over: Record<string, unknown> = {}) {
  const start = Date.parse(startIso);
  return {
    clip_id: id,
    series_id: id,
    segment_index: 0,
    status: 'complete',
    camera: { id: 'front-door', name: 'Front Door' },
    start_time: startIso,
    end_time: new Date(start + 60_000).toISOString().replace('.000Z', 'Z'),
    duration_ms: 60_000,
    triggers: [{ type: 'person', first_at_ms: 1000, last_at_ms: 9000 }],
    ...over,
  };
}

export class DashClient {
  calls: Request[] = [];
  cameras = [{ id: 'front-door', name: 'Front Door' }];
  clips: ReturnType<typeof sidecar>[] = [];
  invalid = 0;
  status: unknown = null;
  failStatus = false;
  async call<T = unknown>(msg: Request): Promise<T> {
    this.calls.push(msg);
    if (msg.type === 'api.get') {
      switch (msg.path) {
        case '/api/v1/cameras':
          return this.cameras as T;
        case '/api/v1/clips':
          return { clips: this.clips, invalid: this.invalid } as T;
        case '/api/v1/coverage':
          return { ready: [], dropped: [] } as T;
        case '/api/v1/status':
          if (this.failStatus) throw new Error('down');
          return this.status as T;
      }
    }
    if (msg.type === 'key.lock') return { keys: [] } as T;
    return {} as T;
  }
}
