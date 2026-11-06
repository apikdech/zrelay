// A throwaway master key for component tests. Unlike worker-fixtures it reads no
// files, so it also works under jsdom (whose URL global node's fs rejects).
import { deriveKeyId, importMasterKey } from '../src/lib/zrec/keys';
import { sha256 } from '../src/lib/zrec/sha256';

const toHex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/** 32 bytes 0x00..0x1f. */
export const RAW = Uint8Array.from({ length: 32 }, (_, i) => i);

/** zk1-<hex>-<checksum> (Contract §3.1). */
export function testKeyText(): string {
  const pre = new Uint8Array(3 + RAW.length);
  pre.set(new TextEncoder().encode('zk1'));
  pre.set(RAW, 3);
  return `zk1-${toHex(RAW)}-${toHex(sha256(pre).subarray(0, 2))}`;
}

/** The key_id (hex) that key derives. */
export async function testKeyId(): Promise<string> {
  return toHex(await deriveKeyId(await importMasterKey(RAW)));
}
