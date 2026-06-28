// @vitest-environment jsdom

import '@testing-library/jest-dom/vitest';

import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import App from './App';
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

describe('App', () => {
  afterEach(() => {
    cleanup();
  });

  beforeEach(() => {
    localStorage.clear();
  });

  it('loads, saves, and clears the bearer token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(okHealth), { status: 200 }));

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

  it('loads health on mount and refreshes without sending Authorization', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const user = userEvent.setup();
    const fetchImpl = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(okHealth), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(degradedHealth), { status: 503 }));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com/' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API status: ok')).toBeInTheDocument();
    expect(screen.getByText('Version: dev')).toBeInTheDocument();
    expect(screen.getByText('postgres: ok')).toBeInTheDocument();
    expect(screen.getByText('r2: ok')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Refresh health' }));

    expect(await screen.findByText('API status: degraded')).toBeInTheDocument();
    expect(fetchImpl).toHaveBeenCalledTimes(2);
    for (const call of fetchImpl.mock.calls) {
      expect(call[0]).toBe('https://api.example.com/api/healthz');
      expect(call[1]).toEqual({ headers: { Accept: 'application/json' } });
    }
  });

  it('displays health errors without clearing the saved token', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'stored-token');
    const fetchImpl = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: { code: 'unavailable', message: 'upstream down' } }), {
          status: 503,
        }),
    );

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('upstream down')).toBeInTheDocument();
    expect(screen.getByLabelText('Bearer token')).toHaveValue('stored-token');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('stored-token');
  });

  it('renders malformed health documents as an error', async () => {
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify({}), { status: 200 }));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API response was not a health document')).toBeInTheDocument();
  });

  it('renders malformed health check values as an error', async () => {
    const malformedHealth = {
      status: 'ok',
      build: { version: 'dev', commit: 'abc123', date: '2026-06-28' },
      checks: { postgres: null },
    };
    const fetchImpl = vi.fn(async () => new Response(JSON.stringify(malformedHealth), { status: 200 }));

    render(<App env={{ VITE_READER_API_BASE_URL: 'https://api.example.com' }} fetchImpl={fetchImpl} />);

    expect(await screen.findByText('API response was not a health document')).toBeInTheDocument();
  });
});
