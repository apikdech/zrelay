// Blob fallback for browsers without MSE (ui-spec §4.3.3): all chunks are
// decrypted into an array of parts and wrapped in one Blob, never concatenated.

/** Least-recently-used Blob URLs; evicted ones are revoked. */
export class BlobLru<V extends { url: string }> {
  #entries = new Map<string, V>();

  constructor(
    private readonly max: number,
    private readonly revoke: (url: string) => void = (u) => URL.revokeObjectURL(u),
  ) {}

  get(key: string): V | undefined {
    const v = this.#entries.get(key);
    if (v !== undefined) {
      // Refresh recency.
      this.#entries.delete(key);
      this.#entries.set(key, v);
    }
    return v;
  }

  add(key: string, value: V): void {
    const old = this.#entries.get(key);
    if (old && old.url !== value.url) this.revoke(old.url);
    this.#entries.delete(key);
    this.#entries.set(key, value);
    while (this.#entries.size > this.max) {
      const [oldestKey, oldest] = this.#entries.entries().next().value as [string, V];
      this.#entries.delete(oldestKey);
      this.revoke(oldest.url);
    }
  }

  clear(): void {
    for (const v of this.#entries.values()) this.revoke(v.url);
    this.#entries.clear();
  }
}

/** The parts are referenced, not copied. */
export const blobFromParts = (parts: readonly ArrayBuffer[]): Blob => new Blob(parts as BlobPart[], { type: 'video/mp4' });
