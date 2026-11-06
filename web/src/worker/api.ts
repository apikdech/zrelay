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

const MAX_JSON_CHARS = 16 << 20;

/**
 * Wraps fetch with `Authorization: Bearer`. With several keys it tries the
 * newest token first and falls back to older ones on 401 (rotation can leave
 * the server accepting only an older key's token).
 */
export function makeAuthFetch(fetchFn: typeof fetch, tokens: () => Promise<string[]>): AuthFetch {
  return async (url, init = {}) => {
    const list = await tokens();
    if (list.length === 0) throw new NoKeyError();
    let res: Response | undefined;
    for (const token of list) {
      const headers = new Headers(init.headers);
      headers.set('Authorization', `Bearer ${token}`);
      res = await fetchFn(url, {
        ...init,
        headers,
        credentials: 'omit',
        redirect: 'error',
        referrerPolicy: 'no-referrer',
      });
      if (res.status !== 401) return res;
      void res.body?.cancel();
    }
    return res!;
  };
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
    const j = JSON.parse((await res.text()).slice(0, 4096)) as { title?: unknown };
    if (typeof j.title === 'string') title = cap(j.title);
  } catch {
    /* body is not problem+json */
  }
  return new ApiError(res.status, title || `request failed with status ${res.status}`);
}

/** Reads a response body as validated JSON. */
export async function readJson<T>(res: Response, parse: (json: unknown) => T): Promise<T> {
  const text = await res.text();
  if (text.length > MAX_JSON_CHARS) throw new SchemaError('response too large');
  let json: unknown;
  try {
    json = JSON.parse(text);
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
