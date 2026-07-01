// @vitest-environment jsdom

import '@testing-library/jest-dom/vitest';

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import App, { PROCESSING_POLL_INTERVAL_MS } from './App';
import { TOKEN_STORAGE_KEY } from './tokenStorage';

const okHealth = {
  status: 'ok',
  build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
  checks: { postgres: { status: 'ok' }, r2: { status: 'ok' } },
};

type ReadingFixture = Record<string, unknown>;

function reading(overrides: ReadingFixture = {}): ReadingFixture {
  return {
    id: 'reading-1',
    url: 'https://example.com/article',
    status: 'ready',
    title: 'Example article',
    site: 'Example',
    summary: 'A concise summary.',
    tags: ['go', 'reading'],
    created_at: '2026-06-28T12:00:00Z',
    updated_at: '2026-06-28T12:10:00Z',
    ...overrides,
  };
}

function detailReading(overrides: ReadingFixture = {}): ReadingFixture {
  return {
    ...reading(),
    similar_json: [{ id: 'reading-2', score: 0.87, title: 'Related reading', url: 'https://example.com/related' }],
    diagnostics_json: {
      source: 'fetch',
      extraction_mode: 'readability',
      similar_count: 1,
      timings_ms: { fetch: 12.5, extract: 8 },
    },
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status });
}

function readingsResponse(readings: ReadingFixture[], options: { total?: number } = {}) {
  return jsonResponse({ readings, total: options.total ?? readings.length });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((innerResolve) => {
    resolve = innerResolve;
  });
  return { promise, resolve };
}

function fetchRoutes(handlers: {
  readings?: () => Response | Promise<Response>;
  health?: () => Response | Promise<Response>;
  detail?: (id: string) => Response | Promise<Response>;
  content?: (id: string) => Response | Promise<Response>;
  raw?: (id: string) => Response | Promise<Response>;
} = {}) {
  const { readings = () => readingsResponse([]), health = () => jsonResponse(okHealth), detail, content, raw } = handlers;
  return vi.fn(async (input: string) => {
    const url = new URL(input);
    if (url.pathname === '/api/healthz') {
      return health();
    }
    if (url.pathname === '/api/readings') {
      return readings();
    }
    const contentMatch = url.pathname.match(/^\/api\/readings\/([^/]+)\/content$/);
    if (contentMatch) {
      if (!content) {
        throw new Error(`unexpected content request: ${input}`);
      }
      return content(contentMatch[1]);
    }
    const rawMatch = url.pathname.match(/^\/api\/readings\/([^/]+)\/raw$/);
    if (rawMatch) {
      if (!raw) {
        throw new Error(`unexpected raw request: ${input}`);
      }
      return raw(rawMatch[1]);
    }
    const detailMatch = url.pathname.match(/^\/api\/readings\/([^/]+)$/);
    if (detailMatch) {
      if (!detail) {
        throw new Error(`unexpected detail request: ${input}`);
      }
      return detail(detailMatch[1]);
    }
    throw new Error(`unexpected request: ${input}`);
  });
}

