import { describe, expect, it, vi } from 'vitest';
import type { ClipReadResult, Request } from '../worker/protocol';
import {
  canExport,
  exportClip,
  ExportDisabledError,
  exportFilename,
  ExportTooLargeError,
  type ExportEnv,
  type SaveHandle,
} from './export';

// 2026-11-06T14:59:05Z is 21:59:05 in Jakarta (UTC+7, no DST).
const T = Date.UTC(2026, 10, 6, 14, 59, 5, 999);

describe('exportFilename', () => {
  it('uses the camera id and the local time in WIB', () => {
    expect(exportFilename('front-door', T, 'Asia/Jakarta', false)).toBe('front-door_20261106-215905.mp4');
  });
  it('rolls over to the next local day at UTC 17:00 in WIB', () => {
    expect(exportFilename('yard', Date.UTC(2026, 10, 6, 17, 0, 0), 'Asia/Jakarta', false)).toBe('yard_20261107-000000.mp4');
  });
  it('puts -incomplete before the extension', () => {
    expect(exportFilename('front-door', T, 'Asia/Jakarta', true)).toBe('front-door_20261106-215905-incomplete.mp4');
  });
  it('zero-pads every field', () => {
    expect(exportFilename('c', Date.UTC(2026, 0, 2, 3, 4, 5), 'UTC', false)).toBe('c_20260102-030405.mp4');
  });
});

describe('canExport', () => {
  it('is false for tampered and for states that cannot play', () => {
    expect(canExport('tampered')).toBe(false);
    expect(canExport('unknown-key')).toBe(false);
    expect(canExport('verifying')).toBe(false);
  });
  it('is true for verified, mismatch and incomplete', () => {
    expect(canExport('verified')).toBe(true);
    expect(canExport('mismatch')).toBe(true);
    expect(canExport('incomplete')).toBe(true);
  });
});

// ---- exportClip ------------------------------------------------------------

const ID = '01M6RXRJAQRJY105FSDF0QRBH0';

class FakeClient {
  calls: Request[] = [];
  failAt: number | null = null;
  /** Like a real incomplete clip: this chunk (the last one) cannot be read and is reported as truncated. */
  truncateAt: number | null = null;
  constructor(
    private readonly chunks: Uint8Array[],
    private readonly size: number | null = chunks.reduce((n, c) => n + c.length, 0),
  ) {}
  async call<T>(msg: Request): Promise<T> {
    this.calls.push(msg);
    if (msg.type === 'clip.open') {
      return { numChunks: this.chunks.length, plaintextSizeBytes: this.size } as unknown as T;
    }
    if (msg.type === 'clip.read') {
      if (this.truncateAt === msg.chunk) {
        throw Object.assign(new Error(`final chunk missing after chunk ${msg.chunk - 1}`), { name: 'ErrTruncated' });
      }
      if (this.failAt === msg.chunk) throw Object.assign(new Error('chunk failed authentication'), { name: 'ErrAuth' });
      const c = this.chunks[msg.chunk]!;
      const r: ClipReadResult = { plaintext: c.slice().buffer, final: msg.chunk === this.chunks.length - 1 };
      return r as unknown as T;
    }
    return {} as T;
  }
}

const chunks = (n: number, size: number): Uint8Array[] => Array.from({ length: n }, (_, i) => new Uint8Array(size).fill(i + 1));

class FakeHandle implements SaveHandle {
  writes: Uint8Array[] = [];
  closed = false;
  aborted = false;
  async createWritable() {
    return {
      write: async (b: Uint8Array) => void this.writes.push(b),
      close: async () => void (this.closed = true),
      abort: async () => void (this.aborted = true),
    };
  }
}

function env(over: Partial<ExportEnv> = {}): ExportEnv & { downloads: { blob: Blob; name: string }[] } {
  const downloads: { blob: Blob; name: string }[] = [];
  return { isIOS: false, download: (blob, name) => void downloads.push({ blob, name }), downloads, ...over };
}

const base = { clipId: ID, filename: 'front-door_20261106-215905.mp4', integrity: 'verified' as const, blobMax: 1000 };

