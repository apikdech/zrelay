import { join, resolve } from 'node:path';

// Directories the Vite dev server may serve through /@fs/: the web root and the
// cross-language zrec vectors. Never the whole repo: deploy/secrets holds the
// master key and the S3 keys in the same checkout on the relay host.
export function fsAllow(webRoot: string): string[] {
  const root = resolve(webRoot);
  return [root, join(root, '..', 'testdata', 'zrec', 'v1')];
}
