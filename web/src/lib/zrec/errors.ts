// Error taxonomy mirroring pkg/zrec (Contract §4.4). `name` equals the Go
// sentinel name so cross-language vectors can compare by string.

export class ErrFormat extends Error {
  override name = 'ErrFormat';
}

export class ErrUnknownKey extends Error {
  override name = 'ErrUnknownKey';
}

export class ErrAuthHeader extends Error {
  override name = 'ErrAuthHeader';
}

/** A chunk failed authentication. `chunk` is its index. */
export class ErrAuth extends Error {
  override name = 'ErrAuth';
  readonly chunk: number;
  constructor(chunk: number) {
    super(`chunk authentication failed: chunk ${chunk}`);
    this.chunk = chunk;
  }
}

export class ErrTruncated extends Error {
  override name = 'ErrTruncated';
}

export class ErrTrailingData extends Error {
  override name = 'ErrTrailingData';
}

/** Well-formed master key text whose checksum does not match: a typo. */
export class KeyChecksumError extends Error {
  override name = 'KeyChecksumError';
  constructor() {
    super('master key checksum mismatch (typo?)');
  }
}

/** Text that is not of the form zk1-<64 hex>-<4 hex>. */
export class KeyFormatError extends Error {
  override name = 'KeyFormatError';
}
