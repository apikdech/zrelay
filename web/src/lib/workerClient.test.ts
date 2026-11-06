import { IDBFactory } from 'fake-indexeddb';
import { describe, expect, it } from 'vitest';
import { loadVector, manifest, masterText, mockServer, sidecarFor, vectorToken } from '../../test-support/worker-fixtures';
import { ErrAuth } from './zrec/errors';
import { attachWorker, createWorker } from '../worker/worker';
import { WorkerClient, type WorkerLike } from './workerClient';

/** In-memory Worker pair: messages are structured-cloned with their transfer lists, like postMessage. */
function loopback(token: string, video?: Uint8Array) {
  const server = mockServer({ token, video, sidecar: sidecarFor(131072, 1) });
  const core = createWorker({ fetch: server.fetch, indexedDB: new IDBFactory() });
  const sent: { msg: unknown; transfer: Transferable[] }[] = [];
  const client: WorkerLike = {
    onmessage: null,
    onerror: null,
    postMessage(msg, transfer = []) {
      sent.push({ msg, transfer });
      const data = structuredClone(msg, { transfer });
      queueMicrotask(() => scope.deliver(data));
    },
    terminate() {},
  };
  const scope = {
    onmessage: null as ((e: { data: unknown }) => void) | null,
    deliver(data: unknown) {
      this.onmessage?.({ data });
    },
    postMessage(msg: unknown, transfer: Transferable[] = []) {
      const data = structuredClone(msg, { transfer });
      queueMicrotask(() => client.onmessage?.({ data }));
    },
  };
  attachWorker(core, scope);
  return { client, sent };
}

describe('WorkerClient', () => {
  it('resolves a call with the worker result', async () => {
    const { client } = loopback(await vectorToken());
    const wc = new WorkerClient(client);
    expect(await wc.call({ type: 'key.import', encoded: await masterText(), remember: false })).toEqual({
      keyId: manifest.derived.key_id_hex,
    });
  });

  it('correlates concurrent calls by id', async () => {
    const { client } = loopback(await vectorToken());
    const wc = new WorkerClient(client);
    const [a, b] = await Promise.all([
      wc.call({ type: 'key.list' }),
      wc.call({ type: 'key.import', encoded: 'nope', remember: false }),
    ]);
    expect(a).toEqual({ keys: [] });
    expect(b).toEqual({ error: 'format' });
  });

  it('rejects with the rehydrated zrec error, keeping the chunk index', async () => {
    const { client } = loopback(await vectorToken(), loadVector('neg-bitflip-chunk.zrec'));
    const wc = new WorkerClient(client);
    await wc.call({ type: 'key.import', encoded: await masterText(), remember: false });
    await wc.call({ type: 'clip.open', clipId: manifest.clip_id });
    const err = await wc.call({ type: 'clip.read', clipId: manifest.clip_id, chunk: 1 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ErrAuth);
    expect((err as ErrAuth).chunk).toBe(1);
  });

  it('keeps the name of other typed errors', async () => {
    const { client } = loopback(await vectorToken());
    const wc = new WorkerClient(client);
    const err = await wc.call({ type: 'clip.read', clipId: manifest.clip_id, chunk: 0 }).catch((e: Error) => e);
    expect((err as Error).name).toBe('ErrNotOpen');
  });

  it('transfers ArrayBuffers found in the request', async () => {
    const { client, sent } = loopback(await vectorToken());
    const wc = new WorkerClient(client);
    const buf = new ArrayBuffer(8);
    await wc.call({ type: 'key.list', extra: buf, nested: { view: new Uint8Array(new ArrayBuffer(4)) } } as never);
    expect(sent[0]!.transfer).toHaveLength(2);
    expect(buf.byteLength).toBe(0); // detached by the transfer
  });

  it('passes a plaintext ArrayBuffer back from clip.read', async () => {
    const { client } = loopback(await vectorToken());
    const wc = new WorkerClient(client);
    await wc.call({ type: 'key.import', encoded: await masterText(), remember: false });
    await wc.call({ type: 'clip.open', clipId: manifest.clip_id });
    const r = await wc.call<{ plaintext: ArrayBuffer; final: boolean }>({ type: 'clip.read', clipId: manifest.clip_id, chunk: 2 });
    expect(r.plaintext).toBeInstanceOf(ArrayBuffer);
    expect(r.final).toBe(true);
  });

  it('rejects every pending call when the worker errors', async () => {
    const w: WorkerLike = { onmessage: null, onerror: null, postMessage() {}, terminate() {} };
    const wc = new WorkerClient(w);
    const p = wc.call({ type: 'key.list' });
    w.onerror?.({ message: 'boom' });
    await expect(p).rejects.toThrow('boom');
    await expect(wc.call({ type: 'key.list' })).rejects.toThrow();
  });

  it('terminate rejects pending calls and terminates the worker', async () => {
    let terminated = false;
    const w: WorkerLike = { onmessage: null, onerror: null, postMessage() {}, terminate: () => (terminated = true) };
    const wc = new WorkerClient(w);
    const p = wc.call({ type: 'key.list' });
    wc.terminate();
    await expect(p).rejects.toThrow();
    expect(terminated).toBe(true);
  });
});
