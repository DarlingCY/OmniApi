import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import type { AppConfig, Provider, Settings } from '../../core/src/types.js';

interface PersistedConfig extends Omit<AppConfig, 'providers' | 'settings'> {
  settings: { adminToken?: string; proxyToken?: string };
  providers: Array<Omit<Provider, 'apiKey'> & { apiKey?: string }>;
}

export interface SecretCodec {
  encrypt(value: string): string;
  decrypt(value: string): string;
}

/** Development/test-only codec; production secrets are encrypted by the Go gateway. */
export class PlaintextCodec implements SecretCodec {
  encrypt(value: string): string { return value; }
  decrypt(value: string): string { return value; }
}

export class ConfigStore {
  private config: AppConfig = { settings: {}, providers: [], logicalModels: [] };

  constructor(private readonly filePath: string, private readonly secrets: SecretCodec) {}

  async load(): Promise<AppConfig> {
    try {
      const stored = JSON.parse(await readFile(this.filePath, 'utf8')) as PersistedConfig;
      this.config = {
        ...stored,
        settings: {
          adminToken: stored.settings.adminToken && this.secrets.decrypt(stored.settings.adminToken),
          proxyToken: stored.settings.proxyToken && this.secrets.decrypt(stored.settings.proxyToken),
        },
        providers: stored.providers.map((provider) => ({
          ...provider,
          apiKey: provider.apiKey && this.secrets.decrypt(provider.apiKey),
        })),
      };
    } catch (error: unknown) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    return this.get();
  }

  get(): AppConfig { return structuredClone(this.config); }

  async replace(config: AppConfig): Promise<void> {
    this.config = structuredClone(config);
    const stored: PersistedConfig = {
      ...this.config,
      settings: {
        adminToken: this.config.settings.adminToken && this.secrets.encrypt(this.config.settings.adminToken),
        proxyToken: this.config.settings.proxyToken && this.secrets.encrypt(this.config.settings.proxyToken),
      },
      providers: this.config.providers.map((provider) => ({
        ...provider,
        apiKey: provider.apiKey && this.secrets.encrypt(provider.apiKey),
      })),
    };
    await mkdir(dirname(this.filePath), { recursive: true });
    const temporaryPath = `${this.filePath}.tmp`;
    await writeFile(temporaryPath, JSON.stringify(stored, null, 2), 'utf8');
    await rename(temporaryPath, this.filePath);
  }

  async updateSettings(settings: Settings): Promise<void> {
    await this.replace({ ...this.config, settings: { ...this.config.settings, ...settings } });
  }
}