describe('exportClip', () => {
  it('refuses a tampered clip without touching the worker or the picker', async () => {
    const client = new FakeClient(chunks(1, 4));
    const picker = vi.fn();
    await expect(
      exportClip({ ...base, integrity: 'tampered', client, env: env({ showSaveFilePicker: picker }) }),
    ).rejects.toBeInstanceOf(ExportDisabledError);
    expect(client.calls).toEqual([]);
    expect(picker).not.toHaveBeenCalled();
  });

  it('streams chunk by chunk through showSaveFilePicker, in order, without a Blob', async () => {
    const client = new FakeClient(chunks(4, 600)); // 2400 bytes, above blobMax: only streaming can do it
    const handle = new FakeHandle();
    const picker = vi.fn(async () => handle);
    const e = env({ showSaveFilePicker: picker });
    const progress: number[] = [];
    const res = await exportClip({ ...base, client, env: e, onProgress: (n) => progress.push(n) });
    expect(res).toEqual({ kind: 'saved', method: 'picker', bytes: 2400 });
    expect(picker).toHaveBeenCalledWith(expect.objectContaining({ suggestedName: base.filename }));
    expect(handle.writes.map((w) => w[0])).toEqual([1, 2, 3, 4]);
    expect(handle.closed).toBe(true);
    expect(handle.aborted).toBe(false);
    expect(e.downloads).toEqual([]);
    expect(progress.at(-1)).toBe(2400);
  });

  it('opens the picker before any worker call, while the click still counts as a gesture', async () => {
    const client = new FakeClient(chunks(1, 4));
    const order: string[] = [];
    const e = env({
      showSaveFilePicker: async () => {
        order.push(`picker after ${client.calls.length} calls`);
        return new FakeHandle();
      },
    });
    await exportClip({ ...base, client, env: e });
    expect(order).toEqual(['picker after 0 calls']);
  });

  it('aborts the file when a chunk fails to authenticate, so no partial file is kept', async () => {
    const client = new FakeClient(chunks(4, 10));
    client.failAt = 2;
    const handle = new FakeHandle();
    await expect(exportClip({ ...base, client, env: env({ showSaveFilePicker: async () => handle }) })).rejects.toThrow(
      /authentication/,
    );
    expect(handle.aborted).toBe(true);
    expect(handle.closed).toBe(false);
  });

  it('reports a cancelled picker without reading the clip', async () => {
    const client = new FakeClient(chunks(1, 4));
    const e = env({
      showSaveFilePicker: async () => {
        throw new DOMException('cancelled', 'AbortError');
      },
    });
    expect(await exportClip({ ...base, client, env: e })).toEqual({ kind: 'cancelled' });
    expect(client.calls).toEqual([]);
  });

  it('falls back to a Blob download within blob_max', async () => {
    const client = new FakeClient(chunks(3, 100));
    const e = env();
    const res = await exportClip({ ...base, client, env: e });
    expect(res).toEqual({ kind: 'saved', method: 'download', bytes: 300 });
    expect(e.downloads).toHaveLength(1);
    expect(e.downloads[0]!.name).toBe(base.filename);
    expect(e.downloads[0]!.blob.size).toBe(300);
    expect(e.downloads[0]!.blob.type).toBe('video/mp4');
  });

  it('refuses a Blob export above blob_max before reading any chunk when the size is known', async () => {
    const client = new FakeClient(chunks(3, 100));
    await expect(exportClip({ ...base, blobMax: 299, client, env: env() })).rejects.toBeInstanceOf(ExportTooLargeError);
    expect(client.calls.filter((c) => c.type === 'clip.read')).toEqual([]);
  });

  it('stops a Blob export as soon as the running size passes blob_max when the size is unknown', async () => {
    const client = new FakeClient(chunks(5, 100), null);
    await expect(exportClip({ ...base, blobMax: 250, client, env: env() })).rejects.toBeInstanceOf(ExportTooLargeError);
    expect(client.calls.filter((c) => c.type === 'clip.read').length).toBeLessThanOrEqual(4);
  });

  it('shares a File on iOS when the browser can share it', async () => {
    const client = new FakeClient(chunks(2, 50));
    const shared: File[][] = [];
    const e = env({
      isIOS: true,
      canShare: () => true,
      share: async (d) => void shared.push(d.files),
    });
    const res = await exportClip({ ...base, client, env: e });
    expect(res).toEqual({ kind: 'saved', method: 'share', bytes: 100 });
    expect(shared[0]![0]!.name).toBe(base.filename);
    expect(shared[0]![0]!.type).toBe('video/mp4');
    expect(e.downloads).toEqual([]);
  });

  it('downloads on iOS when files cannot be shared', async () => {
    const client = new FakeClient(chunks(1, 50));
    const e = env({ isIOS: true, canShare: () => false, share: async () => {} });
    expect(await exportClip({ ...base, client, env: e })).toMatchObject({ method: 'download' });
  });

  it('treats a dismissed share sheet as cancelled', async () => {
    const client = new FakeClient(chunks(1, 50));
    const e = env({
      isIOS: true,
      canShare: () => true,
      share: async () => {
        throw new DOMException('dismissed', 'AbortError');
      },
    });
    expect(await exportClip({ ...base, client, env: e })).toEqual({ kind: 'cancelled' });
  });

  it('exports an incomplete clip under the filename it was given', async () => {
    const client = new FakeClient(chunks(2, 100), null);
    const e = env();
    const res = await exportClip({ ...base, integrity: 'incomplete', filename: 'c_20260101-000000-incomplete.mp4', client, env: e });
    expect(res).toMatchObject({ kind: 'saved', bytes: 200 });
    expect(e.downloads[0]!.name).toBe('c_20260101-000000-incomplete.mp4');
  });

  describe('an incomplete clip, whose last chunk is truncated', () => {
    const incomplete = { ...base, integrity: 'incomplete' as const, filename: 'c_20260101-000000-incomplete.mp4' };

    it('saves the verified chunks through the picker and closes the file', async () => {
      const client = new FakeClient(chunks(4, 100), null);
      client.truncateAt = 3;
      const handle = new FakeHandle();
      let name = '';
      const e = env({
        showSaveFilePicker: async (o) => {
          name = o.suggestedName;
          return handle;
        },
      });
      const res = await exportClip({ ...incomplete, client, env: e });
      expect(res).toEqual({ kind: 'saved', method: 'picker', bytes: 300 });
      expect(name).toBe('c_20260101-000000-incomplete.mp4');
      expect(handle.writes.map((w) => w[0])).toEqual([1, 2, 3]);
      expect(handle.closed).toBe(true);
      expect(handle.aborted).toBe(false);
    });

    it('builds the Blob from the verified chunks under the -incomplete name', async () => {
      const client = new FakeClient(chunks(4, 100), null);
      client.truncateAt = 3;
      const e = env();
      const res = await exportClip({ ...incomplete, client, env: e });
      expect(res).toEqual({ kind: 'saved', method: 'download', bytes: 300 });
      expect(e.downloads).toHaveLength(1);
      expect(e.downloads[0]!.name).toBe('c_20260101-000000-incomplete.mp4');
      expect(e.downloads[0]!.blob.size).toBe(300);
    });

    it('still fails when nothing verified could be read', async () => {
      const client = new FakeClient(chunks(2, 100), null);
      client.truncateAt = 0;
      const handle = new FakeHandle();
      await expect(exportClip({ ...incomplete, client, env: env({ showSaveFilePicker: async () => handle }) })).rejects.toMatchObject({
        name: 'ErrTruncated',
      });
      expect(handle.aborted).toBe(true);
      expect(handle.closed).toBe(false);
    });

    it('still aborts on an authentication failure', async () => {
      const client = new FakeClient(chunks(4, 100), null);
      client.failAt = 2;
      const handle = new FakeHandle();
      await expect(exportClip({ ...incomplete, client, env: env({ showSaveFilePicker: async () => handle }) })).rejects.toMatchObject({
        name: 'ErrAuth',
      });
      expect(handle.aborted).toBe(true);
      expect(handle.closed).toBe(false);
    });
  });

  it('does not hide a truncation in a clip that is not marked incomplete', async () => {
    const client = new FakeClient(chunks(3, 100), null);
    client.truncateAt = 2;
    const e = env();
    await expect(exportClip({ ...base, client, env: e })).rejects.toMatchObject({ name: 'ErrTruncated' });
    expect(e.downloads).toEqual([]);
  });

  it('never reads more chunks than the clip has', async () => {
    const client = new FakeClient(chunks(3, 10));
    await exportClip({ ...base, client, env: env() });
    const reads = client.calls.filter((c): c is Extract<Request, { type: 'clip.read' }> => c.type === 'clip.read');
    expect(reads.map((r) => r.chunk)).toEqual([0, 1, 2]);
  });
});
