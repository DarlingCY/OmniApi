export type ProviderProtocol = 'openai-chat' | 'openai-responses' | 'anthropic-messages';

export interface RuntimeLog {
  id: number;
  time: string;
  level: 'info' | 'warn' | 'error';
  requestId?: string;
  message: string;
  detail?: string;
}

export interface ProviderModel {
  id: string;
  upstreamModel: string;
  alias?: string;
}

export interface Provider {
  id: string;
  name: string;
  baseUrl: string;
  protocol: ProviderProtocol;
  apiKey?: string;
  models: ProviderModel[];
  enabled: boolean;
}

export interface PublicSettings {
  hasAdminToken: boolean;
}

export interface AccessKey {
  id: string;
  name: string;
  secret?: string;
  prefix: string;
  enabled: boolean;
  createdAt: string;
}

export interface RequestUsage {
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  reasoningTokens: number;
}

export interface RequestLog {
  id: string;
  startedAt: string;
  inbound: string;
  exposedModel: string;
  upstreamModel: string;
  providerId: string;
  providerName: string;
  clientAddress: string;
  stream: boolean;
  status: number;
  attempts: number;
  error?: string;
  usage: RequestUsage;
  firstTokenMs: number;
  durationMs: number;
}

export interface RequestLogSummary {
  totalRequests: number;
  totalTokens: number;
  successRate: number;
  averageDuration: number;
}

