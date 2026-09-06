import assert from 'node:assert/strict';
import test from 'node:test';
import { callProvider } from '../packages/adapters/src/provider-client.js';
import type { InternalRequest, ModelBinding, Provider } from '../packages/core/src/types.js';

const provider: Provider = {
  id: 'anthropic',
  name: 'Anthropic',
  baseUrl: 'https://api.anthropic.com/v1',
  protocol: 'anthropic-messages',
  apiKey: 'secret',
  enabled: true,
  models: [{ id: 'claude', upstreamModel: 'claude-test' }],
};
const binding: ModelBinding = { providerId: provider.id, providerModelId: 'claude', enabled: true };
const request: InternalRequest = { model: 'omni', messages: [{ role: 'user', content: 'hello' }], stream: false };

test('Anthropic uses the configured v1 path and required headers', async () => {
  const originalFetch = globalThis.fetch;
  let capturedUrl = '';
  let capturedInit: RequestInit | undefined;
  globalThis.fetch = async (input, init) => {
    capturedUrl = String(input);
    capturedInit = init;
    return new Response(JSON.stringify({ content: [{ type: 'text', text: 'ok' }] }), { status: 200, headers: { 'content-type': 'application/json' } });
  };
  try {
    await callProvider(provider, binding, request);
    assert.equal(capturedUrl, 'https://api.anthropic.com/v1/messages');
    const headers = capturedInit?.headers as Record<string, string>;
    assert.equal(headers['x-api-key'], 'secret');
    assert.equal(headers['anthropic-version'], '2023-06-01');
    assert.equal(headers.authorization, undefined);
    const body = JSON.parse(String(capturedInit?.body)) as Record<string, unknown>;
    assert.equal(body.model, 'claude-test');
    assert.equal(body.max_tokens, 4096);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('an empty upstream stream fails before downstream streaming starts', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response('', { status: 200, headers: { 'content-type': 'text/event-stream' } });
  try {
    await assert.rejects(callProvider(provider, binding, { ...request, stream: true }), /before the first event/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});