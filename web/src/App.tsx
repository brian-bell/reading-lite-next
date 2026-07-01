import { useCallback, useEffect, useRef, useState } from 'react';

import './App.css';
import { APIError, createAPIClient, resolveAPIBaseURL, type HealthDocument, type ReadingListItem, type ReadingStatus } from './api';
import { clearToken, loadToken, saveToken } from './tokenStorage';

type FetchImpl = (input: string, init?: RequestInit) => Promise<Response>;

type AppEnv = {
  VITE_READER_API_BASE_URL?: string;
};

type AppProps = {
  env?: AppEnv;
  fetchImpl?: FetchImpl;
};

const defaultFetch: FetchImpl = (input, init) => globalThis.fetch(input, init);
const readingsAuthMessage = 'Save a bearer token to load your reading list.';

export default function App({ env, fetchImpl = defaultFetch }: AppProps) {
  const runtimeEnv = env ?? (import.meta.env as AppEnv);
  const [token, setToken] = useState(() => loadToken());
  const [health, setHealth] = useState<HealthDocument | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [readings, setReadings] = useState<ReadingListItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [total, setTotal] = useState<number | null>(null);
  const [readingsLoading, setReadingsLoading] = useState(false);
  const [readingsLoadingMore, setReadingsLoadingMore] = useState(false);
  const [readingsError, setReadingsError] = useState('');
  const readingsRequestID = useRef(0);

  const refreshHealth = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const client = createAPIClient({
        baseURL: resolveAPIBaseURL(runtimeEnv),
        fetchImpl,
      });
      setHealth(await client.health());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [runtimeEnv, fetchImpl]);

  useEffect(() => {
    void refreshHealth();
  }, [refreshHealth]);

  const resetReadingsForMissingToken = useCallback(() => {
    readingsRequestID.current += 1;
    setReadings([]);
    setNextCursor(undefined);
    setTotal(null);
    setReadingsLoading(false);
    setReadingsLoadingMore(false);
    setReadingsError(readingsAuthMessage);
  }, []);

  const loadReadings = useCallback(
    async ({ cursor, tokenOverride }: { cursor?: string; tokenOverride?: string } = {}) => {
      const normalizedToken = (tokenOverride ?? loadToken()).trim();
      if (normalizedToken === '') {
        resetReadingsForMissingToken();
        return;
      }

      const requestID = readingsRequestID.current + 1;
      readingsRequestID.current = requestID;
      const firstPage = cursor === undefined;

      setReadingsError('');
      if (firstPage) {
        setReadings([]);
        setNextCursor(undefined);
        setTotal(null);
        setReadingsLoading(true);
      } else {
        setReadingsLoadingMore(true);
      }

      try {
        const client = createAPIClient({
          baseURL: resolveAPIBaseURL(runtimeEnv),
          fetchImpl,
        });
        const document = await client.listReadings({ token: normalizedToken, cursor });
        if (readingsRequestID.current !== requestID) {
          return;
        }
        setReadings((current) => (firstPage ? document.readings : [...current, ...document.readings]));
        setNextCursor(document.next_cursor);
        setTotal(document.total);
      } catch (err) {
        if (readingsRequestID.current !== requestID) {
          return;
        }
        setReadingsError(readingsErrorMessage(err));
        if (firstPage) {
          setReadings([]);
          setNextCursor(undefined);
          setTotal(null);
        }
      } finally {
        if (readingsRequestID.current === requestID) {
          setReadingsLoading(false);
          setReadingsLoadingMore(false);
        }
      }
    },
    [fetchImpl, resetReadingsForMissingToken, runtimeEnv],
  );

  useEffect(() => {
    void loadReadings({ tokenOverride: loadToken() });
  }, [loadReadings]);

  const handleLoadMore = useCallback(() => {
    if (nextCursor === undefined || readingsLoading || readingsLoadingMore) {
      return;
    }
    void loadReadings({ cursor: nextCursor, tokenOverride: loadToken() });
  }, [loadReadings, nextCursor, readingsLoading, readingsLoadingMore]);

  return (
    <main className="app-shell">
      <section className="toolbar" aria-labelledby="token-heading">
        <div>
          <h1 id="token-heading">Reading Lite</h1>
          <label htmlFor="bearer-token">Bearer token</label>
        </div>
        <input
          id="bearer-token"
          type="password"
          value={token}
          autoComplete="off"
          onChange={(event) => setToken(event.target.value)}
        />
        <div className="button-row">
          <button
            type="button"
            onClick={() => {
              saveToken(token);
              const storedToken = loadToken();
              setToken(storedToken);
              void loadReadings({ tokenOverride: storedToken });
            }}
          >
            Save token
          </button>
          <button
            type="button"
            className="secondary"
            onClick={() => {
              clearToken();
              setToken('');
              resetReadingsForMissingToken();
            }}
          >
            Clear token
          </button>
        </div>
      </section>

      <section className="health-panel" aria-labelledby="health-heading">
        <div className="section-heading">
          <h2 id="health-heading">API health</h2>
          <button type="button" className="secondary" onClick={() => void refreshHealth()}>
            Refresh health
          </button>
        </div>

        {loading ? <p className="muted">Loading health...</p> : null}
        {error ? (
          <p role="alert" className="error-message">
            {error}
          </p>
        ) : null}
        {health ? <HealthSummary health={health} /> : null}
      </section>

      <section className="readings-panel" aria-labelledby="readings-heading">
        <div className="section-heading">
          <h2 id="readings-heading">Reading list</h2>
          {total !== null ? <p className="muted">Total readings: {total}</p> : null}
        </div>

        {readingsLoading ? <p className="muted">Loading readings...</p> : null}
        {readingsError ? (
          <p role="alert" className="error-message">
            {readingsError}
          </p>
        ) : null}
        {!readingsLoading && readings.length === 0 && total === 0 && readingsError === '' ? (
          <p className="muted">No readings yet.</p>
        ) : null}
        {readings.length > 0 ? <ReadingList readings={readings} /> : null}
        {nextCursor !== undefined ? (
          <button
            type="button"
            className="secondary load-more"
            disabled={readingsLoading || readingsLoadingMore}
            onClick={handleLoadMore}
          >
            Load more
          </button>
        ) : null}
      </section>
    </main>
  );
}

