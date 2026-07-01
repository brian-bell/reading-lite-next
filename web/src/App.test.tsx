// @vitest-environment jsdom

import '@testing-library/jest-dom/vitest';

import { StrictMode } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import App, { PROCESSING_POLL_INTERVAL_MS } from './App';
import { TOKEN_STORAGE_KEY } from './tokenStorage';

const okHealth = {
  status: 'ok',
  build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
  checks: { postgres: { status: 'ok' }, r2: { status: 'ok' } },
};

const degradedHealth = {
  status: 'degraded',
  build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
  checks: { postgres: { status: 'error', error: 'unavailable' }, r2: { status: 'ok' } },
};

type ReadingFixture = {
  id: string;
  url: string;
  status: 'pending' | 'running' | 'ready' | 'failed';
  title?: string;
  site?: string;
  summary?: string;
  error?: string;
  tags?: string[];
  created_at: string;
  updated_at: string;
};

function reading(overrides: Partial<ReadingFixture> = {}): ReadingFixture {
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

function readingsResponse(readings: ReadingFixture[], options: { total?: number; nextCursor?: string } = {}) {
  return new Response(
    JSON.stringify({
      readings,
      total: options.total ?? readings.length,
      ...(options.nextCursor ? { next_cursor: options.nextCursor } : {}),
    }),
    { status: 200 },
  );
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status });
}

function fetchRoutes(
  readingsHandler: (url: string, init?: RequestInit) => Response | Promise<Response>,
  healthHandler: () => Response | Promise<Response> = () => jsonResponse(okHealth),
) {
  return vi.fn(async (input: string, init?: RequestInit) => {
    if (input.endsWith('/api/healthz')) {
      return healthHandler();
    }
    if (input.includes('/api/readings')) {
      return readingsHandler(input, init);
    }
    throw new Error(`unexpected request: ${input}`);
  });
}

function readingCalls(fetchImpl: ReturnType<typeof vi.fn>) {
  return fetchImpl.mock.calls.filter(([url]) => String(url).includes('/api/readings'));
}

function healthCalls(fetchImpl: ReturnType<typeof vi.fn>) {
  return fetchImpl.mock.calls.filter(([url]) => String(url).includes('/api/healthz'));
}

function submitCalls(fetchImpl: ReturnType<typeof vi.fn>) {
  return readingCalls(fetchImpl).filter(([, init]) => init?.method === 'POST');
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((innerResolve) => {
    resolve = innerResolve;
  });
  return { promise, resolve };
}

