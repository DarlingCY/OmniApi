import assert from 'node:assert/strict';
import test from 'node:test';
import { AllProvidersFailedError, RoundRobinRouter } from '../packages/core/src/router.js';
import type { LogicalModel, Provider } from '../packages/core/src/types.js';

const providers: Provider[] = ['one', 'two', 'three'].map((id) => ({
  id,
  name: id,
  baseUrl: 'https://example.test',
  protocol: 'openai-chat',
  enabled: true,
  models: [{ id: `${id}-model`, upstreamModel: 'model' }],
}));

const logicalModel: LogicalModel = {
  id: 'chat', name: 'chat', bindings: providers.map((provider) => ({
    providerId: provider.id, providerModelId: `${provider.id}-model`, enabled: true,
  })),
};

test('round robin advances the starting provider', async () => {
  const router = new RoundRobinRouter();
  const calls: string[] = [];
  const call = async (provider: Provider) => {
    calls.push(provider.id);
    return { status: 200, body: {} };
  };
  await router.execute(logicalModel, providers, call);
  await router.execute(logicalModel, providers, call);
  assert.deepEqual(calls, ['one', 'two']);
});

test('switches past two failures to a third provider', async () => {
  const router = new RoundRobinRouter();
  const calls: string[] = [];
  const result = await router.execute(logicalModel, providers, async (provider) => {
    calls.push(provider.id);
    return { status: provider.id === 'three' ? 200 : 503, body: {} };
  });
  assert.equal(result.status, 200);
  assert.deepEqual(calls, ['one', 'two', 'three']);
});

test('returns the last public upstream error when every provider fails', async () => {
  const router = new RoundRobinRouter();
  await assert.rejects(
    router.execute(logicalModel, providers, async (provider) => ({
      status: provider.id === 'three' ? 429 : 503, body: {},
    })),
    (error: AllProvidersFailedError) => error.lastFailure.status === 429,
  );
});

test('does not switch on a non-switchable 400 response', async () => {
  const router = new RoundRobinRouter();
  const calls: string[] = [];
  await assert.rejects(
    router.execute(logicalModel, providers, async (provider) => {
      calls.push(provider.id);
      return { status: 400, body: {} };
    }),
    AllProvidersFailedError,
  );
  assert.deepEqual(calls, ['one']);
});