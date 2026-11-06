// Authenticated, schema-validated access to the same-origin zview API. All
// requests carry the K_api Bearer credential, which never leaves the worker.
import type { QueryValue } from './protocol';
import {
  SchemaError,
  cap,
  parseCameras,
  parseClipList,
  parseCoverage,
  parseSidecar,
  parseStatus,
  isUlid,
} from './schemas';

export class ApiError extends Error {
  override name = 'ApiError';
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** No key is loaded, so no Bearer credential can be formed. */
export class NoKeyError extends Error {
  override name = 'ErrNoKey';
  constructor() {
    super('no key loaded');
  }
}

export type AuthFetch = (url: string, init?: RequestInit) => Promise<Response>;

/** An `AuthFetch` whose pinned-token memory can be dropped (on Lock and Forget). */
export type ResettableAuthFetch = AuthFetch & { reset(): void };

const MAX_JSON_BYTES = 16 << 20;
const MAX_ERROR_BYTES = 4096;

/**
 * Wraps fetch with `Authorization: Bearer`. With several keys it tries the
 * newest token first and falls back to older ones on 401 or 429 (rotation can
 * leave the server accepting only an older key's token, and while the 401 rate
 * limit holds the server answers 429 whichever token is sent). The token that
 * last got an answer other than 401 or 429 is tried first from then on, so one
 * unrelated key imported last does not draw a 401 on every request (ui-spec
 * §3.2 rate-limits 401s). `reset()` forgets that token: it is derived key
 * material, so Lock and Forget must not leave it in memory.
 */
export function makeAuthFetch(fetchFn: typeof fetch, tokens: () => Promise<string[]>): ResettableAuthFetch {
  let working: string | undefined;
  const authFetch: AuthFetch = async (url, init = {}) => {
    const list = await tokens();
    if (list.length === 0) throw new NoKeyError();
    // `tokens()` is already newest first.
    const order =
      working !== undefined && list.includes(working) ? [working, ...list.filter((t) => t !== working)] : list;
    let res: Response | undefined;
    for (const token of order) {
      const headers = new Headers(init.headers);
      headers.set('Authorization', `Bearer ${token}`);
      res = await fetchFn(url, {
        ...init,
        headers,
        credentials: 'omit',
        redirect: 'error',
        referrerPolicy: 'no-referrer',
      });
      if (res.status !== 401 && res.status !== 429) {
        working = token;
        return res;
      }
      void res.body?.cancel();
    }
    return res!;
  };
  return Object.assign(authFetch, {
    reset() {
      working = undefined;
    },
  });
}

/**
 * Reads a body of at most `max` bytes. A larger Content-Length is refused before
 * any byte is read, and a body that outgrows the cap (or lies about its length)
 * is cancelled as soon as the cap is crossed, so memory stays bounded.
 */
export async function readBounded(res: Response, max: number, tooBig: () => Error): Promise<Uint8Array> {
  const declared = res.headers.get('Content-Length');
  if (declared !== null && /^\d+$/.test(declared) && Number(declared) > max) {
    void res.body?.cancel();
    throw tooBig();
  }
  if (!res.body) return new Uint8Array(0);
  const reader = res.body.getReader();
  const parts: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.length;
    if (total > max) {
      void reader.cancel();
      throw tooBig();
    }
    parts.push(value);
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

type Parser = (json: unknown) => unknown;

function routeFor(path: string): Parser | null {
  switch (path) {
    case '/api/v1/status':
      return parseStatus;
    case '/api/v1/cameras':
      return parseCameras;
    case '/api/v1/clips':
      return parseClipList;
    case '/api/v1/coverage':
      return parseCoverage;
  }
  const m = /^\/api\/v1\/clips\/([^/]+)$/.exec(path);
  if (m && isUlid(m[1])) return parseSidecar;
  return null;
}

function buildQuery(query: Readonly<Record<string, QueryValue>>): string {
  const p = new URLSearchParams();
  for (const [k, val] of Object.entries(query)) {
    if (!/^[a-z_]{1,32}$/.test(k)) throw new SchemaError(`bad query parameter name`);
    for (const x of typeof val === 'string' ? [val] : val) {
      if (typeof x !== 'string' || x.length > 256) throw new SchemaError(`bad query parameter value`);
      p.append(k, x);
    }
  }
  const s = p.toString();
  return s ? `?${s}` : '';
}

async function errorFor(res: Response): Promise<ApiError> {
  let title = '';
  try {
    const body = await readBounded(res, MAX_ERROR_BYTES, () => new Error('error body too large'));
    const j = JSON.parse(new TextDecoder().decode(body)) as { title?: unknown };
    if (typeof j.title === 'string') title = cap(j.title);
  } catch {
    /* body is not problem+json */
  }
  return new ApiError(res.status, title || `request failed with status ${res.status}`);
}

/** Reads a response body as validated JSON. */
export async function readJson<T>(res: Response, parse: (json: unknown) => T): Promise<T> {
  const bytes = await readBounded(res, MAX_JSON_BYTES, () => new SchemaError('response too large'));
  let json: unknown;
  try {
    json = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    throw new SchemaError('response is not JSON');
  }
  return parse(json);
}

/** `api.get {path, query}`: only known /api/v1 routes are fetched, and only same-origin. */
export async function apiGet(
  authFetch: AuthFetch,
  path: string,
  query: Readonly<Record<string, QueryValue>> = {},
  signal?: AbortSignal,
): Promise<unknown> {
  const parse = typeof path === 'string' ? routeFor(path) : null;
  if (!parse) throw new SchemaError('unknown API path');
  const res = await authFetch(path + buildQuery(query), { method: 'GET', signal });
  if (!res.ok) throw await errorFor(res);
  return readJson(res, parse);
}

/** Fetches and validates one clip's sidecar. */
export async function fetchSidecar(authFetch: AuthFetch, clipId: string, signal?: AbortSignal) {
  if (!isUlid(clipId)) throw new SchemaError('invalid clip id');
  const res = await authFetch(`/api/v1/clips/${clipId}`, { method: 'GET', signal });
  if (!res.ok) throw await errorFor(res);
  return readJson(res, parseSidecar);
}
