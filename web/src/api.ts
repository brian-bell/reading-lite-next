export type HealthDocument = {
  status: string;
  build: {
    version: string;
    commit: string;
    date: string;
  };
  checks: Record<string, { status: string; error?: string }>;
};

type ViteEnv = {
  VITE_READER_API_BASE_URL?: string;
};

type FetchImpl = (input: string, init?: RequestInit) => Promise<Response>;

type APIClientOptions = {
  baseURL: string;
  fetchImpl: FetchImpl;
};

export type APIClient = {
  health(): Promise<HealthDocument>;
};

export class APIError extends Error {
  readonly code: string;
  readonly status?: number;

  constructor(code: string, message: string, status?: number) {
    super(message);
    this.name = 'APIError';
    this.code = code;
    this.status = status;
  }
}

export function resolveAPIBaseURL(env: ViteEnv): string {
  return (env.VITE_READER_API_BASE_URL ?? '').trim().replace(/\/+$/, '');
}

export function createAPIClient({ baseURL, fetchImpl }: APIClientOptions): APIClient {
  const normalizedBaseURL = baseURL.trim().replace(/\/+$/, '');

  return {
    async health(): Promise<HealthDocument> {
      if (normalizedBaseURL === '') {
        throw new APIError('missing_config', 'VITE_READER_API_BASE_URL is required');
      }
      const response = await fetchImpl(`${normalizedBaseURL}/api/healthz`, {
        headers: { Accept: 'application/json' },
      });
      let body: unknown;
      try {
        body = await response.json();
      } catch {
        if (!response.ok) {
          throw new APIError('http_error', `Request failed with status ${response.status}`, response.status);
        }
        throw new APIError('invalid_response', 'API response was not valid JSON', response.status);
      }
      if (isHealthDocument(body)) {
        return body;
      }
      if (!response.ok) {
        throw errorFromBody(response, body);
      }
      throw new APIError('invalid_response', 'API response was not a health document', response.status);
    },
  };
}

function errorFromBody(response: Response, body: unknown): APIError {
  if (isRecord(body) && isRecord(body.error)) {
    const { code, message } = body.error;
    if (typeof code === 'string' && typeof message === 'string') {
      return new APIError(code, message, response.status);
    }
  }
  return new APIError('http_error', `Request failed with status ${response.status}`, response.status);
}

function isHealthDocument(body: unknown): body is HealthDocument {
  return (
    isRecord(body) &&
    typeof body.status === 'string' &&
    isRecord(body.build) &&
    typeof body.build.version === 'string' &&
    typeof body.build.commit === 'string' &&
    typeof body.build.date === 'string' &&
    isHealthChecks(body.checks)
  );
}

function isHealthChecks(value: unknown): value is HealthDocument['checks'] {
  if (!isRecord(value)) {
    return false;
  }
  for (const check of Object.values(value)) {
    if (!isRecord(check) || typeof check.status !== 'string') {
      return false;
    }
    if ('error' in check && typeof check.error !== 'string') {
      return false;
    }
  }
  return true;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
