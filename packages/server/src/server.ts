import Fastify, { type FastifyInstance, type FastifyRequest } from 'fastify';
import fastifyStatic from '@fastify/static';
import { randomUUID } from 'node:crypto';
import { callProvider, discoverModels } from '../../adapters/src/provider-client.js';
import { AllProvidersFailedError, RoundRobinRouter } from '../../core/src/router.js';
import type { AppConfig, InternalMessage, InternalRequest, LogicalModel, Provider, ProviderProtocol, Settings, UpstreamResult } from '../../core/src/types.js';
import type { ConfigStore } from '../../storage/src/config-store.js';

interface ServerOptions { store: ConfigStore; staticDir?: string; }
type RecordBody = Record<string, unknown>;
interface ModelsSyncBody { providerId?: string; protocol: ProviderProtocol; baseUrl: string; apiKey?: string; }

function publicConfig(config: AppConfig): Omit<AppConfig, 'settings' | 'providers'> & { settings: Record<string, boolean>; providers: Array<Omit<Provider, 'apiKey'>> } {
  return {
    ...config,
    settings: { hasAdminToken: Boolean(config.settings.adminToken), hasProxyToken: Boolean(config.settings.proxyToken) },
    providers: config.providers.map(({ apiKey: _apiKey, ...provider }) => provider),
  };
}

function bearer(request: FastifyRequest): string | undefined {
  const value = request.headers.authorization;
  return value?.startsWith('Bearer ') ? value.slice(7) : undefined;
}

function requireToken(kind: 'admin' | 'proxy', store: ConfigStore) {
  return async (request: FastifyRequest, reply: { code(status: number): { send(body: unknown): void } }) => {
    const expected = kind === 'admin' ? store.get().settings.adminToken : store.get().settings.proxyToken;
    if (kind === 'admin' && !expected) return;
    if (!expected || bearer(request) !== expected) reply.code(401).send({ error: { message: 'Unauthorized' } });
  };
}

function messages(value: unknown): InternalMessage[] {
  if (!Array.isArray(value)) return [];
  return value.map((item) => {
    const record = item as RecordBody;
    return { role: record.role === 'assistant' || record.role === 'system' || record.role === 'tool' ? record.role : 'user', content: typeof record.content === 'string' ? record.content : JSON.stringify(record.content ?? ''), toolCallId: typeof record.tool_call_id === 'string' ? record.tool_call_id : undefined, name: typeof record.name === 'string' ? record.name : undefined };
  });
}

function normalize(kind: string, body: RecordBody): InternalRequest {
  const common = { tools: Array.isArray(body.tools) ? body.tools : undefined, temperature: typeof body.temperature === 'number' ? body.temperature : undefined, topP: typeof body.top_p === 'number' ? body.top_p : undefined, stream: body.stream === true };
  if (kind === 'responses') return { ...common, model: String(body.model), messages: messages(body.input), maxOutputTokens: typeof body.max_output_tokens === 'number' ? body.max_output_tokens : undefined };
  if (kind === 'messages') return { ...common, model: String(body.model), system: typeof body.system === 'string' ? body.system : undefined, messages: messages(body.messages), maxOutputTokens: typeof body.max_tokens === 'number' ? body.max_tokens : undefined };
  const source = messages(body.messages);
  const system = source.find((item) => item.role === 'system')?.content;
  return { ...common, model: String(body.model), system, messages: source.filter((item) => item.role !== 'system'), maxOutputTokens: typeof body.max_tokens === 'number' ? body.max_tokens : undefined };
}

function text(result: UpstreamResult): string {
  const body = result.body as RecordBody;
  const choices = body.choices as Array<RecordBody> | undefined;
  if (choices?.[0]?.message && typeof (choices[0].message as RecordBody).content === 'string') return (choices[0].message as RecordBody).content as string;
  if (typeof body.output_text === 'string') return body.output_text;
  const output = body.output as Array<RecordBody> | undefined;
  const outputContent = output?.[0]?.content as Array<RecordBody> | undefined;
  if (typeof outputContent?.[0]?.text === 'string') return outputContent[0].text as string;
  const content = body.content as Array<RecordBody> | undefined;
  if (typeof content?.[0]?.text === 'string') return content[0].text as string;
  return '';
}

