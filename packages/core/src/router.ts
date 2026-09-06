import type { LogicalModel, ModelBinding, Provider, UpstreamFailure, UpstreamResult } from './types.js';

export interface RouteFailure {
  status: number;
  message: string;
}

export class AllProvidersFailedError extends Error {
  constructor(public readonly lastFailure: RouteFailure) {
    super('all_providers_failed');
  }
}

export function isSwitchableStatus(status?: number): boolean {
  return status === undefined || status === 401 || status === 403 || status === 404 ||
    status === 408 || status === 429 || status >= 500;
}

export class RoundRobinRouter {
  private readonly nextIndex = new Map<string, number>();

  async execute(
    logicalModel: LogicalModel,
    providers: Provider[],
    call: (provider: Provider, binding: ModelBinding) => Promise<UpstreamResult>,
  ): Promise<UpstreamResult> {
    const bindings = logicalModel.bindings.filter((binding) => binding.enabled);
    if (bindings.length === 0) {
      throw new AllProvidersFailedError({ status: 503, message: 'No enabled provider bindings' });
    }

    const start = this.nextIndex.get(logicalModel.id) ?? 0;
    this.nextIndex.set(logicalModel.id, (start + 1) % bindings.length);
    let lastFailure: RouteFailure = { status: 503, message: 'No enabled provider bindings' };

    for (let offset = 0; offset < bindings.length; offset += 1) {
      const binding = bindings[(start + offset) % bindings.length];
      const provider = providers.find((item) => item.id === binding.providerId && item.enabled);
      if (!provider || !provider.models.some((model) => model.id === binding.providerModelId)) continue;

      try {
        const result = await call(provider, binding);
        if (result.status >= 200 && result.status < 300) return result;
        lastFailure = { status: result.status, message: `Upstream returned ${result.status}` };
        if (!isSwitchableStatus(result.status)) throw new AllProvidersFailedError(lastFailure);
      } catch (error) {
        if (error instanceof AllProvidersFailedError) throw error;
        const failure = error as UpstreamFailure;
        lastFailure = { status: failure.status ?? 502, message: failure.publicMessage ?? 'Upstream request failed' };
        if (!isSwitchableStatus(failure.status)) throw new AllProvidersFailedError(lastFailure);
      }
    }

    throw new AllProvidersFailedError(lastFailure);
  }
}