function HealthSummary({ health }: { health: HealthDocument }) {
  return (
    <div className="health-summary">
      <p className={`status status-${health.status}`}>API status: {health.status}</p>
      <p>Version: {health.build.version}</p>
      <p>Commit: {health.build.commit}</p>
      <p>Date: {health.build.date}</p>
      <ul>
        {Object.entries(health.checks).map(([name, check]) => (
          <li key={name}>
            {name}: {check.status}
            {check.error ? ` (${check.error})` : ''}
          </li>
        ))}
      </ul>
    </div>
  );
}

function ReadingList({ readings }: { readings: ReadingListItem[] }) {
  return (
    <ul className="reading-list" aria-label="Readings">
      {readings.map((reading) => (
        <li key={reading.id} className="reading-item">
          <div className="reading-item-heading">
            <h3>{reading.title || reading.url}</h3>
            <StatusBadge status={reading.status} />
          </div>
          <p className="reading-meta">{reading.site || reading.url}</p>
          {reading.summary ? <p className="reading-summary">{reading.summary}</p> : null}
          {reading.tags && reading.tags.length > 0 ? (
            <ul className="tag-list" aria-label={`${reading.title || reading.url} tags`}>
              {reading.tags.map((tag) => (
                <li key={tag}>{tag}</li>
              ))}
            </ul>
          ) : null}
          {reading.status === 'failed' && reading.error ? <p className="reading-failure">{reading.error}</p> : null}
        </li>
      ))}
    </ul>
  );
}

function StatusBadge({ status }: { status: ReadingStatus }) {
  const label = statusLabel(status);
  return (
    <span className={`reading-status reading-status-${status}`} aria-label={`Status: ${label}`}>
      {label}
    </span>
  );
}

function statusLabel(status: ReadingStatus): string {
  switch (status) {
    case 'pending':
      return 'Pending';
    case 'running':
      return 'Running';
    case 'ready':
      return 'Ready';
    case 'failed':
      return 'Failed';
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof APIError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'Unable to load API health';
}

function readingsErrorMessage(err: unknown): string {
  if (err instanceof APIError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'Unable to load reading list';
}
