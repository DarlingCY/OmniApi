import type { InternalMessage, InternalRequest, ModelBinding, Provider, ProviderProtocol, UpstreamResult } from '../../core/src/types.js';

function endpoint(provider: Provider): string {
  if (provider.protocol === 'openai-chat') return 'chat/completions';
  if (provider.protocol === 'openai-responses') return 'responses';
  return 'messages';
}

function toOpenAiMessages(request: InternalRequest): InternalMessage[] {
  return request.system ? [{ role: 'system', content: request.system }, ...request.messages] : request.messages;
}

function payload(provider: Provider, binding: ModelBinding, request: InternalRequest): unknown {
  const model = provider.models.find((item) => item.id === binding.providerModelId)?.upstreamModel;
  if (!model) throw Object.assign(new Error('Provider model not found'), { status: 404, publicMessage: 'Provider model not found' });
  if (provider.protocol === 'openai-chat') return { model, messages: toOpenAiMessages(request), tools: request.tools, max_tokens: request.maxOutputTokens, temperature: request.temperature, top_p: request.topP, stream: request.stream };
  if (provider.protocol === 'openai-responses') return { model, input: toOpenAiMessages(request), tools: request.tools, max_output_tokens: request.maxOutputTokens, temperature: request.temperature, top_p: request.topP, stream: request.stream };
  return { model, system: request.system, messages: request.messages.filter((message) => message.role !== 'system'), tools: request.tools, max_tokens: request.maxOutputTokens ?? 4096, temperature: request.temperature, top_p: request.topP, stream: request.stream };
}

async function* sseLines(response: Response): AsyncIterable<string> {
  if (!response.body) return;
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let remainder = '';
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      const parts = (remainder + value).split(/\r?\n/);
      remainder = parts.pop() ?? '';
      for (const line of parts) if (line.startsWith('data:')) yield line.slice(5).trim();
    }
  } finally { reader.releaseLock(); }
}

function textDelta(protocol: Provider['protocol'], data: string): string | undefined {
  if (!data || data === '[DONE]') return undefined;
  const event = JSON.parse(data) as Record<string, unknown>;
  if (protocol === 'openai-chat') {
    const choices = event.choices as Array<Record<string, unknown>> | undefined;
    const delta = choices?.[0]?.delta as Record<string, unknown> | undefined;
    return typeof delta?.content === 'string' ? delta.content : undefined;
  }
  if (protocol === 'openai-responses') return event.type === 'response.output_text.delta' && typeof event.delta === 'string' ? event.delta : undefined;
  const delta = event.delta as Record<string, unknown> | undefined;
  return event.type === 'content_block_delta' && delta?.type === 'text_delta' && typeof delta.text === 'string' ? delta.text : undefined;
}

async function* textDeltas(protocol: Provider['protocol'], response: Response): AsyncIterable<string> {
  for await (const data of sseLines(response)) {
    const delta = textDelta(protocol, data);
    if (delta) yield delta;
  }
}

async function prefetched(source: AsyncIterable<string>): Promise<AsyncIterable<string>> {
  const iterator = source[Symbol.asyncIterator]();
  const first = await iterator.next();
  if (first.done) throw Object.assign(new Error('Upstream stream ended before the first event'), { status: 502, publicMessage: 'Upstream stream ended before the first event' });
  return (async function* () {
    yield first.value;
    for (;;) {
      const next = await iterator.next();
      if (next.done) return;
      yield next.value;
    }
  })();
}

function upstreamError(body: unknown, fallback: string): string {
  const record = body as Record<string, unknown>;
  const error = record?.error as Record<string, unknown> | string | undefined;
  if (typeof error === 'string') return error;
  if (typeof error?.message === 'string') return error.message;
  return fallback;
}

export async function discoverModels(protocol: ProviderProtocol, baseUrl: string, apiKey?: string): Promise<string[]> {
  const headers: Record<string, string> = {};
  if (apiKey && protocol === 'anthropic-messages') {
    headers['x-api-key'] = apiKey;
    headers['anthropic-version'] = '2023-06-01';
  } else if (apiKey) headers.authorization = `Bearer ${apiKey}`;

  let response: Response;
  try {
    response = await fetch(new URL('models', `${baseUrl.replace(/\/$/, '')}/`), { headers, signal: AbortSignal.timeout(60_000) });
  } catch {
    throw Object.assign(new Error('Unable to reach upstream models endpoint'), { status: 502, publicMessage: 'Unable to reach upstream models endpoint' });
  }
  const body = await response.json().catch(() => undefined);
  if (!response.ok) throw Object.assign(new Error(upstreamError(body, response.statusText || 'Upstream request failed')), { status: response.status || 502, publicMessage: upstreamError(body, response.statusText || 'Upstream request failed') });
  if (!body || typeof body !== 'object') throw Object.assign(new Error('Upstream models response could not be parsed'), { status: 502, publicMessage: 'Upstream models response could not be parsed' });
  const data = (body as Record<string, unknown>).data;
  if (!Array.isArray(data)) return [];
  const models = data.map((item) => {
    const id = (item as Record<string, unknown>).id;
    return typeof id === 'string' ? id.trim() : '';
  }).filter(Boolean);
  return [...new Set(models)].sort((left, right) => left.localeCompare(right));
}

export async function callProvider(provider: Provider, binding: ModelBinding, request: InternalRequest): Promise<UpstreamResult> {
  const headers: Record<string, string> = { 'content-type': 'application/json' };
  if (provider.apiKey && provider.protocol === 'anthropic-messages') {
    headers['x-api-key'] = provider.apiKey;
    headers['anthropic-version'] = '2023-06-01';
  } else if (provider.apiKey) headers.authorization = `Bearer ${provider.apiKey}`;
  const response = await fetch(new URL(endpoint(provider), `${provider.baseUrl.replace(/\/$/, '')}/`), {
    method: 'POST',
    headers,
    body: JSON.stringify(payload(provider, binding, request)),
    signal: AbortSignal.timeout(60_000),
  });
  if (request.stream && response.ok) return { status: response.status, body: null, stream: await prefetched(textDeltas(provider.protocol, response)) };
  const body = await response.json().catch(() => ({ error: { message: response.statusText } }));
  if (!response.ok) throw Object.assign(new Error(upstreamError(body, response.statusText)), { status: response.status, publicMessage: upstreamError(body, response.statusText) });
  return { status: response.status, body };
}
