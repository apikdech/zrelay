// Main-thread side of the crypto/data worker protocol.
import { ErrAuth, ErrAuthHeader, ErrFormat, ErrTrailingData, ErrTruncated, ErrUnknownKey } from './zrec/errors';
import type { Envelope, Reply, Request, WireError } from '../worker/protocol';

/** The slice of `Worker` the client needs, so tests can use an in-memory pair. */
export interface WorkerLike {
  postMessage(msg: unknown, transfer?: Transferable[]): void;
  onmessage: ((e: { data: unknown }) => void) | null;
  onerror: ((e: { message?: string }) => void) | null;
  terminate(): void;
}

/** Collects every ArrayBuffer reachable in a request so it is transferred, not copied. */
function collectBuffers(v: unknown, out: Set<ArrayBuffer>, depth = 0): void {
  if (depth > 4 || v === null || typeof v !== 'object') return;
  if (v instanceof ArrayBuffer) out.add(v);
  else if (ArrayBuffer.isView(v)) {
    if (v.buffer instanceof ArrayBuffer) out.add(v.buffer);
  } else for (const x of Object.values(v)) collectBuffers(x, out, depth + 1);
}

/** The zrec errors callers branch on with `instanceof`; any other error keeps only its `name`. */
const ZREC_ERRORS: Record<string, new (message: string) => Error> = {
  ErrFormat,
  ErrUnknownKey,
  ErrAuthHeader,
  ErrTruncated,
  ErrTrailingData,
};

/** Rebuilds a typed error from the wire. ErrAuth keeps its chunk index. */
function rehydrate(e: WireError): Error {
  let err: Error;
  if (e.name === 'ErrAuth') {
    err = new ErrAuth(e.chunk ?? -1);
    err.message = e.message;
  } else {
    const Cls = Object.hasOwn(ZREC_ERRORS, e.name) ? ZREC_ERRORS[e.name] : undefined;
    err = Cls ? new Cls(e.message) : new Error(e.message);
    err.name = e.name;
  }
  if (e.status !== undefined) (err as Error & { status?: number }).status = e.status;
  return err;
}

interface Pending {
  resolve(v: unknown): void;
  reject(e: Error): void;
}

export class WorkerClient {
  #next = 1;
  #pending = new Map<number, Pending>();
  #dead: Error | null = null;

  constructor(private readonly worker: WorkerLike) {
    worker.onmessage = (e) => this.#onMessage(e.data as Reply);
    worker.onerror = (e) => this.#fail(new Error(e.message ?? 'worker error'));
  }

  call<T = unknown>(msg: Request): Promise<T> {
    if (this.#dead) return Promise.reject(this.#dead);
    const id = this.#next++;
    return new Promise<T>((resolve, reject) => {
      this.#pending.set(id, { resolve: resolve as (v: unknown) => void, reject });
      const buffers = new Set<ArrayBuffer>();
      collectBuffers(msg, buffers);
      const env: Envelope = { id, msg };
      this.worker.postMessage(env, [...buffers]);
    });
  }

  terminate(): void {
    this.worker.terminate();
    this.#fail(new Error('worker terminated'));
  }

  #onMessage(r: Reply): void {
    const p = this.#pending.get(r.id);
    if (!p) return;
    this.#pending.delete(r.id);
    if (r.ok) p.resolve(r.result);
    else p.reject(rehydrate(r.error));
  }

  #fail(err: Error): void {
    this.#dead = err;
    for (const p of this.#pending.values()) p.reject(err);
    this.#pending.clear();
  }
}

let shared: WorkerClient | null = null;

/** The app-wide client instance (for APIs that take a client); the worker starts on first use. */
export function getWorkerClient(): WorkerClient {
  shared ??= new WorkerClient(
    new Worker(new URL('../worker/worker.ts', import.meta.url), { type: 'module' }) as unknown as WorkerLike,
  );
  return shared;
}

/** `workerClient.call<T>(msg): Promise<T>` on the app-wide client; the worker starts on first call. */
export const workerClient = {
  call: <T = unknown>(msg: Request): Promise<T> => getWorkerClient().call<T>(msg),
};
