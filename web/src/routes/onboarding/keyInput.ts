// Live validation of the pasted key (ui-spec §4.1). The check is the same pure
// parser the worker uses; the decoded bytes are zeroed at once and never kept.
import { KeyChecksumError } from '../../lib/zrec/errors';
import { parseMasterKey } from '../../lib/zrec/keys';

export type KeyInputState = 'empty' | 'valid' | 'typo' | 'format';

export function checkKeyInput(text: string): KeyInputState {
  if (text.trim() === '') return 'empty';
  try {
    parseMasterKey(text).fill(0);
    return 'valid';
  } catch (e) {
    return e instanceof KeyChecksumError ? 'typo' : 'format';
  }
}

/** A 16-hex key_id as a fingerprint, e.g. a1b2-c3d4-e5f6-0718. */
export function fingerprint(keyIdHex: string): string {
  return (keyIdHex.match(/.{1,4}/g) ?? []).join('-');
}
