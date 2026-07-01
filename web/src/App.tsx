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
  const readingsLoadingRef = useRef({ firstPage: false, nextPage: false });

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
    readingsLoadingRef.current = { firstPage: false, nextPage: false };
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
        readingsLoadingRef.current = { firstPage: true, nextPage: false };
        setReadings([]);
        setNextCursor(undefined);
        setTotal(null);
        setReadingsLoading(true);
      } else {
        readingsLoadingRef.current = { ...readingsLoadingRef.current, nextPage: true };
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
          readingsLoadingRef.current = { firstPage: false, nextPage: false };
          setReadingsLoading(false);
          setReadingsLoadingMore(false);
        }
      }
    },
    [fetchImpl, resetReadingsForMissingToken, runtimeEnv],
  );

  const refreshVisibleReadings = useCallback(
    async ({
      tokenOverride,
      bypassCache = false,
      force = false,
    }: { tokenOverride?: string; bypassCache?: boolean; force?: boolean } = {}) => {
      const normalizedToken = (tokenOverride ?? loadToken()).trim();
      if (normalizedToken === '') {
        resetReadingsForMissingToken();
        return;
      }
      if (!force && (readingsLoadingRef.current.firstPage || readingsLoadingRef.current.nextPage)) {
        return;
      }

      const requestID = readingsRequestID.current + 1;
      readingsRequestID.current = requestID;
      readingsLoadingRef.current = { firstPage: true, nextPage: false };
      if (force) {
        setReadingsLoading(true);
        setReadingsLoadingMore(false);
      }
      setReadingsError('');

      try {
        const baseURL = resolveAPIBaseURL(runtimeEnv);
        const targetPageCount = loadedCursors.length + 1;
        const documents: ReadingsListDocument[] = [];
        const refreshedCursors: string[] = [];
        let cursor: string | undefined;
        for (let page = 0; page < targetPageCount; page += 1) {
          if (page > 0) {
            if (cursor === undefined) {
              break;
            }
            refreshedCursors.push(cursor);
          }
          if (readingsRequestID.current !== requestID) {
            return;
          }
          documents.push(
            await listReadingsOnce({
              baseURL,
              fetchImpl,
              token: normalizedToken,
              cursor,
              bypassCache,
            }),
          );
          cursor = documents.at(-1)?.next_cursor;
        }
        if (readingsRequestID.current !== requestID) {
          return;
        }
        const refreshed = mergeReadingsDocuments(documents);
        setReadings(refreshed.readings);
        setNextCursor(refreshed.next_cursor);
        setLoadedCursors((current) => (sameStrings(current, refreshedCursors) ? current : refreshedCursors));
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
      } finally {
        if (readingsRequestID.current === requestID) {
          readingsLoadingRef.current = { firstPage: false, nextPage: false };
          if (force) {
            setReadingsLoading(false);
            setReadingsLoadingMore(false);
          }
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
    if (
      nextCursor === undefined ||
      readingsLoading ||
      readingsLoadingMore ||
      readingsLoadingRef.current.firstPage ||
      readingsLoadingRef.current.nextPage
    ) {
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
        const baseURL = resolveAPIBaseURL(runtimeEnv);
        const client = createAPIClient({
          baseURL,
          fetchImpl,
        });
        const submitted = await client.submitURL({ token: normalizedToken, url: normalizedURL });
        if (loadToken().trim() !== normalizedToken) {
          return;
        }
        clearInFlightReadings(fetchImpl);
        setSubmitURL('');
        setSubmitMessage(`Submitted URL. Status: ${submitted.status}.`);
        await refreshVisibleReadings({ tokenOverride: normalizedToken, bypassCache: true, force: true });
      } catch (err) {
        if (loadToken().trim() === normalizedToken) {
          setSubmitError(readingsErrorMessage(err));
        }
      } finally {
        setSubmitLoading(false);
      }
    },
    [fetchImpl, refreshVisibleReadings, runtimeEnv, submitLoading, submitURL],
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
  bypassCache = false,
}: {
  baseURL: string;
  fetchImpl: FetchImpl;
  token: string;
  cursor?: string;
  bypassCache?: boolean;
}): Promise<ReadingsListDocument> {
  if (bypassCache) {
    return createAPIClient({ baseURL, fetchImpl }).listReadings({ token, cursor });
  }

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
    .finally(() => {
      if (requests.get(requestKey) === request) {
        requests.delete(requestKey);
      }
    });
  requests.set(requestKey, request);
  return request;
}

function clearInFlightReadings(fetchImpl: FetchImpl) {
  inFlightReadingsByFetch.get(fetchImpl)?.clear();
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

function sameStrings(left: string[], right: string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
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
