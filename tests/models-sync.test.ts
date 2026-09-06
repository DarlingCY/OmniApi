import assert from 'node:assert/strict';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { discoverModels } from '../packages/adapters/src/provider-client.js';
import { createServer } from '../packages/server/src/server.js';
import { ConfigStore, PlaintextCodec } from '../packages/storage/src/config-store.js';

test('OpenAI model discovery preserves v1, uses Bearer auth, and normalizes models', async () => {
  const originalFetch = globalThis.fetch;
  let capturedUrl = '';
  let capturedHeaders: HeadersInit | undefined;
  globalThis.fetch = async (input, init) => {
    capturedUrl = String(input);
    capturedHeaders = init?.headers;
    return new Response(JSON.stringify({ data: [{ id: 'zeta' }, { id: 'alpha' }, { id: 'zeta' }, { id: '' }, {}] }), { status: 200, headers: { 'content-type': 'application/json' } });
  };
  try {
    assert.deepEqual(await discoverModels('openai-chat', 'https://api.example.test/v1', 'secret'), ['alpha', 'zeta']);
    assert.equal(capturedUrl, 'https://api.example.test/v1/models');
    assert.equal((capturedHeaders as Record<string, string>).authorization, 'Bearer secret');
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('Anthropic model discovery uses Anthropic headers', async () => {
  const originalFetch = globalThis.fetch;
  let capturedHeaders: HeadersInit | undefined;
  globalThis.fetch = async (_input, init) => {
    capturedHeaders = init?.headers;
    return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'content-type': 'application/json' } });
  };
  try {
    await discoverModels('anthropic-messages', 'https://api.anthropic.test/v1', 'secret');
    const headers = capturedHeaders as Record<string, string>;
    assert.equal(headers['x-api-key'], 'secret');
    assert.equal(headers['anthropic-version'], '2023-06-01');
    assert.equal(headers.authorization, undefined);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('model synchronization reuses a saved key and relays upstream errors', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'omni-api-'));
  const store = new ConfigStore(join(directory, 'config.json'), new PlaintextCodec());
  await store.replace({
    settings: { adminToken: 'admin-token' },
    providers: [{ id: 'saved', name: 'Saved', baseUrl: 'https://saved.test/v1', protocol: 'openai-responses', apiKey: 'stored-key', enabled: true, models: [] }],
    logicalModels: [],
  });
  const server = await createServer({ store });
  const originalFetch = globalThis.fetch;
  let capturedHeaders: HeadersInit | undefined;
  globalThis.fetch = async (_input, init) => {
    capturedHeaders = init?.headers;
    return new Response(JSON.stringify({ error: { message: 'Model listing is disabled' } }), { status: 403, headers: { 'content-type': 'application/json' } });
  };
  try {
    const reply = await server.inject({ method: 'POST', url: '/api/v1/providers/models:sync', headers: { authorization: 'Bearer admin-token' }, payload: { providerId: 'saved', protocol: 'openai-responses', baseUrl: 'https://saved.test/v1', apiKey: '' } });
    assert.equal((capturedHeaders as Record<string, string>).authorization, 'Bearer stored-key');
    assert.equal(reply.statusCode, 403);
    assert.deepEqual(reply.json(), { error: { message: 'Model listing is disabled' } });
  } finally {
    globalThis.fetch = originalFetch;
    await server.close();
  }
});