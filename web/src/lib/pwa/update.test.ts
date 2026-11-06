import { describe, expect, it, vi } from 'vitest';
import { createUpdater, type RegistrationLike, type WorkerLike } from './update';

class FakeWorker extends EventTarget implements WorkerLike {
  state: 'installing' | 'installed' | 'activated' = 'installing';
  posted: unknown[] = [];
  postMessage(m: unknown) {
    this.posted.push(m);
  }
  become(s: FakeWorker['state']) {
    this.state = s;
    this.dispatchEvent(new Event('statechange'));
  }
}
class FakeRegistration extends EventTarget implements RegistrationLike {
  waiting: FakeWorker | null = null;
  installing: FakeWorker | null = null;
}
class FakeContainer extends EventTarget {
  controller: object | null = {};
}

function setup(opts: { playing?: boolean; controller?: boolean } = {}) {
  const registration = new FakeRegistration();
  const container = new FakeContainer();
  if (opts.controller === false) container.controller = null;
  const state = { playing: opts.playing ?? false };
  const reload = vi.fn();
  const updater = createUpdater({ container, registration, isPlaying: () => state.playing, reload });
  const changes = vi.fn();
  updater.onChange(changes);
  return { registration, container, state, reload, updater, changes };
}

/** A new version installs behind a page that already has a controller. */
function install(registration: FakeRegistration): FakeWorker {
  const w = new FakeWorker();
  registration.installing = w;
  registration.dispatchEvent(new Event('updatefound'));
  w.become('installed');
  registration.waiting = w;
  registration.installing = null;
  return w;
}

describe('update availability', () => {
  it('is false until a new version is waiting', () => {
    expect(setup().updater.available).toBe(false);
  });

  it('turns true when a new version finishes installing behind a controlled page', () => {
    const t = setup();
    install(t.registration);
    expect(t.updater.available).toBe(true);
    expect(t.changes).toHaveBeenCalled();
  });

  it('is true at start when a version is already waiting', () => {
    const registration = new FakeRegistration();
    registration.waiting = new FakeWorker();
    const updater = createUpdater({ container: new FakeContainer(), registration, isPlaying: () => false, reload: () => {} });
    expect(updater.available).toBe(true);
  });

  it('is not an update when nothing controlled the page yet (the first install)', () => {
    const t = setup({ controller: false });
    install(t.registration);
    expect(t.updater.available).toBe(false);
  });
});

describe('apply', () => {
  it('asks the waiting worker to take over and reloads once it controls the page', () => {
    const t = setup();
    const w = install(t.registration);
    expect(t.updater.apply()).toBe(true);
    expect(w.posted).toEqual([{ type: 'SKIP_WAITING' }]);
    expect(t.reload).not.toHaveBeenCalled();
    t.container.dispatchEvent(new Event('controllerchange'));
    expect(t.reload).toHaveBeenCalledTimes(1);
  });

  it('never skips waiting while a clip plays', () => {
    const t = setup({ playing: true });
    const w = install(t.registration);
    expect(t.updater.apply()).toBe(false);
    expect(w.posted).toEqual([]);
    t.container.dispatchEvent(new Event('controllerchange'));
    expect(t.reload).not.toHaveBeenCalled();
    // Once playback stops the same request goes through.
    t.state.playing = false;
    expect(t.updater.apply()).toBe(true);
    expect(w.posted).toEqual([{ type: 'SKIP_WAITING' }]);
  });

  it('does not reload for a controller change it did not ask for', () => {
    const t = setup();
    install(t.registration);
    t.container.dispatchEvent(new Event('controllerchange')); // another tab updated
    expect(t.reload).not.toHaveBeenCalled();
  });

  it('does nothing when no update is waiting', () => {
    const t = setup();
    expect(t.updater.apply()).toBe(false);
  });
});