function response(kind: string, model: string, result: UpstreamResult): unknown {
  const content = text(result);
  if (kind === 'responses') return { id: `resp_${randomUUID()}`, object: 'response', model, output_text: content, output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: content }] }] };
  if (kind === 'messages') return { id: `msg_${randomUUID()}`, type: 'message', role: 'assistant', model, content: [{ type: 'text', text: content }], stop_reason: 'end_turn' };
  return { id: `chatcmpl_${randomUUID()}`, object: 'chat.completion', model, choices: [{ index: 0, message: { role: 'assistant', content }, finish_reason: 'stop' }] };
}

async function stream(reply: { raw: import('node:http').ServerResponse }, kind: string, model: string, source: AsyncIterable<string>, requestId: string): Promise<void> {
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream; charset=utf-8', 'cache-control': 'no-cache', connection: 'keep-alive', 'x-request-id': requestId });
  if (kind === 'messages') reply.raw.write(`event: message_start\ndata: ${JSON.stringify({ type: 'message_start', message: { id: `msg_${requestId}`, type: 'message', role: 'assistant', model, content: [], stop_reason: null } })}\n\n`);
  for await (const delta of source) {
    if (kind === 'responses') reply.raw.write(`event: response.output_text.delta\ndata: ${JSON.stringify({ type: 'response.output_text.delta', delta })}\n\n`);
    else if (kind === 'messages') reply.raw.write(`event: content_block_delta\ndata: ${JSON.stringify({ type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: delta } })}\n\n`);
    else reply.raw.write(`data: ${JSON.stringify({ id: `chatcmpl_${requestId}`, object: 'chat.completion.chunk', model, choices: [{ index: 0, delta: { content: delta }, finish_reason: null }] })}\n\n`);
  }
  if (kind === 'responses') reply.raw.end(`event: response.completed\ndata: ${JSON.stringify({ type: 'response.completed', response: { id: `resp_${requestId}`, object: 'response', model } })}\n\n`);
  else if (kind === 'messages') reply.raw.end(`event: message_stop\ndata: ${JSON.stringify({ type: 'message_stop' })}\n\n`);
  else reply.raw.end('data: [DONE]\n\n');
}

