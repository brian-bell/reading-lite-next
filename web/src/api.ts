export type HealthDocument = {
  status: string;
  build: {
    version: string;
    commit: string;
    date: string;
  };
  checks: Record<string, { status: string; error?: string }>;
};

export type ReadingStatus = 'pending' | 'running' | 'ready' | 'failed';

export type ReadingListItem = {
  id: string;
  url: string;
  status: ReadingStatus;
  title?: string;
  site?: string;
  summary?: string;
  error?: string;
  tags?: string[];
  word_count?: number;
  created_at: string;
  updated_at: string;
};

export type ReadingsListDocument = {
  readings: ReadingListItem[];
  total: number;
  next_cursor?: string;
};

type ReadingListItemWire = Omit<ReadingListItem, 'tags'> & {
  tags?: string[] | null;
};

type ReadingsListDocumentWire = Omit<ReadingsListDocument, 'readings'> & {
  readings: ReadingListItemWire[];
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
  listReadings(options: { token: string; cursor?: string }): Promise<ReadingsListDocument>;
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
      const body = await responseBody(response);
      if (isHealthDocument(body)) {
        return body;
      }
      if (!response.ok) {
        throw errorFromBody(response, body);
      }
      throw new APIError('invalid_response', 'API response was not a health document', response.status);
    },

    async listReadings({ token, cursor }: { token: string; cursor?: string }): Promise<ReadingsListDocument> {
      if (normalizedBaseURL === '') {
        throw new APIError('missing_config', 'VITE_READER_API_BASE_URL is required');
      }
      const query = new URLSearchParams();
      if (cursor !== undefined) {
        query.set('cursor', cursor);
      }
      const url = `${normalizedBaseURL}/api/readings${query.size > 0 ? `?${query.toString()}` : ''}`;
      const response = await fetchImpl(url, {
        headers: { Accept: 'application/json', Authorization: `Bearer ${token}` },
      });
      const body = await responseBody(response);
      if (!response.ok) {
        throw errorFromBody(response, body);
      }
      if (isReadingsListDocument(body)) {
        return normalizeReadingsListDocument(body);
      }
      throw new APIError('invalid_response', 'API response was not a readings list document', response.status);
    },
  };
}

async function responseBody(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    if (!response.ok) {
      throw new APIError('http_error', `Request failed with status ${response.status}`, response.status);
    }
    throw new APIError('invalid_response', 'API response was not valid JSON', response.status);
  }
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

function isReadingsListDocument(body: unknown): body is ReadingsListDocumentWire {
  return (
    isRecord(body) &&
    Array.isArray(body.readings) &&
    body.readings.every(isReadingListItem) &&
    typeof body.total === 'number' &&
    Number.isFinite(body.total) &&
    (!('next_cursor' in body) || typeof body.next_cursor === 'string')
  );
}

function isReadingListItem(value: unknown): value is ReadingListItemWire {
  if (!isRecord(value)) {
    return false;
  }
  return (
    typeof value.id === 'string' &&
    typeof value.url === 'string' &&
    isReadingStatus(value.status) &&
    typeof value.created_at === 'string' &&
    typeof value.updated_at === 'string' &&
    optionalString(value.title) &&
    optionalString(value.site) &&
    optionalString(value.summary) &&
    optionalString(value.error) &&
    optionalNumber(value.word_count) &&
    optionalNullableStringArray(value.tags)
  );
}

function isReadingStatus(value: unknown): value is ReadingStatus {
  return value === 'pending' || value === 'running' || value === 'ready' || value === 'failed';
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

function optionalString(value: unknown): boolean {
  return value === undefined || typeof value === 'string';
}

function optionalNumber(value: unknown): boolean {
  return value === undefined || (typeof value === 'number' && Number.isFinite(value));
}

function optionalNullableStringArray(value: unknown): boolean {
  return (
    value === undefined ||
    value === null ||
    (Array.isArray(value) && value.every((item) => typeof item === 'string'))
  );
}

function normalizeReadingsListDocument(body: ReadingsListDocumentWire): ReadingsListDocument {
  return {
    ...body,
    readings: body.readings.map((reading) => {
      const { tags, ...rest } = reading;
      if (tags === null) {
        return { ...rest, tags: [] };
      }
      if (tags === undefined) {
        return rest;
      }
      return { ...rest, tags };
    }),
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
