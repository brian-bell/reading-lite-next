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

  it('fetches authenticated readings from the configured base URL', async () => {
    const readings = {
      readings: [
        {
          id: 'reading-1',
          url: 'https://example.com/article',
          status: 'ready',
          title: 'Example article',
          site: 'Example',
          summary: 'A short summary.',
          tags: ['go', 'reading'],
          created_at: '2026-06-28T12:00:00Z',
          updated_at: '2026-06-28T12:10:00Z',
        },
      ],
      total: 1,
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(readings), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).resolves.toEqual(readings);
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings', {
      headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' },
    });
  });

  it('submits a URL with bearer auth to the configured base URL', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ id: 'reading-1', status: 'pending' }), { status: 201 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    await expect(client.submitURL({ token: ' secret ', url: 'https://example.com/post' })).resolves.toEqual({
      id: 'reading-1',
      status: 'pending',
    });
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings', {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        Authorization: 'Bearer secret',
      },
      body: JSON.stringify({ url: 'https://example.com/post' }),
    });
  });

  it('turns submit URL Go error envelopes into APIError values', async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'invalid_url', message: 'invalid reading url' } }), {
          status: 400,
        }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    const request = client.submitURL({ token: 'stored-token', url: 'not-a-url' });
    await expect(request).rejects.toBeInstanceOf(APIError);
    await expect(request).rejects.toMatchObject({
      code: 'invalid_url',
      message: 'invalid reading url',
      status: 400,
    });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('rejects malformed submit URL success responses', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ id: 42, status: 'queued' }), { status: 201 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.submitURL({ token: 'stored-token', url: 'https://example.com/post' })).rejects.toMatchObject({
      code: 'invalid_response',
      message: 'API response was not a submit URL document',
      status: 201,
    });
  });

  it('normalizes null reading tags from the Go API to an empty list', async () => {
    const readings = {
      readings: [
        {
          id: 'reading-1',
          url: 'https://example.com/article',
          status: 'pending',
          tags: null,
          created_at: '2026-06-28T12:00:00Z',
          updated_at: '2026-06-28T12:10:00Z',
        },
      ],
      total: 1,
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(readings), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).resolves.toEqual({
      readings: [{ ...readings.readings[0], tags: [] }],
      total: 1,
    });
  });

  it('serializes the optional readings cursor without dropping a base URL path prefix', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ readings: [], total: 0 }), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://host/prefix/', fetchImpl });

    await client.listReadings({ token: 'stored-token', cursor: 'next cursor' });

    expect(fetchImpl).toHaveBeenCalledWith('https://host/prefix/api/readings?cursor=next+cursor', {
      headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' },
    });
  });

  it('rejects missing API base URL for readings without calling fetch', async () => {
    const fetchImpl = vi.fn();
    const client = createAPIClient({ baseURL: '', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).rejects.toMatchObject({
      code: 'missing_config',
      message: 'VITE_READER_API_BASE_URL is required',
    });
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it('turns readings Go error envelopes into APIError values', async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response(
          JSON.stringify({ error: { code: 'unauthorized', message: 'missing or invalid bearer token' } }),
          { status: 401 },
        ),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).rejects.toBeInstanceOf(APIError);
    await expect(client.listReadings({ token: 'stored-token' })).rejects.toMatchObject({
      code: 'unauthorized',
      message: 'missing or invalid bearer token',
      status: 401,
    });
  });

  it('rejects non-OK readings responses even when the body looks like a list document', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ readings: [], total: 0 }), { status: 500 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).rejects.toMatchObject({
      code: 'http_error',
      message: 'Request failed with status 500',
      status: 500,
    });
  });

  it('rejects successful readings responses that are not list documents', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ readings: [{ id: 'missing-fields' }] }), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.listReadings({ token: 'stored-token' })).rejects.toMatchObject({
      code: 'invalid_response',
      message: 'API response was not a readings list document',
      status: 200,
    });
  });
});
