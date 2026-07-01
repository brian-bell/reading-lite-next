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

  it('fetches a reading detail document with all optional fields present', async () => {
    const detail = {
      id: 'reading-1',
      url: 'https://example.com/article',
      status: 'ready',
      title: 'Example article',
      site: 'Example',
      summary: 'A concise summary.',
      tags: ['go', 'reading'],
      word_count: 512,
      summary_json: { key_points: ['one'] },
      similar_json: [{ id: 'reading-2', score: 0.91, title: 'Related', url: 'https://example.com/related' }],
      diagnostics_json: {
        source: 'fetch',
        extraction_mode: 'readability',
        similar_count: 1,
        reused: false,
        timings_ms: { fetch: 12.5, extract: 8 },
      },
      created_at: '2026-06-28T12:00:00Z',
      updated_at: '2026-06-28T12:10:00Z',
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(detail), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    await expect(client.getReading({ token: 'stored-token', id: 'reading-1' })).resolves.toEqual(detail);
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings/reading-1', {
      headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' },
    });
  });

  it('fetches a reading detail document with all optional fields absent', async () => {
    const detail = {
      id: 'reading-1',
      url: 'https://example.com/article',
      status: 'pending',
      created_at: '2026-06-28T12:00:00Z',
      updated_at: '2026-06-28T12:10:00Z',
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(detail), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getReading({ token: 'stored-token', id: 'reading-1' })).resolves.toEqual({
      ...detail,
      similar_json: [],
    });
  });

  it('normalizes null tags and null similar_json on a reading detail document', async () => {
    const detail = {
      id: 'reading-1',
      url: 'https://example.com/article',
      status: 'ready',
      tags: null,
      similar_json: null,
      created_at: '2026-06-28T12:00:00Z',
      updated_at: '2026-06-28T12:10:00Z',
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(detail), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getReading({ token: 'stored-token', id: 'reading-1' })).resolves.toEqual({
      ...detail,
      tags: [],
      similar_json: [],
    });
  });

  it('rejects a 404 reading detail response as an APIError', async () => {
    const fetchImpl = vi.fn(
      async () => new Response(JSON.stringify({ error: { code: 'not_found', message: 'reading not found' } }), { status: 404 }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getReading({ token: 'stored-token', id: 'missing' })).rejects.toMatchObject({
      code: 'not_found',
      message: 'reading not found',
      status: 404,
    });
  });

  it('rejects an unauthorized reading detail response as an APIError', async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'unauthorized', message: 'missing or invalid bearer token' } }), {
          status: 401,
        }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getReading({ token: 'bad-token', id: 'reading-1' })).rejects.toMatchObject({
      code: 'unauthorized',
      status: 401,
    });
  });

  it('rejects a malformed reading detail body', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({ id: 42 }), { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getReading({ token: 'stored-token', id: 'reading-1' })).rejects.toMatchObject({
      code: 'invalid_response',
      message: 'API response was not a reading detail document',
      status: 200,
    });
  });

  it('fetches extracted content as raw text', async () => {
    const fetchImpl = vi.fn(async () => new Response('# Heading\n\nBody text.', { status: 200 }));
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    await expect(client.getContent({ token: 'stored-token', id: 'reading-1' })).resolves.toBe('# Heading\n\nBody text.');
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings/reading-1/content', {
      headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' },
    });
  });

  it('rejects a 404 content response as not_found', async () => {
    const fetchImpl = vi.fn(
      async () => new Response(JSON.stringify({ error: { code: 'not_found', message: 'blob not found' } }), { status: 404 }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getContent({ token: 'stored-token', id: 'reading-1' })).rejects.toMatchObject({
      code: 'not_found',
      message: 'blob not found',
      status: 404,
    });
  });

  it('falls back to an http_error for a non-JSON content error body', async () => {
    const fetchImpl = vi.fn(async () => new Response('upstream exploded', { status: 502, statusText: 'Bad Gateway' }));
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getContent({ token: 'stored-token', id: 'reading-1' })).rejects.toMatchObject({
      code: 'http_error',
      message: 'Request failed with status 502',
      status: 502,
    });
  });

  it('fetches a raw blob download', async () => {
    const bytes = new Uint8Array([1, 2, 3, 4]);
    const fetchImpl = vi.fn(
      async () =>
        new Response(bytes, {
          status: 200,
          headers: {
            'Content-Type': 'application/octet-stream',
            'Content-Disposition': 'attachment; filename="raw-content"',
          },
        }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com/', fetchImpl });

    const result = await client.getRawBlob({ token: 'stored-token', id: 'reading-1' });
    expect(result.filename).toBe('raw-content');
    expect(result.blob).toBeInstanceOf(Blob);
    await expect(result.blob.arrayBuffer()).resolves.toEqual(bytes.buffer);
    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings/reading-1/raw', {
      headers: { Authorization: 'Bearer stored-token' },
    });
  });

  it('rejects a 404 raw blob response as not_found', async () => {
    const fetchImpl = vi.fn(
      async () => new Response(JSON.stringify({ error: { code: 'not_found', message: 'blob not found' } }), { status: 404 }),
    );
    const client = createAPIClient({ baseURL: 'https://api.example.com', fetchImpl });

    await expect(client.getRawBlob({ token: 'stored-token', id: 'reading-1' })).rejects.toMatchObject({
      code: 'not_found',
      message: 'blob not found',
      status: 404,
    });
  });
});