describe('App', () => {
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  beforeEach(() => {
    localStorage.clear();
  });

  it('loads, saves, and clears the bearer token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes(() => readingsResponse([]));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    const tokenInput = screen.getByLabelText('Bearer token');
    expect(tokenInput).toHaveValue('stored-token');

    await user.clear(tokenInput);
    await user.type(tokenInput, 'updated-token');
    await user.click(screen.getByRole('button', { name: 'Save token' }));
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('updated-token');

    await user.click(screen.getByRole('button', { name: 'Clear token' }));
    expect(tokenInput).toHaveValue('');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });

  it('loads health and authenticated readings on mount', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(() => readingsResponse([reading()]));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API status: ok')).toBeInTheDocument();
    expect(await screen.findByText('Example article')).toBeInTheDocument();
    expect(screen.getByText('Example')).toBeInTheDocument();
    expect(screen.getByText('A concise summary.')).toBeInTheDocument();
    expect(screen.getByText('go')).toBeInTheDocument();
    expect(screen.getByText('reading')).toBeInTheDocument();
    expect(screen.getByText('Total readings: 1')).toBeInTheDocument();

    expect(fetchImpl).toHaveBeenCalledWith('https://api.example.com/api/readings', {
      headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' },
    });
  });

  it('deduplicates the initial authenticated readings request under StrictMode', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(() => readingsResponse([reading({ title: 'Strict mode article' })]));

    render(
      <StrictMode>
        <App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />
      </StrictMode>,
    );

    expect(await screen.findByText('Strict mode article')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(1);
  });

  it('loads health on mount and refreshes without sending Authorization', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    let healthCount = 0;
    const fetchImpl = fetchRoutes(
      () => readingsResponse([]),
      () => jsonResponse(healthCount++ === 0 ? okHealth : degradedHealth, healthCount === 1 ? 200 : 503),
    );

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API status: ok')).toBeInTheDocument();
    expect(screen.getByText('Version: dev')).toBeInTheDocument();
    expect(screen.getByText('postgres: ok')).toBeInTheDocument();
    expect(screen.getByText('r2: ok')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Refresh health' }));

    expect(await screen.findByText('API status: degraded')).toBeInTheDocument();
    expect(healthCalls(fetchImpl)).toHaveLength(2);
    for (const call of healthCalls(fetchImpl)) {
      expect(call[0]).toBe('https://api.example.com/api/healthz');
      expect(call[1]).toEqual({ headers: { Accept: 'application/json' } });
    }
  });

  it('displays health errors without clearing the saved token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(
      () => readingsResponse([]),
      () => jsonResponse({ error: { code: 'unavailable', message: 'upstream down' } }, 503),
    );

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('upstream down')).toBeInTheDocument();
    expect(screen.getByLabelText('Bearer token')).toHaveValue('stored-token');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('stored-token');
  });

  it('renders malformed health documents as an error', async () => {
    const fetchImpl = fetchRoutes(() => readingsResponse([]), () => jsonResponse({}));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API response was not a health document')).toBeInTheDocument();
  });

  it('renders malformed health check values as an error', async () => {
    const malformedHealth = {
      status: 'ok',
      build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
      checks: { postgres: null },
    };
    const fetchImpl = fetchRoutes(() => readingsResponse([]), () => jsonResponse(malformedHealth));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API response was not a health document')).toBeInTheDocument();
  });

  it('renders an empty reading list without treating it as an error', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(() => readingsResponse([], { total: 0 }));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('No readings yet.')).toBeInTheDocument();
    expect(screen.getByText('Total readings: 0')).toBeInTheDocument();
    expect(screen.queryByText('Unable to load reading list')).not.toBeInTheDocument();
  });

  it('saves a changed token, trims it, and reloads readings with the new token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'old-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((_url, init) => {
      const authorization = (init?.headers as Record<string, string>).Authorization;
      if (authorization === 'Bearer new-token') {
        return readingsResponse([reading({ id: 'new', title: 'New token article' })]);
      }
      return readingsResponse([reading({ id: 'old', title: 'Old token article' })]);
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Old token article')).toBeInTheDocument();

    const tokenInput = screen.getByLabelText('Bearer token');
    await user.clear(tokenInput);
    await user.type(tokenInput, '  new-token  ');
    await user.click(screen.getByRole('button', { name: 'Save token' }));

    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('new-token');
    expect(await screen.findByText('New token article')).toBeInTheDocument();
    expect(screen.queryByText('Old token article')).not.toBeInTheDocument();
    expect(readingCalls(fetchImpl).at(-1)?.[1]).toEqual({
      headers: { Accept: 'application/json', Authorization: 'Bearer new-token' },
    });
  });

  it('hides previous readings when a newly saved token fails to load', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'old-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((_url, init) => {
      const authorization = (init?.headers as Record<string, string>).Authorization;
      if (authorization === 'Bearer new-token') {
        return jsonResponse({ error: { code: 'unauthorized', message: 'missing or invalid bearer token' } }, 401);
      }
      return readingsResponse([reading({ title: 'Old private article' })]);
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Old private article')).toBeInTheDocument();

    const tokenInput = screen.getByLabelText('Bearer token');
    await user.clear(tokenInput);
    await user.type(tokenInput, 'new-token');
    await user.click(screen.getByRole('button', { name: 'Save token' }));

    expect(await screen.findByText('missing or invalid bearer token')).toBeInTheDocument();
    expect(screen.queryByText('Old private article')).not.toBeInTheDocument();
  });

  it('skips readings when the saved token is blank while health still loads', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, '   ');
    const fetchImpl = fetchRoutes(() => readingsResponse([reading()]));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API status: ok')).toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(0);
  });

  it('clearing the token removes authenticated reading data and skips new list requests', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes(() => readingsResponse([reading({ title: 'Private article' })]));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Private article')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Clear token' }));

    expect(screen.queryByText('Private article')).not.toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(1);
  });

  it('ignores in-flight readings from an old token after the token is cleared', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const pendingReadings = deferred<Response>();
    const fetchImpl = fetchRoutes(() => pendingReadings.promise);

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await waitFor(() => expect(readingCalls(fetchImpl)).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: 'Clear token' }));

    await act(async () => {
      pendingReadings.resolve(readingsResponse([reading({ title: 'Stale article' })]));
      await pendingReadings.promise;
    });

    expect(screen.queryByText('Stale article')).not.toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();
  });

  it('renders readings error envelopes without clearing the token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(() =>
      jsonResponse({ error: { code: 'unauthorized', message: 'missing or invalid bearer token' } }, 401),
    );

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByRole('alert', { name: '' })).toHaveTextContent('missing or invalid bearer token');
    expect(screen.getByLabelText('Bearer token')).toHaveValue('stored-token');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('stored-token');
  });

  it('loads additional pages with the next cursor and appends them', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=cursor-1')) {
        return readingsResponse([reading({ id: 'second', title: 'Second article' })], { total: 2 });
      }
      return readingsResponse([reading({ id: 'first', title: 'First article' })], { total: 2, nextCursor: 'cursor-1' });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('First article')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText('Second article')).toBeInTheDocument();
    expect(screen.getByText('First article')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
    expect(readingCalls(fetchImpl).map(([url]) => url)).toEqual([
      'https://api.example.com/api/readings',
      'https://api.example.com/api/readings?cursor=cursor-1',
    ]);
  });

  it('submits a URL and refreshes the visible reading list', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    let listLoads = 0;
    const fetchImpl = fetchRoutes((url, init) => {
      if (init?.method === 'POST') {
        return jsonResponse({ id: 'new-reading', status: 'pending' }, 201);
      }
      listLoads += 1;
      if (listLoads === 1) {
        return readingsResponse([reading({ id: 'old-reading', title: 'Existing article' })], { total: 1 });
      }
      return readingsResponse(
        [
          reading({ id: 'new-reading', title: 'Freshly submitted article', url: 'https://example.com/post' }),
          reading({ id: 'old-reading', title: 'Existing article' }),
        ],
        { total: 2 },
      );
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Existing article')).toBeInTheDocument();

    await user.type(screen.getByLabelText('URL'), 'https://example.com/post');
    await user.click(screen.getByRole('button', { name: 'Add reading' }));

    expect(await screen.findByText('Freshly submitted article')).toBeInTheDocument();
    expect(screen.getByText('Existing article')).toBeInTheDocument();
    expect(screen.getByLabelText('URL')).toHaveValue('');
    expect(screen.getByText('Submitted URL. Status: pending.')).toBeInTheDocument();

    expect(readingCalls(fetchImpl).map(([url, init]) => [url, init])).toEqual([
      [
        'https://api.example.com/api/readings',
        { headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' } },
      ],
      [
        'https://api.example.com/api/readings',
        {
          method: 'POST',
          headers: {
            Accept: 'application/json',
            'Content-Type': 'application/json',
            Authorization: 'Bearer stored-token',
          },
          body: JSON.stringify({ url: 'https://example.com/post' }),
        },
      ],
      [
        'https://api.example.com/api/readings',
        { headers: { Accept: 'application/json', Authorization: 'Bearer stored-token' } },
      ],
    ]);
  });

  it('shows submit errors without clearing existing readings', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((_url, init) => {
      if (init?.method === 'POST') {
        return jsonResponse({ error: { code: 'invalid_url', message: 'invalid reading url' } }, 400);
      }
      return readingsResponse([reading({ id: 'old-reading', title: 'Existing article' })], { total: 1 });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Existing article')).toBeInTheDocument();

    await user.type(screen.getByLabelText('URL'), 'https://bad.example/post');
    await user.click(screen.getByRole('button', { name: 'Add reading' }));

    expect(await screen.findByText('invalid reading url')).toBeInTheDocument();
    expect(screen.getByText('Existing article')).toBeInTheDocument();
    expect(screen.queryByText('Submitted URL. Status:')).not.toBeInTheDocument();
  });

  it('submits a URL and refreshes the same visible page depth with fresh cursors', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((url, init) => {
      if (init?.method === 'POST') {
        return jsonResponse({ id: 'new-reading', status: 'pending' }, 201);
      }
      if (url.endsWith('?cursor=old-page-2')) {
        return readingsResponse([reading({ id: 'second', title: 'Second visible article' })], {
          total: 2,
        });
      }
      if (url.endsWith('?cursor=fresh-page-2')) {
        return readingsResponse([reading({ id: 'first', title: 'First visible article' })], {
          total: 3,
          nextCursor: 'fresh-page-3',
        });
      }
      return readingCalls(fetchImpl).length < 3
        ? readingsResponse([reading({ id: 'first', title: 'First visible article' })], {
            total: 2,
            nextCursor: 'old-page-2',
          })
        : readingsResponse([reading({ id: 'new-reading', title: 'Freshly submitted article' })], {
            total: 3,
            nextCursor: 'fresh-page-2',
          });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('First visible article')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('Second visible article')).toBeInTheDocument();

    await user.type(screen.getByLabelText('URL'), 'https://example.com/post');
    await user.click(screen.getByRole('button', { name: 'Add reading' }));

    expect(await screen.findByText('Freshly submitted article')).toBeInTheDocument();
    expect(screen.getByText('First visible article')).toBeInTheDocument();
    expect(screen.queryByText('Second visible article')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Load more' })).toBeEnabled();
    expect(readingCalls(fetchImpl).map(([url, init]) => [url, init?.method ?? 'GET'])).toEqual([
      ['https://api.example.com/api/readings', 'GET'],
      ['https://api.example.com/api/readings?cursor=old-page-2', 'GET'],
      ['https://api.example.com/api/readings', 'POST'],
      ['https://api.example.com/api/readings', 'GET'],
      ['https://api.example.com/api/readings?cursor=fresh-page-2', 'GET'],
    ]);
  });

  it('prevents duplicate submit requests while a URL is being submitted', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const submitted = deferred<Response>();
    const fetchImpl = fetchRoutes((_url, init) => {
      if (init?.method === 'POST') {
        return submitted.promise;
      }
      return readingsResponse([reading({ id: 'old-reading', title: 'Existing article' })], { total: 1 });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Existing article')).toBeInTheDocument();
    await user.type(screen.getByLabelText('URL'), 'https://example.com/post');

    const button = screen.getByRole('button', { name: 'Add reading' });
    await user.click(button);
    await user.click(button);

    expect(button).toBeDisabled();
    expect(submitCalls(fetchImpl)).toHaveLength(1);

    await act(async () => {
      submitted.resolve(jsonResponse({ id: 'new-reading', status: 'pending' }, 201));
      await submitted.promise;
    });
  });

  it('ignores in-flight submit responses after the token is cleared', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const submitted = deferred<Response>();
    let listLoads = 0;
    const fetchImpl = fetchRoutes((_url, init) => {
      if (init?.method === 'POST') {
        return submitted.promise;
      }
      listLoads += 1;
      if (listLoads === 1) {
        return readingsResponse([reading({ id: 'old-reading', title: 'Existing article' })], { total: 1 });
      }
      return readingsResponse([reading({ id: 'new-reading', title: 'Stale submitted article' })], { total: 1 });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Existing article')).toBeInTheDocument();
    await user.type(screen.getByLabelText('URL'), 'https://example.com/post');
    await user.click(screen.getByRole('button', { name: 'Add reading' }));
    await user.click(screen.getByRole('button', { name: 'Clear token' }));

    expect(screen.queryByText('Existing article')).not.toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();

    await act(async () => {
      submitted.resolve(jsonResponse({ id: 'new-reading', status: 'pending' }, 201));
      await submitted.promise;
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(screen.queryByText('Stale submitted article')).not.toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();
  });

  it('prevents duplicate load-more requests while a page is loading', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const nextPage = deferred<Response>();
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=cursor-1')) {
        return nextPage.promise;
      }
      return readingsResponse([reading({ title: 'First article' })], { total: 2, nextCursor: 'cursor-1' });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    const button = screen.getByRole('button', { name: 'Load more' });
    await user.click(button);
    expect(button).toBeDisabled();
    expect(readingCalls(fetchImpl)).toHaveLength(2);

    await act(async () => {
      nextPage.resolve(
        readingsResponse([reading({ id: 'second', title: 'Second article' })], {
          total: 3,
          nextCursor: 'cursor-2',
        }),
      );
      await nextPage.promise;
    });
  });

  it('does not strand the load-more button when polling is due while the next page is in flight', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const nextPage = deferred<Response>();
    let firstPageLoads = 0;
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=cursor-1')) {
        return nextPage.promise;
      }
      firstPageLoads += 1;
      return readingsResponse(
        [reading({ id: 'pending', status: 'pending', title: `Pending article ${firstPageLoads}` })],
        { total: 2, nextCursor: 'cursor-1' },
      );
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    const button = screen.getByRole('button', { name: 'Load more' });
    act(() => {
      fireEvent.click(button);
    });
    expect(button).toBeDisabled();

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });

    await act(async () => {
      nextPage.resolve(
        readingsResponse([reading({ id: 'second', title: 'Second article' })], {
          total: 3,
          nextCursor: 'cursor-2',
        }),
      );
      await nextPage.promise;
    });

    expect(screen.getByRole('button', { name: 'Load more' })).toBeEnabled();
  });

  it('keeps current readings and cursor usable when loading more fails', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=cursor-1')) {
        return jsonResponse({ error: { code: 'upstream_error', message: 'try again later' } }, 500);
      }
      return readingsResponse([reading({ title: 'First article' })], { total: 2, nextCursor: 'cursor-1' });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('First article')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText('try again later')).toBeInTheDocument();
    expect(screen.getByText('First article')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Load more' })).toBeEnabled();
  });

  it('renders every lifecycle status distinctly and surfaces failed reading errors', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = fetchRoutes(() =>
      readingsResponse([
        reading({ id: 'pending', status: 'pending', title: 'Pending article' }),
        reading({ id: 'running', status: 'running', title: 'Running article' }),
        reading({ id: 'ready', status: 'ready', title: 'Ready article' }),
        reading({ id: 'failed', status: 'failed', title: 'Failed article', error: 'extraction failed' }),
      ]),
    );

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('Pending article')).toBeInTheDocument();
    expect(screen.getByLabelText('Status: Pending')).toHaveClass('reading-status-pending');
    expect(screen.getByLabelText('Status: Running')).toHaveClass('reading-status-running');
    expect(screen.getByLabelText('Status: Ready')).toHaveClass('reading-status-ready');
    expect(screen.getByLabelText('Status: Failed')).toHaveClass('reading-status-failed');
    expect(screen.getByText('extraction failed')).toBeInTheDocument();
  });

  it('polls the visible reading list while a reading is pending', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    let listLoads = 0;
    const fetchImpl = fetchRoutes(() => {
      listLoads += 1;
      if (listLoads === 1) {
        return readingsResponse([reading({ id: 'pending', status: 'pending', title: 'Pending article' })]);
      }
      return readingsResponse([reading({ id: 'pending', status: 'ready', title: 'Ready article' })]);
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });

    expect(screen.getByText('Pending article')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(1);

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });

    expect(screen.getByText('Ready article')).toBeInTheDocument();
    expect(screen.queryByText('Pending article')).not.toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(2);
  });

  it('polls loaded pages by following fresh cursors so shifted readings stay visible', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    let firstPageLoads = 0;
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=old-page-2')) {
        return readingsResponse([reading({ id: 'third', title: 'Third article' })], {
          total: 3,
        });
      }
      if (url.endsWith('?cursor=fresh-page-2')) {
        return readingsResponse(
          [
            reading({ id: 'second', title: 'Second article' }),
            reading({ id: 'third', title: 'Third article' }),
          ],
          { total: 4 },
        );
      }
      firstPageLoads += 1;
      if (firstPageLoads === 1) {
        return readingsResponse(
          [
            reading({ id: 'pending', status: 'pending', title: 'Pending article' }),
            reading({ id: 'second', title: 'Second article' }),
          ],
          { total: 3, nextCursor: 'old-page-2' },
        );
      }
      return readingsResponse(
        [
          reading({ id: 'newest', title: 'Newest article' }),
          reading({ id: 'pending', status: 'running', title: 'Running article' }),
        ],
        { total: 4, nextCursor: 'fresh-page-2' },
      );
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
      await Promise.resolve();
    });
    expect(screen.getByText('Third article')).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });

    expect(screen.getByText('Newest article')).toBeInTheDocument();
    expect(screen.getByText('Running article')).toBeInTheDocument();
    expect(screen.getByText('Second article')).toBeInTheDocument();
    expect(screen.getByText('Third article')).toBeInTheDocument();
    expect(readingCalls(fetchImpl).map(([url]) => url)).toEqual([
      'https://api.example.com/api/readings',
      'https://api.example.com/api/readings?cursor=old-page-2',
      'https://api.example.com/api/readings',
      'https://api.example.com/api/readings?cursor=fresh-page-2',
    ]);
  });

  it('continues polling running readings and stops after terminal readings are visible', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    let listLoads = 0;
    const fetchImpl = fetchRoutes(() => {
      listLoads += 1;
      if (listLoads === 1) {
        return readingsResponse([reading({ id: 'processing', status: 'pending', title: 'Pending article' })]);
      }
      if (listLoads === 2) {
        return readingsResponse([reading({ id: 'processing', status: 'running', title: 'Running article' })]);
      }
      return readingsResponse([reading({ id: 'processing', status: 'ready', title: 'Ready article' })]);
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(screen.getByText('Running article')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(2);

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(screen.getByText('Ready article')).toBeInTheDocument();
    expect(readingCalls(fetchImpl)).toHaveLength(3);

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl)).toHaveLength(3);
  });

  it('does not restart a paginated poll while a later page is still in flight', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const slowPoll = deferred<Response>();
    let firstPageLoads = 0;
    let secondPageLoads = 0;
    const fetchImpl = fetchRoutes((url) => {
      if (url.endsWith('?cursor=cursor-1')) {
        secondPageLoads += 1;
        if (secondPageLoads === 1) {
          return readingsResponse([reading({ id: 'second', title: 'Second article' })], { total: 2 });
        }
        return slowPoll.promise;
      }

      firstPageLoads += 1;
      if (firstPageLoads === 1) {
        return readingsResponse([reading({ id: 'processing', status: 'pending', title: 'Pending article' })], {
          nextCursor: 'cursor-1',
          total: 2,
        });
      }
      return readingsResponse([reading({ id: 'processing', status: 'running', title: 'Running article' })], {
        nextCursor: 'cursor-1',
        total: 2,
      });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
      await Promise.resolve();
    });
    expect(screen.getByText('Second article')).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl)).toHaveLength(4);

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl)).toHaveLength(4);

    await act(async () => {
      slowPoll.resolve(readingsResponse([reading({ id: 'second', title: 'Fresh second article' })], { total: 2 }));
      await slowPoll.promise;
    });
    expect(screen.getByText('Running article')).toBeInTheDocument();
    expect(screen.getByText('Fresh second article')).toBeInTheDocument();
  });

  it('ignores in-flight polling responses after the token is cleared', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const pollingResponse = deferred<Response>();
    let listLoads = 0;
    const fetchImpl = fetchRoutes(() => {
      listLoads += 1;
      if (listLoads === 1) {
        return readingsResponse([reading({ id: 'processing', status: 'pending', title: 'Pending article' })]);
      }
      return pollingResponse.promise;
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl)).toHaveLength(2);

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Clear token' }));
    });
    expect(screen.queryByText('Pending article')).not.toBeInTheDocument();
    expect(screen.getByText('Save a bearer token to load your reading list.')).toBeInTheDocument();

    await act(async () => {
      pollingResponse.resolve(readingsResponse([reading({ id: 'processing', status: 'ready', title: 'Stale ready article' })]));
      await pollingResponse.promise;
    });

    expect(screen.queryByText('Stale ready article')).not.toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl)).toHaveLength(2);
  });

  it('does not satisfy a post-submit refresh from a pre-submit in-flight polling response', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const stalePoll = deferred<Response>();
    let firstPageLoads = 0;
    let submitted = false;
    const fetchImpl = fetchRoutes((_url, init) => {
      if (init?.method === 'POST') {
        submitted = true;
        return jsonResponse({ id: 'new-reading', status: 'pending' }, 201);
      }
      firstPageLoads += 1;
      if (firstPageLoads === 1) {
        return readingsResponse([reading({ id: 'pending', status: 'pending', title: 'Pending article' })], {
          total: 1,
        });
      }
      if (!submitted) {
        return stalePoll.promise;
      }
      return readingsResponse(
        [
          reading({ id: 'new-reading', status: 'pending', title: 'Freshly submitted article' }),
          reading({ id: 'pending', status: 'running', title: 'Running article' }),
        ],
        { total: 2 },
      );
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });
    expect(readingCalls(fetchImpl)).toHaveLength(2);

    act(() => {
      fireEvent.change(screen.getByLabelText('URL'), { target: { value: 'https://example.com/post' } });
      fireEvent.click(screen.getByRole('button', { name: 'Add reading' }));
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(readingCalls(fetchImpl).map(([, init]) => init?.method ?? 'GET')).toEqual(['GET', 'GET', 'POST', 'GET']);
    expect(screen.getByText('Freshly submitted article')).toBeInTheDocument();

    await act(async () => {
      stalePoll.resolve(readingsResponse([reading({ id: 'pending', status: 'pending', title: 'Stale pending article' })]));
      await stalePoll.promise;
    });

    expect(screen.getByText('Freshly submitted article')).toBeInTheDocument();
    expect(screen.queryByText('Stale pending article')).not.toBeInTheDocument();
  });

  it('keeps a delayed post-submit refresh active so polling cannot replace it with a stale response', async () => {
    vi.useFakeTimers();
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const stalePoll = deferred<Response>();
    const submitRefresh = deferred<Response>();
    let firstPageLoads = 0;
    let submitted = false;
    let submitRefreshResolved = false;
    const fetchImpl = fetchRoutes((_url, init) => {
      if (init?.method === 'POST') {
        submitted = true;
        return jsonResponse({ id: 'new-reading', status: 'pending' }, 201);
      }
      firstPageLoads += 1;
      if (firstPageLoads === 1) {
        return readingsResponse([reading({ id: 'pending', status: 'pending', title: 'Pending article' })], {
          total: 1,
        });
      }
      if (!submitted) {
        return stalePoll.promise;
      }
      if (submitRefreshResolved) {
        return readingsResponse(
          [reading({ id: 'new-reading', status: 'running', title: 'Freshly submitted article' })],
          { total: 2 },
        );
      }
      return submitRefresh.promise;
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText('Pending article')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });
    expect(readingCalls(fetchImpl)).toHaveLength(2);

    act(() => {
      fireEvent.change(screen.getByLabelText('URL'), { target: { value: 'https://example.com/post' } });
      fireEvent.click(screen.getByRole('button', { name: 'Add reading' }));
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl).map(([, init]) => init?.method ?? 'GET')).toEqual(['GET', 'GET', 'POST', 'GET']);

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });
    expect(readingCalls(fetchImpl).map(([, init]) => init?.method ?? 'GET')).toEqual(['GET', 'GET', 'POST', 'GET']);

    await act(async () => {
      submitRefreshResolved = true;
      submitRefresh.resolve(
        readingsResponse([reading({ id: 'new-reading', status: 'pending', title: 'Freshly submitted article' })], {
          total: 2,
        }),
      );
      await submitRefresh.promise;
    });
    expect(screen.getByText('Freshly submitted article')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(PROCESSING_POLL_INTERVAL_MS);
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(readingCalls(fetchImpl).map(([, init]) => init?.method ?? 'GET')).toEqual([
      'GET',
      'GET',
      'POST',
      'GET',
      'GET',
    ]);

    await act(async () => {
      stalePoll.resolve(readingsResponse([reading({ id: 'pending', status: 'pending', title: 'Stale pending article' })]));
      await stalePoll.promise;
    });
    expect(screen.queryByText('Stale pending article')).not.toBeInTheDocument();
  });

  it('keeps load more disabled while a post-submit refresh is in flight', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const submitRefresh = deferred<Response>();
    let submitted = false;
    const fetchImpl = fetchRoutes((url, init) => {
      if (init?.method === 'POST') {
        submitted = true;
        return jsonResponse({ id: 'new-reading', status: 'pending' }, 201);
      }
      if (url.endsWith('?cursor=old-page-2')) {
        return readingsResponse([reading({ id: 'second', title: 'Second article' })], {
          total: 2,
          nextCursor: 'old-page-3',
        });
      }
      if (url.endsWith('?cursor=fresh-page-2')) {
        return readingsResponse([reading({ id: 'first', title: 'First article' })], {
          total: 3,
          nextCursor: 'fresh-page-3',
        });
      }
      if (submitted) {
        return submitRefresh.promise;
      }
      return readingsResponse([reading({ id: 'first', title: 'First article' })], {
        total: 2,
        nextCursor: 'old-page-2',
      });
    });

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('First article')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('Second article')).toBeInTheDocument();

    await user.type(screen.getByLabelText('URL'), 'https://example.com/post');
    await user.click(screen.getByRole('button', { name: 'Add reading' }));
    expect(screen.getByRole('button', { name: 'Load more' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(readingCalls(fetchImpl).map(([url]) => url)).toEqual([
      'https://api.example.com/api/readings',
      'https://api.example.com/api/readings?cursor=old-page-2',
      'https://api.example.com/api/readings',
      'https://api.example.com/api/readings',
    ]);

    await act(async () => {
      submitRefresh.resolve(
        readingsResponse([reading({ id: 'new-reading', status: 'pending', title: 'Freshly submitted article' })], {
          total: 3,
          nextCursor: 'fresh-page-2',
        }),
      );
      await submitRefresh.promise;
    });
    expect(screen.getByText('Freshly submitted article')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Load more' })).toBeEnabled();
  });
});
