import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { svelteTesting } from '@testing-library/svelte/vite';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import { serviceWorkerPlugin } from './vite-sw-plugin.ts';

// The cross-language zrec vectors live at <repo>/testdata, outside web/.
const repoRoot = fileURLToPath(new URL('..', import.meta.url));

export default defineConfig({
  // static/ holds the PWA manifest and icons, served at the root.
  publicDir: 'static',
  plugins: [tailwindcss(), svelte(), svelteTesting(), serviceWorkerPlugin()],
  server: { fs: { allow: [repoRoot] } },
  test: {
    include: ['src/**/*.test.ts', 'test-support/**/*.test.ts', '*.test.ts'],
    environment: 'node',
    setupFiles: ['test-support/setup-dom.ts'],
  },
});
