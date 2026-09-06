export type ProviderProtocol = 'openai-chat' | 'openai-responses' | 'anthropic-messages';

export interface ProviderModel {
  id: string;
  upstreamModel: string;
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

export interface ModelBinding {
  providerId: string;
  providerModelId: string;
  enabled: boolean;
}

export interface LogicalModel {
  id: string;
  name: string;
  bindings: ModelBinding[];
}

export interface Settings {
  adminToken?: string;
  proxyToken?: string;
}

export interface AppConfig {
  settings: Settings;
  providers: Provider[];
  logicalModels: LogicalModel[];
}

export interface InternalMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: string;
  toolCallId?: string;
  name?: string;
}

export interface InternalRequest {
  model: string;
  system?: string;
  messages: InternalMessage[];
  tools?: unknown[];
  maxOutputTokens?: number;
  temperature?: number;
  topP?: number;
  stream: boolean;
}

export interface UpstreamResult {
  status: number;
  body: unknown;
  stream?: AsyncIterable<string>;
}

export interface UpstreamFailure extends Error {
  status?: number;
  publicMessage?: string;
}
