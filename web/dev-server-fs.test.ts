import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { createServer, type ViteDevServer } from 'vite';
import { fsAllow } from './vite-fs-allow.ts';

// The dev server may run on the relay host, where deploy/secrets holds the
// master key and the S3 keys in the same checkout. It must serve only the web
// root and the zrec vectors, never the rest of the repo (Contract D8).
const webRoot = fileURLToPath(new URL('.', import.meta.url));
const repoRoot = fileURLToPath(new URL('..', import.meta.url));

// deploy/runbook.md stands in for deploy/secrets/*: same directory subtree.
const secretish = `/@fs/${repoRoot}deploy/runbook.md`;
const vectors = `/@fs/${repoRoot}testdata/zrec/v1/vectors.json`;

const servers: [string, () => Promise<ViteDevServer>][] = [
  [
    'vite.config.ts',
    () =>
      createServer({
        configFile: `${webRoot}vite.config.ts`,
        root: webRoot,
        logLevel: 'silent',
        server: { host: '127.0.0.1', port: 0 },
      }),
  ],
  [
    'the shared fsAllow list (e2e server)',
    () =>
      createServer({
        configFile: false,
        root: webRoot,
        logLevel: 'silent',
        server: { host: '127.0.0.1', port: 0, fs: { allow: fsAllow(webRoot) } },
      }),
  ],
];

describe.each(servers)('dev server fs scope: %s', (_name, make) => {
  let server: ViteDevServer;
  let origin: string;
  beforeAll(async () => {
    server = await make();
    await server.listen();
    origin = `http://127.0.0.1:${(server.httpServer!.address() as AddressInfo).port}`;
  });
  afterAll(async () => {
    await server.close();
  });

  it('does not serve deploy/ files (the secrets directory)', async () => {
    expect((await fetch(origin + secretish)).status).toBe(403);
    expect((await fetch(origin + secretish + '?raw')).status).toBe(403);
  });

  it('still serves the zrec vectors', async () => {
    expect((await fetch(origin + vectors)).status).toBe(200);
  });
});
