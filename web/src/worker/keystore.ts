// Key storage inside the worker. The IndexedDB store keeps only non-extractable
// HKDF base CryptoKeys (ui-spec §4.1 "Remember this device"); the master key's
// raw bytes are never persisted. `Keys` holds the in-memory working set.
import { deriveApiToken, deriveKeyId, deriveWrapKey, importMasterKey, parseMasterKey } from '../lib/zrec/keys';
import type { Keyring } from '../lib/zrec/reader';
import type { ImportResult } from './protocol';

const DB_NAME = 'zview-keys';
const STORE = 'keys';

export interface Keystore {
  put(keyId: string, base: CryptoKey): Promise<void>;
  delete(keyId: string): Promise<void>;
  all(): Promise<[string, CryptoKey][]>;
  close(): void;
}

function wrap<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error('indexeddb request failed'));
  });
}

function done(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error('indexeddb transaction failed'));
    tx.onabort = () => reject(tx.error ?? new Error('indexeddb transaction aborted'));
  });
}

export async function openKeystore(idb: IDBFactory): Promise<Keystore> {
  const req = idb.open(DB_NAME, 1);
  req.onupgradeneeded = () => {
    req.result.createObjectStore(STORE, { keyPath: 'keyId' });
  };
  const db = await wrap(req);
  return {
    async put(keyId, base) {
      const tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).put({ keyId, base });
      await done(tx);
    },
    async delete(keyId) {
      const tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).delete(keyId);
      await done(tx);
    },
    async all() {
      const rows = await wrap(db.transaction(STORE).objectStore(STORE).getAll() as IDBRequest<{ keyId: string; base: CryptoKey }[]>);
      return rows.map((r) => [r.keyId, r.base] as [string, CryptoKey]);
    },
    close: () => db.close(),
  };
}

interface Entry {
  base: CryptoKey;
  /** K_api as the Bearer credential. It exists only inside the worker. */
  token: string;
  remembered: boolean;
}

const toHex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/**
 * The worker's working key set. Remembered base keys are loaded lazily from
 * IndexedDB, so `lock()` can discard every derived key and still leave the
 * remembered base keys on disk for the next use.
 */
export class Keys {
  #store: Promise<Keystore> | null = null;
  #entries = new Map<string, Entry>();
  #ring: Keyring = new Map();
  #loaded = false;

  constructor(private readonly idb: IDBFactory) {}

  #open(): Promise<Keystore> {
    return (this.#store ??= openKeystore(this.idb));
  }

  async #add(keyId: string, base: CryptoKey, remembered: boolean): Promise<void> {
    const [wrapKey, token] = await Promise.all([deriveWrapKey(base), deriveApiToken(base)]);
    const prev = this.#entries.get(keyId);
    // Re-insert so the newest key is last.
    this.#entries.delete(keyId);
    this.#entries.set(keyId, { base, token, remembered: remembered || (prev?.remembered ?? false) });
    this.#ring.set(keyId, wrapKey);
  }

  async #load(): Promise<void> {
    if (this.#loaded) return;
    for (const [keyId, base] of await (await this.#open()).all()) {
      if (!this.#entries.has(keyId)) await this.#add(keyId, base, true);
    }
    this.#loaded = true;
  }

  async import(encoded: string, remember: boolean): Promise<ImportResult> {
    let raw: Uint8Array;
    try {
      raw = parseMasterKey(encoded);
    } catch (e) {
      return { error: (e as Error).name === 'KeyChecksumError' ? 'checksum' : 'format' };
    }
    let base: CryptoKey;
    try {
      base = await importMasterKey(raw);
    } finally {
      raw.fill(0);
    }
    const keyId = toHex(await deriveKeyId(base));
    await this.#load();
    if (remember) await (await this.#open()).put(keyId, base);
    await this.#add(keyId, base, remember);
    return { keyId };
  }

  /** Key ids and remembered flags. It derives nothing, so listing after lock() keeps memory empty. */
  async list(): Promise<{ keys: { keyId: string; remembered: boolean }[] }> {
    const stored = new Set((await (await this.#open()).all()).map(([id]) => id));
    const ids = [...new Set([...this.#entries.keys(), ...stored])];
    return { keys: ids.map((keyId) => ({ keyId, remembered: stored.has(keyId) })) };
  }

  /** Deletes the key from IndexedDB, then locks (ui-spec §4.1). */
  async forget(keyId: string): Promise<void> {
    await (await this.#open()).delete(keyId);
    this.lock();
  }

  /** Discards every derived key from memory. Remembered base keys reload on next use. */
  lock(): void {
    this.#entries.clear();
    this.#ring = new Map();
    this.#loaded = false;
  }

  /** Wrap keys by hex key_id, for opening clip headers. */
  async keyring(): Promise<Keyring> {
    await this.#load();
    return this.#ring;
  }

  /** Bearer credentials, newest key first. */
  async tokens(): Promise<string[]> {
    await this.#load();
    return [...this.#entries.values()].reverse().map((e) => e.token);
  }
}
