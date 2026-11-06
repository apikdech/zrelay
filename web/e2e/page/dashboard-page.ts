// Test page for e2e/dashboard.spec.ts: the real timeline page with a canned
// API and a scripted player, so layout and keyboard handling run in a real
// browser engine. The clock is fixed at 2026-11-06 15:00 UTC (22:00 in Jakarta).
import { mount } from 'svelte';
import '../../src/app.css';
import Page from '../../src/routes/+page.svelte';
import { DashClient, fakePlayerFactory, sidecar } from '../../test-support/dashboard-fakes';

const NOW = Date.UTC(2026, 10, 6, 15, 0, 0);
const ULID = '01M6RXRJAQRJY105FSDF0QR';
const client = new DashClient();
client.status = {
  schema: 'zrec.status/1',
  relay_id: 'r',
  version: 'v',
  updated_at: new Date(NOW - 4 * 60_000).toISOString().replace('.000Z', 'Z'),
  key_id: '0123456789abcdef',
  cameras: [],
};
client.clips = Array.from({ length: 400 }, (_, i) => {
  const id = ULID + String(i).padStart(3, '0');
  const start = new Date(Date.UTC(2026, 10, 6, 0, 0, 0) + i * 120_000).toISOString().replace('.000Z', 'Z');
  return sidecar(id, start);
});

mount(Page, {
  target: document.getElementById('app')!,
  props: { client, factory: fakePlayerFactory, zone: 'Asia/Jakarta', nowMs: NOW, profile: 'desktop' },
});
