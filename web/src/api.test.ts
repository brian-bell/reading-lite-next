import { describe, expect, it, vi } from 'vitest';

import { APIError, createAPIClient, resolveAPIBaseURL } from './api';

describe('resolveAPIBaseURL', () => {
  it('trims trailing slashes from VITE_READER_API_BASE_URL', () => {
    expect(resolveAPIBaseURL({ VITE_READER_API_BASE_URL: 'https://api.example.com///' })).toBe(
      'https://api.example.com',
    );
  });
});

describe('createAPIClient', () => {
  it('fetches the API health document from the configured base URL', async () => {
    const health = {
      status: 'ok',
      build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
      checks: { postgres: { status: 'ok' }, r2: { status: 'ok' } },
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(health), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    await expect(client.health()).resolves.toEqual(health);
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/healthz', {
      headers: { Accept: 'application/json' },
    });
  });

  it('returns degraded health documents even when healthz uses a 503 status', async () => {
    const health = {
      status: 'degraded',
      build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
      checks: { postgres: { status: 'error', error: 'unavailable' }, r2: { status: 'ok' } },
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(health), { status: 503 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.health()).resolves.toEqual(health);
  });

  it('rejects missing API base URL without calling fetch', async () => {
    const fetchImpl = vi.fn();
    const client = createAPIClient({ baseURL: '', fetchImpl });

    await expect(client.health()).rejects.toMatchObject({
      code: 'missing_config',
      message: 'VITE_READER_API_BASE_URL is required',
    });
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it('turns Go error envelopes into APIError values', async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'unauthorized', message: 'missing token' } }), {
          status: 401,
        }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.health()).rejects.toBeInstanceOf(APIError);
    await expect(client.health()).rejects.toMatchObject({
      code: 'unauthorized',
      message: 'missing token',
      status: 401,
    });
  });

  it('reports malformed error responses with an HTTP fallback', async () => {
    const fetchImpl = vi.fn(async () => new Response('not-json', { status: 502, statusText: 'Bad Gateway' }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.health()).rejects.toMatchObject({
      code: 'http_error',
      message: 'Request failed with status 502',
      status: 502,
    });
  });

  it('rejects successful responses that are not health documents', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({}), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.health()).rejects.toMatchObject({
      code: 'invalid_response',
      message: 'API response was not a health document',
      status: 200,
    });
  });

  it('rejects successful health documents with malformed check values', async () => {
    const malformedHealth = {
      status: 'ok',
      build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
      checks: { postgres: null },
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(malformedHealth), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.health()).rejects.toMatchObject({
      code: 'invalid_response',
      message: 'API response was not a health document',
      status: 200,
    });
  });
});
