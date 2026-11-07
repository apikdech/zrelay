// The crypto and data worker (ui-spec §4.2). A single Dedicated Worker owns all
// key material and every data-origin request; the main thread never holds the
// master key, K_api or a DEK. `createWorker` is the testable core and
// `attachWorker` binds it to a postMessage scope.
import { ErrAuth } from '../lib/zrec/errors';
import { ApiError, apiGet, makeAuthFetch } from './api';
import { Clips } from './clips';
import { Keys } from './keystore';
import { LiveError, liveClose, liveOffer } from './live';
import type { Envelope, Handled, Reply, Request, WireError } from './protocol';
import { SchemaError } from './schemas';

export interface WorkerDeps {
  fetch: typeof fetch;
  indexedDB: IDBFactory;
}

export interface Worker {
  handle(req: Request): Promise<Handled>;
}

export function createWorker(deps: WorkerDeps): Worker {
  const keys = new Keys(deps.indexedDB);
  const authFetch = makeAuthFetch(deps.fetch, () => keys.tokens());
  const clips = new Clips(authFetch, () => keys.keyring());

  return {
    async handle(req: Request): Promise<Handled> {
      switch (req?.type) {
        case 'key.import':
          return { result: await keys.import(String(req.encoded), req.remember === true) };
        case 'key.list':
          return { result: await keys.list() };
        case 'key.forget':
          await keys.forget(String(req.keyId));
          authFetch.reset();
          clips.closeAll();
          return { result: await keys.list() };
        case 'key.lock':
          keys.lock();
          authFetch.reset();
          clips.closeAll();
          return { result: await keys.list() };
        case 'api.get':
          return { result: await apiGet(authFetch, req.path, req.query ?? {}) };
        case 'clip.open':
          return { result: await clips.open(req.clipId) };
        case 'clip.read': {
          const result = await clips.read(req.clipId, req.chunk);
          return { result, transfer: [result.plaintext] };
        }
        case 'clip.close':
          clips.close(req.clipId);
          return { result: {} };
        case 'live.offer':
          return { result: await liveOffer(authFetch, req.cameraId, req.sdp) };
        case 'live.close':
          return { result: await liveClose(authFetch, req.sessionPath) };
        default:
          throw new SchemaError('unknown message type');
      }
    },
  };
}

/** The part of a worker global scope the protocol needs. */
export interface Scope {
  onmessage: ((e: { data: unknown }) => void) | null;
  postMessage(msg: unknown, transfer?: Transferable[]): void;
}

function toWire(e: unknown): WireError {
  const err = e instanceof Error ? e : new Error('unknown error');
  const wire: WireError = {
    name: err.name,
    // Defence in depth: nothing that looks like key text ever leaves the worker.
    message: err.message.slice(0, 512).replace(/zk1-[0-9a-f-]+/gi, '[redacted]'),
  };
  if (err instanceof ErrAuth) wire.chunk = err.chunk;
  if (err instanceof ApiError || err instanceof LiveError) {
    if (err.status !== undefined) wire.status = err.status;
  }
  return wire;
}

export function attachWorker(core: Worker, scope: Scope): void {
  scope.onmessage = (e) => {
    const env = e.data as Envelope;
    void (async () => {
      let reply: Reply;
      let transfer: Transferable[] = [];
      try {
        const out = await core.handle(env.msg);
        reply = { id: env.id, ok: true, result: out.result };
        transfer = out.transfer ?? [];
      } catch (err) {
        reply = { id: env.id, ok: false, error: toWire(err) };
      }
      try {
        scope.postMessage(reply, transfer);
      } catch (err) {
        // e.g. DataCloneError: still settle the caller's promise.
        try {
          scope.postMessage({ id: env.id, ok: false, error: toWire(err) } satisfies Reply);
        } catch {
          /* nothing more can be done */
        }
      }
    })();
  };
}

// Bootstrap only inside a real Dedicated Worker; importing this module in tests has no side effects.
declare const WorkerGlobalScope: (new () => unknown) | undefined;
if (typeof WorkerGlobalScope !== 'undefined' && self instanceof (WorkerGlobalScope as new () => unknown)) {
  const scope = self as unknown as Scope;
  attachWorker(createWorker({ fetch: self.fetch.bind(self), indexedDB: self.indexedDB }), scope);
}
