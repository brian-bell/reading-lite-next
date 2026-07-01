import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';

import './App.css';
import {
  APIError,
  createAPIClient,
  resolveAPIBaseURL,
  type HealthDocument,
  type ReadingListItem,
  type ReadingsListDocument,
  type ReadingStatus,
} from './api';
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
export const PROCESSING_POLL_INTERVAL_MS = 4000;
const inFlightReadingsByFetch = new WeakMap<FetchImpl, Map<string, Promise<ReadingsListDocument>>>();

export default function App({ env, fetchImpl = defaultFetch }: AppProps) {
  const runtimeEnv = env ?? (import.meta.env as AppEnv);
  const [token, setToken] = useState(() => loadToken());
  const [health, setHealth] = useState<HealthDocument | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [readings, setReadings] = useState<ReadingListItem[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loadedCursors, setLoadedCursors] = useState<string[]>([]);
  const [total, setTotal] = useState<number | null>(null);
  const [readingsLoading, setReadingsLoading] = useState(false);
  const [readingsLoadingMore, setReadingsLoadingMore] = useState(false);
  const [readingsError, setReadingsError] = useState('');
  const [submitURL, setSubmitURL] = useState('');
  const [submitLoading, setSubmitLoading] = useState(false);
  const [submitError, setSubmitError] = useState('');
  const [submitMessage, setSubmitMessage] = useState('');
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
    setLoadedCursors([]);
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
        const document = await listReadingsOnce({
          baseURL: resolveAPIBaseURL(runtimeEnv),
          fetchImpl,
          token: normalizedToken,
          cursor,
        });
        if (readingsRequestID.current !== requestID) {
          return;
        }
        setReadings((current) => (firstPage ? document.readings : appendUniqueReadings(current, document.readings)));
        setNextCursor(document.next_cursor);
        setLoadedCursors((current) => {
          if (firstPage) {
            return [];
          }
          if (cursor === undefined || current.includes(cursor)) {
            return current;
          }
          return [...current, cursor];
        });
        setTotal(document.total);
      } catch (err) {
        if (readingsRequestID.current !== requestID) {
          return;
        }
        setReadingsError(readingsErrorMessage(err));
        if (firstPage) {
          setReadings([]);
          setNextCursor(undefined);
          setLoadedCursors([]);
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

  const refreshVisibleReadings = useCallback(
    async ({ tokenOverride }: { tokenOverride?: string } = {}) => {
      const normalizedToken = (tokenOverride ?? loadToken()).trim();
      if (normalizedToken === '') {
        resetReadingsForMissingToken();
        return;
      }

      const requestID = readingsRequestID.current + 1;
      readingsRequestID.current = requestID;
      setReadingsError('');

      try {
        const baseURL = resolveAPIBaseURL(runtimeEnv);
        const documents = [
          await listReadingsOnce({
            baseURL,
            fetchImpl,
            token: normalizedToken,
          }),
        ];
        for (const cursor of loadedCursors) {
          if (readingsRequestID.current !== requestID) {
            return;
          }
          documents.push(
            await listReadingsOnce({
              baseURL,
              fetchImpl,
              token: normalizedToken,
              cursor,
            }),
          );
        }
        if (readingsRequestID.current !== requestID) {
          return;
        }
        const refreshed = mergeReadingsDocuments(documents);
        setReadings(refreshed.readings);
        setNextCursor(refreshed.next_cursor);
        setTotal(refreshed.total);
      } catch (err) {
        if (readingsRequestID.current !== requestID) {
          return;
        }
        setReadingsError(readingsErrorMessage(err));
        if (isUnauthorizedError(err)) {
          setReadings([]);
          setNextCursor(undefined);
          setLoadedCursors([]);
          setTotal(null);
        }
      }
    },
    [fetchImpl, loadedCursors, resetReadingsForMissingToken, runtimeEnv],
  );

  useEffect(() => {
    void loadReadings({ tokenOverride: loadToken() });
  }, [loadReadings]);

  const hasActiveVisibleReading = hasProcessingReadings(readings);

  useEffect(() => {
    const normalizedToken = loadToken().trim();
    if (!hasActiveVisibleReading || normalizedToken === '') {
      return;
    }

    const intervalID = window.setInterval(() => {
      void refreshVisibleReadings({ tokenOverride: normalizedToken });
    }, PROCESSING_POLL_INTERVAL_MS);
    return () => window.clearInterval(intervalID);
  }, [hasActiveVisibleReading, refreshVisibleReadings, token]);

  const handleLoadMore = useCallback(() => {
    if (nextCursor === undefined || readingsLoading || readingsLoadingMore) {
      return;
    }
    void loadReadings({ cursor: nextCursor, tokenOverride: loadToken() });
  }, [loadReadings, nextCursor, readingsLoading, readingsLoadingMore]);

  const handleSubmitURL = useCallback(
    async (event: FormEvent<HTMLFormElement>) => {
      event.preventDefault();
      const normalizedToken = loadToken().trim();
      const normalizedURL = submitURL.trim();
      if (normalizedToken === '' || normalizedURL === '' || submitLoading) {
        return;
      }

      setSubmitLoading(true);
      setSubmitError('');
      setSubmitMessage('');
      try {
        const client = createAPIClient({
          baseURL: resolveAPIBaseURL(runtimeEnv),
          fetchImpl,
        });
        const submitted = await client.submitURL({ token: normalizedToken, url: normalizedURL });
        if (loadToken().trim() !== normalizedToken) {
          return;
        }
        setSubmitURL('');
        setSubmitMessage(`Submitted URL. Status: ${submitted.status}.`);
        await loadReadings({ tokenOverride: normalizedToken });
      } catch (err) {
        if (loadToken().trim() === normalizedToken) {
          setSubmitError(readingsErrorMessage(err));
        }
      } finally {
        setSubmitLoading(false);
      }
    },
    [fetchImpl, loadReadings, runtimeEnv, submitLoading, submitURL],
  );

  const canSubmitURL = loadToken().trim() !== '' && submitURL.trim() !== '' && !submitLoading;

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
              setSubmitError('');
              setSubmitMessage('');
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

        <form className="submission-form" onSubmit={handleSubmitURL}>
          <label htmlFor="reading-url">URL</label>
          <div className="submission-row">
            <input
              id="reading-url"
              type="url"
              value={submitURL}
              onChange={(event) => setSubmitURL(event.target.value)}
            />
            <button type="submit" disabled={!canSubmitURL}>
              Add reading
            </button>
          </div>
          {submitError ? (
            <p role="alert" className="error-message">
              {submitError}
            </p>
          ) : null}
          {submitMessage ? <p className="muted">{submitMessage}</p> : null}
        </form>

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

function listReadingsOnce({
  baseURL,
  fetchImpl,
  token,
  cursor,
}: {
  baseURL: string;
  fetchImpl: FetchImpl;
  token: string;
  cursor?: string;
}): Promise<ReadingsListDocument> {
  const requestKey = JSON.stringify([baseURL, token, cursor ?? null]);
  let requests = inFlightReadingsByFetch.get(fetchImpl);
  if (requests === undefined) {
    requests = new Map();
    inFlightReadingsByFetch.set(fetchImpl, requests);
  }

  const existingRequest = requests.get(requestKey);
  if (existingRequest !== undefined) {
    return existingRequest;
  }

  const request = createAPIClient({ baseURL, fetchImpl })
    .listReadings({ token, cursor })
    .finally(() => requests.delete(requestKey));
  requests.set(requestKey, request);
  return request;
}

function appendUniqueReadings(current: ReadingListItem[], next: ReadingListItem[]): ReadingListItem[] {
  const byID = new Map(current.map((reading) => [reading.id, reading]));
  for (const reading of next) {
    byID.set(reading.id, reading);
  }
  return Array.from(byID.values());
}

function mergeReadingsDocuments(documents: ReadingsListDocument[]): ReadingsListDocument {
  const byID = new Map<string, ReadingListItem>();
  for (const document of documents) {
    for (const reading of document.readings) {
      byID.set(reading.id, reading);
    }
  }
  const lastDocument = documents.at(-1);
  return {
    readings: Array.from(byID.values()),
    total: documents[0]?.total ?? 0,
    next_cursor: lastDocument?.next_cursor,
  };
}

function hasProcessingReadings(readings: ReadingListItem[]): boolean {
  return readings.some((reading) => reading.status === 'pending' || reading.status === 'running');
}

function isUnauthorizedError(err: unknown): boolean {
  return err instanceof APIError && (err.status === 401 || err.code === 'unauthorized');
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
