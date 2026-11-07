import { describe, expect, it } from 'vitest';
import { makeAuthFetch } from './api';

/** A fetch that answers per Bearer token and records the order tried. */
function fakeFetch(answers: Record<string, () => number>) {
  const tried: string[] = [];
  const fetchFn = (async (_url: string, init?: RequestInit) => {
    const token = (new Headers(init?.headers).get('Authorization') ?? '').replace('Bearer ', '');
    tried.push(token);
    const status = answers[token]?.() ?? 401;
    return new Response(status === 200 ? '{}' : '', { status });
  }) as typeof fetch;
  return { fetchFn, tried };
}

describe('makeAuthFetch', () => {
  it('tries the remaining tokens when the newest one draws 429, and never pins the limited one', async () => {
    // The server rate-limits 401s per IP; while the limit holds, the newest token
    // is answered 429 and the older, accepted key must still be tried.
    const { fetchFn, tried } = fakeFetch({ bad: () => 429, good: () => 200 });
    const authFetch = makeAuthFetch(fetchFn, async () => ['bad', 'good']);
    const statuses: number[] = [];
    for (let i = 0; i < 3; i++) statuses.push((await authFetch('/api/v1/status')).status);
    expect(statuses).toEqual([200, 200, 200]);
    // "good" is pinned after the first call, so "bad" is probed only once.
    expect(tried).toEqual(['bad', 'good', 'good', 'good']);
  });

  it('does not pin a token that was answered 429 when every token is limited', async () => {
    let limited = true;
    const answer = () => (limited ? 429 : 200);
    const { fetchFn, tried } = fakeFetch({ a: answer, b: answer });
    const authFetch = makeAuthFetch(fetchFn, async () => ['a', 'b']);
    expect((await authFetch('/x')).status).toBe(429);
    limited = false;
    expect((await authFetch('/x')).status).toBe(200);
    expect(tried).toEqual(['a', 'b', 'a']);
  });

  it('reset() forgets the pinned token', async () => {
    const { fetchFn, tried } = fakeFetch({ new: () => 401, old: () => 200 });
    const authFetch = makeAuthFetch(fetchFn, async () => ['new', 'old']);
    await authFetch('/x'); // new (401), old (200): pins "old"
    await authFetch('/x'); // pinned: old only
    authFetch.reset();
    await authFetch('/x'); // newest first again
    expect(tried).toEqual(['new', 'old', 'old', 'new', 'old']);
  });
});
