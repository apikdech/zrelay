import { defineConfig, devices } from '@playwright/test';

// The spec starts its own Vite server on an ephemeral port (127.0.0.1:0), so
// there is no fixed port and no `webServer` entry here.
export default defineConfig({
  testDir: './e2e',
  testMatch: '*.spec.ts',
  fullyParallel: true,
  reporter: 'list',
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
  ],
});
