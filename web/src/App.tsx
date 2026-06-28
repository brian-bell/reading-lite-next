import { useCallback, useEffect, useState } from 'react';

import './App.css';
import { APIError, createAPIClient, HealthDocument, resolveAPIBaseURL } from './api';
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

export default function App({ env, fetchImpl = defaultFetch }: AppProps) {
  const runtimeEnv = env ?? (import.meta.env as AppEnv);
  const [token, setToken] = useState(() => loadToken());
  const [health, setHealth] = useState<HealthDocument | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

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
              setToken(loadToken());
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

function errorMessage(err: unknown): string {
  if (err instanceof APIError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'Unable to load API health';
}
