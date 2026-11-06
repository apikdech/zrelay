// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import type { Status } from '../worker/schemas';
import StatusDot from './components/StatusDot.svelte';
import { relayHealth } from './relayStatus';

const NOW = Date.UTC(2026, 10, 6, 15, 0, 0);
const iso = (ms: number) => new Date(ms).toISOString().replace('.000Z', 'Z');

function status(over: Partial<Status> & { ageMs?: number } = {}): Status {
  const { ageMs = 30_000, ...rest } = over;
  return {
    schema: 'zrec.status/1',
    relay_id: 'r1',
    version: 'v2',
    updated_at: iso(NOW - ageMs),
    key_id: '0123456789abcdef',
    cameras: [{ id: 'front', name: 'Front', rtsp: 'up', events: 'up', since: iso(NOW - 3_600_000), last_clip_at: null }],
    ...rest,
  };
}

afterEach(cleanup);

describe('relayHealth', () => {
  it('is ok when fresh, cameras up and storage closed', () => {
    const h = relayHealth(status({ storage: { state: 'closed' } }), NOW);
    expect(h.level).toBe('ok');
  });

  it('is red when updated_at is 4 minutes old', () => {
    const h = relayHealth(status({ ageMs: 4 * 60_000 }), NOW);
    expect(h.level).toBe('bad');
    expect(h.tooltip).toContain(iso(NOW - 4 * 60_000));
    expect(h.tooltip.toLowerCase()).toContain('stale');
  });

  it('is not stale at exactly 3 minutes, only older', () => {
    expect(relayHealth(status({ ageMs: 180_000 }), NOW).level).toBe('ok');
    expect(relayHealth(status({ ageMs: 180_001 }), NOW).level).toBe('bad');
  });

  it('is red when a camera is down, and the tooltip shows its since', () => {
    const since = iso(NOW - 600_000);
    const h = relayHealth(
      status({ cameras: [{ id: 'front', name: 'Front Door', rtsp: 'down', events: 'up', since, last_clip_at: null }] }),
      NOW,
    );
    expect(h.level).toBe('bad');
    expect(h.tooltip).toContain('Front Door');
    expect(h.tooltip).toContain(since);
  });

  it('is amber while the storage breaker is not closed, and says which state', () => {
    const since = iso(NOW - 120_000);
    const h = relayHealth(status({ storage: { state: 'open', since, backlog_bytes: 5 } }), NOW);
    expect(h.level).toBe('warn');
    expect(h.tooltip).toContain('open');
    expect(h.tooltip).toContain(since);
    expect(relayHealth(status({ storage: { state: 'half_open' } }), NOW).level).toBe('warn');
  });

  it('is amber when only the event stream or a camera is degraded', () => {
    const cam = (rtsp: 'up' | 'down' | 'degraded', events: 'up' | 'down' | 'degraded') => [
      { id: 'f', name: 'F', rtsp, events, since: iso(NOW), last_clip_at: null },
    ];
    expect(relayHealth(status({ cameras: cam('up', 'down') }), NOW).level).toBe('warn');
    expect(relayHealth(status({ cameras: cam('degraded', 'up') }), NOW).level).toBe('warn');
  });

  it('stale beats a healthy body', () => {
    expect(relayHealth(status({ ageMs: 10 * 60_000, storage: { state: 'closed' } }), NOW).level).toBe('bad');
  });

  it('is unknown when no status could be read', () => {
    expect(relayHealth(null, NOW).level).toBe('unknown');
  });
});

describe('StatusDot', () => {
  it('renders red with an accessible name when updated_at is 4 minutes old', () => {
    render(StatusDot, { status: status({ ageMs: 4 * 60_000 }), nowMs: NOW });
    const dot = screen.getByRole('img');
    expect(dot.dataset.level).toBe('bad');
    expect(dot).toHaveAccessibleName(/stale/i);
    expect(dot.getAttribute('title')).toContain(iso(NOW - 4 * 60_000));
  });

  it('renders green when healthy', () => {
    render(StatusDot, { status: status(), nowMs: NOW });
    expect(screen.getByRole('img').dataset.level).toBe('ok');
  });
});
