import type { AccessKey, Provider, ProviderProtocol, RequestLog, RequestLogSummary, RuntimeLog } from './types';

export interface RequestLogFilters {
  startedAfter?: string;
  startedBefore?: string;
  exposedModel?: string;
  upstreamModel?: string;
  providerId?: string;
}

export interface RequestLogsResult {
  data: RequestLog[];
  total: number;
  summary?: RequestLogSummary;
}

const tokenKey = 'omni-api-admin-token';
let adminToken = sessionStorage.getItem(tokenKey) ?? '';

const hashToken = new URLSearchParams(location.hash.slice(1)).get('token');
if (hashToken) {
  adminToken = hashToken;
  sessionStorage.setItem(tokenKey, hashToken);
  history.replaceState(null, '', `${location.pathname}${location.search}`);
}

export function getAdminToken(): string {
  return adminToken;
}

export function setAdminToken(value: string): void {
  adminToken = value.trim();
  if (adminToken) sessionStorage.setItem(tokenKey, adminToken);
  else sessionStorage.removeItem(tokenKey);
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (adminToken) headers.set('authorization', `Bearer ${adminToken}`);
  if (init?.body) headers.set('content-type', 'application/json');
  const response = await fetch(path, { ...init, headers });
  if (response.status === 204) return undefined as T;
  const body = await response.json().catch(() => ({})) as { error?: string | { message?: string } };
  if (!response.ok) {
    const message = typeof body.error === 'string' ? body.error : body.error?.message;
    const error = new Error(message || `请求失败 (${response.status})`) as Error & { status?: number };
    error.status = response.status;
    throw error;
  }
  return body as T;
}

export const api = {
  runtimeLogs: (signal?: AbortSignal) => request<{ data: RuntimeLog[]; capacity: number }>('/api/v1/runtime-logs', { signal }),
  providers: () => request<Provider[]>('/api/v1/providers'),
  syncProviderModels: (value: { providerId?: string; protocol: ProviderProtocol; baseUrl: string; apiKey?: string }) => request<{ models: string[] }>('/api/v1/providers/models:sync', { method: 'POST', body: JSON.stringify(value) }),
  createProvider: (value: Provider) => request<Provider>('/api/v1/providers', { method: 'POST', body: JSON.stringify(value) }),
  updateProvider: (value: Provider) => request<Provider>(`/api/v1/providers/${encodeURIComponent(value.id)}`, { method: 'PUT', body: JSON.stringify(value) }),
  deleteProvider: (id: string) => request<void>(`/api/v1/providers/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  requestLogs: (limit = 50, offset = 0, filters: RequestLogFilters = {}) => {
    const query = new URLSearchParams({ limit: String(limit), offset: String(offset) });
    for (const [name, value] of Object.entries(filters)) {
      if (value) query.set(name, value);
    }
    return request<RequestLogsResult>(`/api/v1/request-logs?${query}`);
  },
  accessKeys: () => request<AccessKey[]>('/api/v1/access-keys'),
  accessKey: (id: string) => request<AccessKey>(`/api/v1/access-keys/${encodeURIComponent(id)}`),
  createAccessKey: (name: string, secret?: string) => request<AccessKey>('/api/v1/access-keys', { method: 'POST', body: JSON.stringify({ name, ...(secret !== undefined ? { secret } : {}) }) }),
  updateAccessKey: (value: Pick<AccessKey, 'id' | 'name' | 'enabled'>) => request<void>(`/api/v1/access-keys/${encodeURIComponent(value.id)}`, { method: 'PUT', body: JSON.stringify({ name: value.name, enabled: value.enabled }) }),
  deleteAccessKey: (id: string) => request<void>(`/api/v1/access-keys/${encodeURIComponent(id)}`, { method: 'DELETE' }),
};