export async function createServer(options: ServerOptions): Promise<FastifyInstance> {
  const server = Fastify({ logger: false });
  const router = new RoundRobinRouter();
  if (options.staticDir) await server.register(fastifyStatic, { root: options.staticDir, wildcard: false });

  server.get('/', async (_request, reply) => {
    if (options.staticDir) return reply.sendFile('index.html');
    return { status: 'ok', service: 'omni-api' };
  });

  const admin = { preHandler: requireToken('admin', options.store) };
  server.get('/api/v1/settings', admin, async () => publicConfig(options.store.get()).settings);
  server.put('/api/v1/settings', admin, async (request) => {
    const body = request.body as Settings;
    await options.store.updateSettings({
      ...(body.adminToken === undefined ? {} : { adminToken: body.adminToken }),
      ...(body.proxyToken === undefined ? {} : { proxyToken: body.proxyToken }),
    });
    return publicConfig(options.store.get()).settings;
  });
  server.get('/api/v1/providers', admin, async () => publicConfig(options.store.get()).providers);
  server.post('/api/v1/providers/models:sync', admin, async (request, reply) => {
    const body = request.body as Partial<ModelsSyncBody>;
    if (!body.protocol || !body.baseUrl) return reply.code(400).send({ error: { message: 'protocol and baseUrl are required' } });
    const savedProvider = body.providerId ? options.store.get().providers.find((provider) => provider.id === body.providerId) : undefined;
    const apiKey = body.apiKey?.trim() || savedProvider?.apiKey;
    try {
      return { models: await discoverModels(body.protocol, body.baseUrl, apiKey) };
    } catch (error) {
      const upstream = error as { status?: number; publicMessage?: string };
      return reply.code(upstream.status && upstream.status >= 400 && upstream.status <= 599 ? upstream.status : 502).send({ error: { message: upstream.publicMessage ?? 'Unable to synchronize upstream models' } });
    }
  });
  server.post('/api/v1/providers', admin, async (request, reply) => {
    const provider = request.body as Provider;
    if (!provider.id || !provider.baseUrl || !provider.protocol) return reply.code(400).send({ error: 'id, baseUrl and protocol are required' });
    const config = options.store.get();
    if (config.providers.some((item) => item.id === provider.id)) return reply.code(409).send({ error: 'Provider id already exists' });
    await options.store.replace({ ...config, providers: [...config.providers, provider] });
    const { apiKey: _apiKey, ...safe } = provider;
    return reply.code(201).send(safe);
  });
  server.put('/api/v1/providers/:id', admin, async (request, reply) => {
    const id = (request.params as { id: string }).id;
    const replacement = request.body as Provider;
    const config = options.store.get();
    const index = config.providers.findIndex((item) => item.id === id);
    if (index < 0) return reply.code(404).send({ error: 'Provider not found' });
    const providers = [...config.providers];
    providers[index] = {
      ...replacement,
      id,
      apiKey: replacement.apiKey === undefined ? providers[index].apiKey : replacement.apiKey,
    };
    await options.store.replace({ ...config, providers });
    const { apiKey: _apiKey, ...safe } = providers[index]; return safe;
  });
  server.delete('/api/v1/providers/:id', admin, async (request, reply) => {
    const id = (request.params as { id: string }).id; const config = options.store.get();
    if (!config.providers.some((item) => item.id === id)) return reply.code(404).send({ error: 'Provider not found' });
    await options.store.replace({ ...config, providers: config.providers.filter((item) => item.id !== id) }); return reply.code(204).send();
  });
  server.get('/api/v1/logical-models', admin, async () => options.store.get().logicalModels);
  server.post('/api/v1/logical-models', admin, async (request, reply) => {
    const model = request.body as LogicalModel; const config = options.store.get();
    if (!model.id || !model.name) return reply.code(400).send({ error: 'id and name are required' });
    if (config.logicalModels.some((item) => item.id === model.id)) return reply.code(409).send({ error: 'Logical model id already exists' });
    await options.store.replace({ ...config, logicalModels: [...config.logicalModels, model] }); return reply.code(201).send(model);
  });
  server.put('/api/v1/logical-models/:id', admin, async (request, reply) => {
    const id = (request.params as { id: string }).id; const model = request.body as LogicalModel; const config = options.store.get();
    const index = config.logicalModels.findIndex((item) => item.id === id);
    if (index < 0) return reply.code(404).send({ error: 'Logical model not found' });
    const logicalModels = [...config.logicalModels]; logicalModels[index] = { ...model, id };
    await options.store.replace({ ...config, logicalModels }); return logicalModels[index];
  });
  server.delete('/api/v1/logical-models/:id', admin, async (request, reply) => {
    const id = (request.params as { id: string }).id; const config = options.store.get();
    if (!config.logicalModels.some((item) => item.id === id)) return reply.code(404).send({ error: 'Logical model not found' });
    await options.store.replace({ ...config, logicalModels: config.logicalModels.filter((item) => item.id !== id) }); return reply.code(204).send();
  });

  for (const [path, kind] of [['/v1/chat/completions', 'chat'], ['/v1/responses', 'responses'], ['/v1/messages', 'messages']] as const) {
    server.post(path, { preHandler: requireToken('proxy', options.store) }, async (request, reply) => {
      const requestId = randomUUID();
      const internal = normalize(kind, request.body as RecordBody);
      const config = options.store.get();
      const logical = config.logicalModels.find((item) => item.name === internal.model || item.id === internal.model);
      if (!logical) return reply.code(404).header('x-request-id', requestId).send({ error: { message: 'Logical model not found', code: 'model_not_found' } });
      try {
        const upstream = await router.execute(logical, config.providers, (provider, binding) => callProvider(provider, binding, internal));
        if (internal.stream && upstream.stream) { await stream(reply, kind, internal.model, upstream.stream, requestId); return reply; }
        return reply.header('x-request-id', requestId).send(response(kind, internal.model, upstream));
      } catch (error) {
        const failed = error as AllProvidersFailedError;
        return reply.code(failed.lastFailure?.status ?? 502).header('x-request-id', requestId).send({ error: { message: failed.lastFailure?.message ?? 'Upstream request failed', code: 'all_providers_failed', requestId } });
      }
    });
  }
  return server;
}