describe('ReadingDetail', () => {
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  beforeEach(() => {
    localStorage.clear();
  });

  it('renders summary, tags, similar readings, and diagnostics timings, then fetches markdown content for a ready reading', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => new Response('# Heading\n\n- one\n- two', { status: 200 }),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    const detail = within(await screen.findByRole('region', { name: 'Reading detail' }));
    expect(await detail.findByRole('heading', { name: 'Example article', level: 3 })).toBeInTheDocument();
    expect(detail.getByText('A concise summary.')).toBeInTheDocument();
    expect(detail.getByText('Related reading')).toBeInTheDocument();
    expect(detail.getByText('fetch')).toBeInTheDocument();
    expect(detail.getByText('12.5')).toBeInTheDocument();
    expect(detail.getByText('extract')).toBeInTheDocument();

    expect(await detail.findByRole('heading', { name: 'Heading', level: 1 })).toBeInTheDocument();
    expect(detail.getByText('one')).toBeInTheDocument();
    expect(detail.getByText('two')).toBeInTheDocument();
  });

  it('shows a processing placeholder for a running reading and does not fetch content', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading({ status: 'running' })]),
      detail: () => jsonResponse(detailReading({ status: 'running' })),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('Processing...')).toBeInTheDocument();
    expect(fetchImpl.mock.calls.some(([url]) => String(url).includes('/content'))).toBe(false);
  });

  it('shows the persisted error message for a failed reading', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading({ status: 'failed' })]),
      detail: () => jsonResponse(detailReading({ status: 'failed', error: 'extraction failed: timeout' })),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('extraction failed: timeout')).toBeInTheDocument();
  });

  it('falls back to stale_reason for a failed reading with an empty error', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading({ status: 'failed' })]),
      detail: () =>
        jsonResponse(
          detailReading({ status: 'failed', error: '', stale_reason: 'processing stalled after 10m0s' }),
        ),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('processing stalled after 10m0s')).toBeInTheDocument();
  });

  it('renders without crashing when similar_json and diagnostics_json are both absent', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(reading()),
      content: () => new Response('Body text.', { status: 200 }),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('Body text.')).toBeInTheDocument();
    expect(screen.queryByText('Similar readings')).not.toBeInTheDocument();
    expect(screen.queryByText('Timings (ms)')).not.toBeInTheDocument();
  });

  it('renders reading B when B is selected while reading A detail fetch is still in flight', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const readingA = reading({ id: 'reading-a', title: 'Reading A' });
    const readingB = reading({ id: 'reading-b', title: 'Reading B' });
    const detailA = deferred<Response>();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([readingA, readingB]),
      detail: (id) => {
        if (id === 'reading-a') {
          return detailA.promise;
        }
        return jsonResponse(detailReading({ id: 'reading-b', title: 'Reading B' }));
      },
      content: () => new Response('Body content for B', { status: 200 }),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Reading A' }));
    await user.click(await screen.findByRole('button', { name: 'Reading B' }));

    expect(await screen.findByRole('heading', { name: 'Reading B', level: 3 })).toBeInTheDocument();

    await act(async () => {
      detailA.resolve(jsonResponse(detailReading({ id: 'reading-a', title: 'Reading A' })));
      await detailA.promise;
    });

    expect(screen.getByRole('heading', { name: 'Reading B', level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Reading A', level: 3 })).not.toBeInTheDocument();
  });

  it('clears the open reading detail and content when the bearer token is cleared', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => new Response('# Heading\n\nSecret body text.', { status: 200 }),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));
    expect(await screen.findByRole('region', { name: 'Reading detail' })).toBeInTheDocument();
    expect(await screen.findByText('Secret body text.')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Clear token' }));

    expect(screen.queryByRole('region', { name: 'Reading detail' })).not.toBeInTheDocument();
    expect(screen.queryByText('Secret body text.')).not.toBeInTheDocument();
  });

  it('shows a friendly empty state when extracted content is missing', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => jsonResponse({ error: { code: 'not_found', message: 'blob not found' } }, 404),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('No content available.')).toBeInTheDocument();
  });

  it('shows a distinct content-fetch error with a working retry affordance', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    let contentCalls = 0;
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => {
        contentCalls += 1;
        if (contentCalls === 1) {
          return jsonResponse({ error: { code: 'internal', message: 'upstream exploded' } }, 500);
        }
        return new Response('Recovered content', { status: 200 });
      },
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));

    expect(await screen.findByText('upstream exploded')).toBeInTheDocument();
    expect(screen.queryByText('No content available.')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Retry' }));

    expect(await screen.findByText('Recovered content')).toBeInTheDocument();
  });

  it('discards a stale retry response when a second, faster retry has already superseded it', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    let contentCalls = 0;
    const firstRetry = deferred<Response>();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => {
        contentCalls += 1;
        if (contentCalls === 1) {
          return jsonResponse({ error: { code: 'internal', message: 'upstream exploded' } }, 500);
        }
        if (contentCalls === 2) {
          return firstRetry.promise;
        }
        return new Response('Second retry content', { status: 200 });
      },
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await user.click(await screen.findByRole('button', { name: 'Example article' }));
    expect(await screen.findByText('upstream exploded')).toBeInTheDocument();

    // Fire two retries back to back, before React re-renders in response to the first,
    // so each click hits the still-mounted button. This is what previously made both
    // retries share the same stale-response guard value.
    const retryButton = screen.getByRole('button', { name: 'Retry' });
    act(() => {
      fireEvent.click(retryButton);
      fireEvent.click(retryButton);
    });

    expect(await screen.findByText('Second retry content')).toBeInTheDocument();

    await act(async () => {
      firstRetry.resolve(new Response('First retry content', { status: 200 }));
      await firstRetry.promise;
    });

    expect(screen.getByText('Second retry content')).toBeInTheDocument();
    expect(screen.queryByText('First retry content')).not.toBeInTheDocument();
  });

  it('downloads raw source via an object URL and revokes it afterward', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const bytes = new Uint8Array([1, 2, 3]);
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => new Response('Body text.', { status: 200 }),
      raw: () =>
        new Response(bytes, {
          status: 200,
          headers: { 'Content-Disposition': 'attachment; filename="raw-content"' },
        }),
    });

    const originalCreateObjectURL = URL.createObjectURL;
    const originalRevokeObjectURL = URL.revokeObjectURL;
    const createObjectURL = vi.fn(() => 'blob:mock-url');
    const revokeObjectURL = vi.fn();
    URL.createObjectURL = createObjectURL;
    URL.revokeObjectURL = revokeObjectURL;
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});

    try {
      render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

      await user.click(await screen.findByRole('button', { name: 'Example article' }));
      await user.click(await screen.findByRole('button', { name: 'Download raw source' }));

      await waitFor(() => expect(createObjectURL).toHaveBeenCalledTimes(1));
      expect(clickSpy).toHaveBeenCalledTimes(1);
      expect(revokeObjectURL).toHaveBeenCalledWith('blob:mock-url');
    } finally {
      clickSpy.mockRestore();
      URL.createObjectURL = originalCreateObjectURL;
      URL.revokeObjectURL = originalRevokeObjectURL;
    }
  });

  it('revokes the object URL even when the download click throws', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const bytes = new Uint8Array([1, 2, 3]);
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => new Response('Body text.', { status: 200 }),
      raw: () =>
        new Response(bytes, {
          status: 200,
          headers: { 'Content-Disposition': 'attachment; filename="raw-content"' },
        }),
    });

    const originalCreateObjectURL = URL.createObjectURL;
    const originalRevokeObjectURL = URL.revokeObjectURL;
    const createObjectURL = vi.fn(() => 'blob:mock-url');
    const revokeObjectURL = vi.fn();
    URL.createObjectURL = createObjectURL;
    URL.revokeObjectURL = revokeObjectURL;
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
      throw new Error('blocked by browser policy');
    });

    try {
      render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

      await user.click(await screen.findByRole('button', { name: 'Example article' }));
      await user.click(await screen.findByRole('button', { name: 'Download raw source' }));

      await waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith('blob:mock-url'));
      expect(createObjectURL).toHaveBeenCalledTimes(1);
    } finally {
      clickSpy.mockRestore();
      URL.createObjectURL = originalCreateObjectURL;
      URL.revokeObjectURL = originalRevokeObjectURL;
    }
  });

  it('shows an inline error and does not trigger a download when the raw fetch fails', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading()]),
      detail: () => jsonResponse(detailReading()),
      content: () => new Response('Body text.', { status: 200 }),
      raw: () => jsonResponse({ error: { code: 'not_found', message: 'blob not found' } }, 404),
    });

    const originalCreateObjectURL = URL.createObjectURL;
    const createObjectURL = vi.fn(() => 'blob:mock-url');
    URL.createObjectURL = createObjectURL;

    try {
      render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

      await user.click(await screen.findByRole('button', { name: 'Example article' }));
      await user.click(await screen.findByRole('button', { name: 'Download raw source' }));

      expect(await screen.findByText('blob not found')).toBeInTheDocument();
      expect(createObjectURL).not.toHaveBeenCalled();
    } finally {
      URL.createObjectURL = originalCreateObjectURL;
    }
  });

  it('polls a selected running reading until ready, independent of the list-level poll', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    let detailCalls = 0;
    const fetchImpl = fetchRoutes({
      readings: () => readingsResponse([reading({ status: 'ready' })]),
      detail: () => {
        detailCalls += 1;
        if (detailCalls === 1) {
          return jsonResponse(detailReading({ status: 'running' }));
        }
        return jsonResponse(detailReading({ status: 'ready' }));
      },
      content: () => new Response('Ready content', { status: 200 }),
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'Example article' }));
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.getByText('Processing...')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(screen.getByText('Ready content')).toBeInTheDocument();
  });
});
