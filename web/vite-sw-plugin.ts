import { resolve } from 'node:path';
import { build, type Plugin } from 'vite';
import { shellAssets, shellVersion } from './src/lib/pwa/shell.ts';

/**
 * Compiles src/sw.ts to /sw.js beside the app, with the list of the build's
 * content-hashed files baked in as the precache list (ui-spec §4.9). The worker
 * is not hashed: it is revalidated on every load, which is how updates are found.
 */
export function serviceWorkerPlugin(): Plugin {
  let root = '';
  let outDir = '';
  return {
    name: 'zview-service-worker',
    apply: 'build',
    configResolved(config) {
      root = config.root;
      outDir = resolve(config.root, config.build.outDir);
    },
    async writeBundle(_options, bundle) {
      const shell = shellAssets(Object.keys(bundle));
      await build({
        configFile: false,
        root,
        publicDir: false,
        logLevel: 'warn',
        define: { __SHELL__: JSON.stringify(shell), __SHELL_VERSION__: JSON.stringify(shellVersion(shell)) },
        build: {
          outDir,
          emptyOutDir: false,
          minify: true,
          lib: { entry: resolve(root, 'src/sw.ts'), formats: ['iife'], name: 'zviewSw', fileName: () => 'sw.js' },
        },
      });
    },
  };
}
