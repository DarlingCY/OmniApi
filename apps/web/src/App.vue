<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { Message, Modal } from '@arco-design/web-vue';
import { api, getAdminToken, setAdminToken } from './api';
import RuntimeLogs from './RuntimeLogs.vue';
import type { RequestLogFilters } from './api';
import type { AccessKey, Provider, ProviderModel, ProviderProtocol, PublicSettings, RequestLog, RequestLogSummary } from './types';

type View = 'overview' | 'providers' | 'monitor' | 'runtimeLogs' | 'accessKeys' | 'settings';
type Theme = 'light' | 'dark';

const route = useRoute();
const router = useRouter();
const view = computed<View>({
  get: () => route.name as View,
  set: (value) => { void router.push({ name: value }); },
});
const theme = ref<Theme>(document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light');
const loading = ref(true);
const runtimeLogsView = ref<InstanceType<typeof RuntimeLogs>>();
const unauthorized = ref(false);
const providers = ref<Provider[]>([]);
const accessKeys = ref<AccessKey[]>([]);
const requestLogs = ref<RequestLog[]>([]);
const overviewLogs = ref<RequestLog[]>([]);
const requestLogTotal = ref(0);
const monitoringTotal = ref(0);
const serverSummary = ref<RequestLogSummary | null>(null);
const monitoringLoading = ref(false);
const monitoringPage = ref(1);
const monitoringPageSize = 11;
const monitoringFilterForm = reactive({ timeRange: [] as string[], exposedModel: '', upstreamModel: '', providerId: '' });
const monitoringFilters = ref<RequestLogFilters>({});
const monitoringFiltered = computed(() => Object.values(monitoringFilters.value).some((value) => value));

const monitoringExposedModelOptions = computed(() => {
  const targetProviders = monitoringFilterForm.providerId
    ? providers.value.filter((p) => p.id === monitoringFilterForm.providerId)
    : providers.value;
  const set = new Set<string>();
  for (const provider of targetProviders) {
    for (const model of provider.models || []) {
      const name = (model.alias?.trim() || model.upstreamModel?.trim()) ?? '';
      if (name) set.add(name);
    }
  }
  return [...set].sort((a, b) => a.localeCompare(b));
});

const monitoringUpstreamModelOptions = computed(() => {
  const targetProviders = monitoringFilterForm.providerId
    ? providers.value.filter((p) => p.id === monitoringFilterForm.providerId)
    : providers.value;
  const set = new Set<string>();
  for (const provider of targetProviders) {
    for (const model of provider.models || []) {
      const name = model.upstreamModel?.trim() ?? '';
      if (name) set.add(name);
    }
  }
  return [...set].sort((a, b) => a.localeCompare(b));
});

watch(() => monitoringFilterForm.providerId, () => {
  if (monitoringFilterForm.exposedModel && !monitoringExposedModelOptions.value.includes(monitoringFilterForm.exposedModel)) {
    monitoringFilterForm.exposedModel = '';
  }
  if (monitoringFilterForm.upstreamModel && !monitoringUpstreamModelOptions.value.includes(monitoringFilterForm.upstreamModel)) {
    monitoringFilterForm.upstreamModel = '';
  }
});

const settings = ref<PublicSettings>({ hasAdminToken: false });
const providerDrawer = ref(false);
const editingProvider = ref(false);
const saving = ref(false);
const syncingModels = ref(false);
const syncedModels = ref<string[]>([]);
const selectedSyncedModels = ref<string[]>([]);
const modelSearch = ref('');
const tokenInput = ref(getAdminToken());
const settingForm = reactive({ adminToken: '' });
const accessKeyDialog = ref(false);
const createdKeyDialog = ref(false);
const accessKeyName = ref('');
const accessKeySecret = ref('');
const createdSecret = ref('');
const editingAccessKey = ref<AccessKey>();
const revealedAccessKeySecrets = reactive<Record<string, string>>({});
const visibleAccessKeySecrets = reactive<Record<string, boolean>>({});

const protocolLabels: Record<ProviderProtocol, string> = {
  'openai-chat': 'OpenAI Chat Completions',
  'openai-responses': 'OpenAI Responses',
  'anthropic-messages': 'Anthropic Messages',
};

const providerForm = reactive<Provider>({
  id: '', name: '', baseUrl: '', protocol: 'openai-chat', apiKey: '', enabled: true, models: [],
});

interface ExposedModelSource {
  providerId: string;
  providerName: string;
  upstreamModel: string;
}

interface ExposedModel {
  name: string;
  aliased: boolean;
  sources: ExposedModelSource[];
}

const exposedModels = computed<ExposedModel[]>(() => {
  const grouped = new Map<string, ExposedModel>();
  for (const provider of providers.value) {
    if (!provider.enabled) continue;
    for (const model of provider.models) {
      const alias = model.alias?.trim() ?? '';
      const name = alias || model.upstreamModel.trim();
      if (!name) continue;
      const entry = grouped.get(name) ?? { name, aliased: false, sources: [] };
      entry.aliased = entry.aliased || alias !== '';
      entry.sources.push({ providerId: provider.id, providerName: provider.name, upstreamModel: model.upstreamModel.trim() });
      grouped.set(name, entry);
    }
  }
  return [...grouped.values()].sort((left, right) => left.name.localeCompare(right.name));
});
const monitorSummary = computed(() => {
  if (serverSummary.value) {
    return serverSummary.value;
  }
  const successful = requestLogs.value.filter((item) => item.status >= 200 && item.status < 300);
  const totalTokens = requestLogs.value.reduce((sum, item) => sum + item.usage.inputTokens + item.usage.outputTokens, 0);
  const duration = successful.reduce((sum, item) => sum + item.durationMs, 0);
  return {
    totalRequests: monitoringTotal.value || requestLogs.value.length,
    successRate: requestLogs.value.length ? Math.round((successful.length / requestLogs.value.length) * 1000) / 10 : 0,
    totalTokens,
    averageDuration: successful.length ? Math.round(duration / successful.length) : 0,
  };
});
const overviewSummary = computed(() => {
  const successful = overviewLogs.value.filter((item) => item.status >= 200 && item.status < 300);
  const inputTokens = overviewLogs.value.reduce((sum, item) => sum + item.usage.inputTokens, 0);
  const outputTokens = overviewLogs.value.reduce((sum, item) => sum + item.usage.outputTokens, 0);
  const cachedTokens = overviewLogs.value.reduce((sum, item) => sum + item.usage.cachedTokens, 0);
  const cacheCreationTokens = overviewLogs.value.reduce((sum, item) => sum + (item.usage.cacheCreationTokens ?? 0), 0);
  const reasoningTokens = overviewLogs.value.reduce((sum, item) => sum + (item.usage.reasoningTokens ?? 0), 0);
  const generationMs = successful.reduce((sum, item) => sum + Math.max(0, item.durationMs - item.firstTokenMs), 0);
  return {
    sampleSize: overviewLogs.value.length,
    inputTokens,
    outputTokens,
    cachedTokens,
    cacheCreationTokens,
    reasoningTokens,
    totalTokens: inputTokens + outputTokens,
    successCount: successful.length,
    failedCount: overviewLogs.value.length - successful.length,
    successRate: overviewLogs.value.length ? successful.length / overviewLogs.value.length * 100 : 0,
    tps: generationMs ? outputTokens / generationMs * 1000 : 0,
    cacheRate: inputTokens ? cachedTokens / inputTokens * 100 : 0,
  };
});
const trendBuckets = computed(() => {
  const buckets = new Map<string, { label: string; requests: number; tokens: number }>();
  for (const item of [...overviewLogs.value].reverse()) {
    const date = new Date(item.startedAt);
    date.setMinutes(0, 0, 0);
    const key = date.toISOString();
    const label = new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', hour12: false }).format(date);
    const bucket = buckets.get(key) ?? { label, requests: 0, tokens: 0 };
    bucket.requests++;
    bucket.tokens += item.usage.inputTokens + item.usage.outputTokens;
    buckets.set(key, bucket);
  }
  return [...buckets.values()].slice(-24);
});
const requestTrendPoints = computed(() => trendPoints(trendBuckets.value.map((item) => item.requests)));
const tokenTrendPoints = computed(() => trendPoints(trendBuckets.value.map((item) => item.tokens)));
const tokenComposition = computed(() => {
  const { inputTokens, outputTokens, cachedTokens, cacheCreationTokens, reasoningTokens } = overviewSummary.value;
  const regularInputTokens = Math.max(0, inputTokens - cachedTokens - cacheCreationTokens);
  const regularOutputTokens = Math.max(0, outputTokens - reasoningTokens);
  const total = regularInputTokens + regularOutputTokens + cachedTokens + cacheCreationTokens + reasoningTokens;
  return [
    { label: '普通输入', value: regularInputTokens, color: '#165dff' },
    { label: '普通输出', value: regularOutputTokens, color: '#00b42a' },
    { label: '缓存读取', value: cachedTokens, color: '#ff9a2e' },
    { label: '缓存创建', value: cacheCreationTokens, color: '#f53f3f' },
    { label: '思考', value: reasoningTokens, color: '#722ed1' },
  ].map((item) => ({ ...item, percent: total ? item.value / total * 100 : 0 }));
});
const filteredSyncedModels = computed(() => {
  const keyword = modelSearch.value.trim().toLowerCase();
  return keyword ? syncedModels.value.filter((model) => model.toLowerCase().includes(keyword)) : syncedModels.value;
});

function clearSyncedModels() {
  syncedModels.value = [];
  selectedSyncedModels.value = [];
  modelSearch.value = '';
}

function resetProvider(value?: Provider) {
  clearSyncedModels();
  editingProvider.value = Boolean(value);
  Object.assign(providerForm, value ? { ...value, apiKey: '', models: value.models.map((item) => ({ ...item })) } : {
    id: crypto.randomUUID(), name: '', baseUrl: '', protocol: 'openai-chat', apiKey: '', enabled: true, models: [],
  });
  providerDrawer.value = true;
}

function addProviderModel() {
  providerForm.models.push({ id: crypto.randomUUID(), upstreamModel: '', alias: '' });
}

function updateSelectedModels(values: Array<string | number | boolean>) {
  const selected = new Set(values.map(String));
  const existing = new Map<string, ProviderModel>(providerForm.models.map((model) => [model.upstreamModel.trim(), model]));
  providerForm.models = syncedModels.value
    .filter((model) => selected.has(model))
    .map((model) => existing.get(model) ?? { id: crypto.randomUUID(), upstreamModel: model, alias: '' });
  selectedSyncedModels.value = [...selected];
}

async function syncProviderModels() {
  if (!providerForm.baseUrl.trim()) return Message.warning('请先填写 Base URL');
  syncingModels.value = true;
  try {
    const result = await api.syncProviderModels({
      ...(editingProvider.value ? { providerId: providerForm.id } : {}),
      protocol: providerForm.protocol,
      baseUrl: providerForm.baseUrl.trim(),
      ...(providerForm.apiKey?.trim() ? { apiKey: providerForm.apiKey.trim() } : {}),
    });
    syncedModels.value = result.models;
    selectedSyncedModels.value = providerForm.models
      .map((model) => model.upstreamModel.trim())
      .filter((model) => result.models.includes(model));
    modelSearch.value = '';
    if (result.models.length === 0) Message.info('上游未返回可用模型');
    else Message.success(`已同步 ${result.models.length} 个模型`);
  } catch (error) { Message.error((error as Error).message); }
  finally { syncingModels.value = false; }
}

async function loadAll() {
  loading.value = true;
  try {
    const [nextSettings, nextProviders, logs, nextAccessKeys] = await Promise.all([api.settings(), api.providers(), api.requestLogs(200, 0), api.accessKeys()]);
    settings.value = nextSettings;
    providers.value = nextProviders;
    accessKeys.value = nextAccessKeys;
    overviewLogs.value = logs.data;
    requestLogTotal.value = logs.total;
    unauthorized.value = false;
  } catch (error) {
    const failure = error as Error & { status?: number };
    unauthorized.value = failure.status === 401;
    if (!unauthorized.value) Message.error(failure.message);
  } finally {
    loading.value = false;
  }
}

async function loadRequestLogs(silent = false) {
  if (!silent) monitoringLoading.value = true;
  try {
    const result = await api.requestLogs(monitoringPageSize, (monitoringPage.value - 1) * monitoringPageSize, monitoringFilters.value);
    requestLogs.value = result.data;
    monitoringTotal.value = result.total;
    serverSummary.value = result.summary ?? null;
    if (!monitoringFiltered.value) requestLogTotal.value = result.total;
  } catch (error) {
    if (!silent) Message.error((error as Error).message);
  } finally {
    monitoringLoading.value = false;
  }
}

function applyMonitoringFilters() {
  const [startedAfter, startedBefore] = monitoringFilterForm.timeRange;
  monitoringFilters.value = {
    startedAfter: startedAfter ? new Date(startedAfter.replace(' ', 'T')).toISOString() : undefined,
    startedBefore: startedBefore ? new Date(startedBefore.replace(' ', 'T')).toISOString() : undefined,
    exposedModel: monitoringFilterForm.exposedModel.trim() || undefined,
    upstreamModel: monitoringFilterForm.upstreamModel.trim() || undefined,
    providerId: monitoringFilterForm.providerId || undefined,
  };
  if (monitoringPage.value === 1) void loadRequestLogs();
  else monitoringPage.value = 1;
}

function clearMonitoringFilters() {
  monitoringFilterForm.timeRange = [];
  monitoringFilterForm.exposedModel = '';
  monitoringFilterForm.upstreamModel = '';
  monitoringFilterForm.providerId = '';
  monitoringFilters.value = {};
  if (monitoringPage.value === 1) void loadRequestLogs();
  else monitoringPage.value = 1;
}

async function refreshCurrent() {
  if (view.value === 'runtimeLogs') { await runtimeLogsView.value?.refresh(); return; }
  if (view.value === 'monitor') await loadRequestLogs();
  else await loadAll();
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }).format(new Date(value));
}

function formatNumber(value: number) {
  return new Intl.NumberFormat('zh-CN').format(value);
}

function formatTokens(value: number | null | undefined): string {
  if (value === null || value === undefined || isNaN(value)) return '0';
  const abs = Math.abs(value);
  const sign = value < 0 ? '-' : '';
  if (abs >= 1e9) {
    const formatted = (abs / 1e9).toFixed(1).replace(/\.0$/, '');
    return `${sign}${formatted}B`;
  }
  if (abs >= 1e6) {
    const formatted = (abs / 1e6).toFixed(1).replace(/\.0$/, '');
    return `${sign}${formatted}M`;
  }
  if (abs >= 1e3) {
    const formatted = (abs / 1e3).toFixed(1).replace(/\.0$/, '');
    return `${sign}${formatted}K`;
  }
  return `${sign}${abs}`;
}

const formatCompact = formatTokens;

function trendPoints(values: number[]) {
  if (values.length === 0) return '';
  const maximum = Math.max(...values, 1);
  return values.map((value, index) => {
    const x = values.length === 1 ? 50 : index / (values.length - 1) * 100;
    const y = 92 - value / maximum * 76;
    return `${x.toFixed(2)},${y.toFixed(2)}`;
  }).join(' ');
}

function cacheRate(item: RequestLog) {
  return item.usage.inputTokens ? `${((item.usage.cachedTokens / item.usage.inputTokens) * 100).toFixed(1)}%` : '-';
}

function generationSpeed(item: RequestLog) {
  const generationMs = Math.max(1, item.durationMs - item.firstTokenMs);
  return item.usage.outputTokens ? `${(item.usage.outputTokens / generationMs * 1000).toFixed(1)} t/s` : '-';
}

function applyTheme(value: Theme, persist = true) {
  theme.value = value;
  document.documentElement.dataset.theme = value;
  document.documentElement.removeAttribute('arco-theme');
  if (value === 'dark') document.body.setAttribute('arco-theme', 'dark');
  else document.body.removeAttribute('arco-theme');
  if (persist) localStorage.setItem('omni-api-theme', value);
}

function toggleTheme() {
  applyTheme(theme.value === 'dark' ? 'light' : 'dark');
}

const systemTheme = window.matchMedia('(prefers-color-scheme: dark)');
function followSystemTheme(event: MediaQueryListEvent) {
  if (!localStorage.getItem('omni-api-theme')) applyTheme(event.matches ? 'dark' : 'light', false);
}

async function unlock() {
  setAdminToken(tokenInput.value);
  await loadAll();
}

async function saveProvider() {
  if (!providerForm.name.trim() || !providerForm.baseUrl.trim() || providerForm.models.some((item) => !item.upstreamModel.trim())) {
    return Message.warning('请填写供应商名称、地址和全部上游模型');
  }
  const exposedNames = providerForm.models.map((item) => item.alias?.trim() || item.upstreamModel.trim());
  if (new Set(exposedNames).size !== exposedNames.length) {
    return Message.warning('同一供应商内暴露的模型名称不能重复');
  }
  saving.value = true;
  try {
    const payload: Provider = {
      ...providerForm,
      models: providerForm.models.map((item) => ({ ...item, upstreamModel: item.upstreamModel.trim(), alias: item.alias?.trim() || '' })),
    };
    if (editingProvider.value && !payload.apiKey) delete payload.apiKey;
    if (editingProvider.value) await api.updateProvider(payload); else await api.createProvider(payload);
    providerDrawer.value = false;
    Message.success('供应商已保存');
    await loadAll();
  } catch (error) { Message.error((error as Error).message); }
  finally { saving.value = false; }
}

const updatingProviders = reactive<Record<string, boolean>>({});

async function toggleProvider(provider: Provider, enabled: boolean) {
  if (updatingProviders[provider.id]) return;
  updatingProviders[provider.id] = true;
  try {
    const payload = { ...provider, enabled };
    delete payload.apiKey;
    await api.updateProvider(payload);
    provider.enabled = enabled;
    Message.success(enabled ? '供应商已启用' : '供应商已停用');
  } catch (error) { Message.error((error as Error).message); }
  finally { updatingProviders[provider.id] = false; }
}

function confirmDeleteProvider(id: string, name: string) {
  Modal.warning({
    title: '删除供应商',
    content: `确认删除“${name}”？其上游模型与别名会一并移除，操作不可恢复。`,
    hideCancel: false,
    okButtonProps: { status: 'danger' },
    onOk: async () => {
      try {
        await api.deleteProvider(id);
        Message.success('已删除');
        await loadAll();
      } catch (error) { Message.error((error as Error).message); }
    },
  });
}

async function saveSettings() {
  const nextToken = settingForm.adminToken.trim();
  saving.value = true;
  try {
    await api.saveSettings({ adminToken: nextToken || undefined });
    if (nextToken) {
      setAdminToken(nextToken);
      tokenInput.value = nextToken;
    }
    settingForm.adminToken = '';
    Message.success('管理 Token 已更新');
    await loadAll();
  } catch (error) { Message.error((error as Error).message); }
  finally { saving.value = false; }
}

function openAccessKeyDialog(accessKey?: AccessKey) {
  editingAccessKey.value = accessKey;
  accessKeyName.value = accessKey?.name ?? '';
  accessKeySecret.value = '';
  accessKeyDialog.value = true;
}

function generateAccessKeySecret() {
  const raw = crypto.getRandomValues(new Uint8Array(16));
  accessKeySecret.value = `oa_${[...raw].map((value) => value.toString(16).padStart(2, '0')).join('')}`;
}

async function saveAccessKey() {
  const name = accessKeyName.value.trim();
  if (!name) return Message.warning('请输入密钥名称');
  const secret = accessKeySecret.value.trim();
  if (!editingAccessKey.value && !secret) return Message.warning('请输入密钥，或点击生成');
  saving.value = true;
  try {
    if (editingAccessKey.value) {
      await api.updateAccessKey({ ...editingAccessKey.value, name });
      Message.success('密钥名称已更新');
    } else {
      const created = await api.createAccessKey(name, secret);
      createdSecret.value = created.secret ?? '';
      createdKeyDialog.value = true;
    }
    accessKeyDialog.value = false;
    accessKeyName.value = '';
    accessKeySecret.value = '';
    editingAccessKey.value = undefined;
    accessKeys.value = await api.accessKeys();
  } catch (error) { Message.error((error as Error).message); }
  finally { saving.value = false; }
}

async function toggleAccessKey(accessKey: AccessKey, enabled: boolean) {
  try {
    await api.updateAccessKey({ ...accessKey, enabled });
    accessKey.enabled = enabled;
    Message.success(enabled ? '密钥已启用' : '密钥已停用');
  } catch (error) { Message.error((error as Error).message); }
}

async function getAccessKeySecret(accessKey: AccessKey) {
  const cached = revealedAccessKeySecrets[accessKey.id];
  if (cached) return cached;
  const detail = await api.accessKey(accessKey.id);
  if (!detail.secret) throw new Error('密钥读取失败');
  revealedAccessKeySecrets[accessKey.id] = detail.secret;
  return detail.secret;
}

async function toggleAccessKeySecret(accessKey: AccessKey) {
  if (visibleAccessKeySecrets[accessKey.id]) {
    delete visibleAccessKeySecrets[accessKey.id];
    return;
  }
  try {
    await getAccessKeySecret(accessKey);
    visibleAccessKeySecrets[accessKey.id] = true;
  }
  catch (error) { Message.error((error as Error).message); }
}

async function copyAccessKeySecret(accessKey: AccessKey) {
  try {
    await navigator.clipboard.writeText(await getAccessKeySecret(accessKey));
    Message.success('密钥已复制');
  } catch (error) { Message.error((error as Error).message || '复制失败，请先查看后手动复制'); }
}

function confirmDeleteAccessKey(accessKey: AccessKey) {
  Modal.warning({
    title: '删除鉴权密钥',
    content: `确认删除“${accessKey.name}”？使用此密钥的外部调用将立即失效，且操作不可恢复。`,
    hideCancel: false,
    okButtonProps: { status: 'danger' },
    onOk: async () => {
      try {
        await api.deleteAccessKey(accessKey.id);
        accessKeys.value = accessKeys.value.filter((item) => item.id !== accessKey.id);
        delete revealedAccessKeySecrets[accessKey.id];
        delete visibleAccessKeySecrets[accessKey.id];
        Message.success('鉴权密钥已删除');
      } catch (error) { Message.error((error as Error).message); }
    },
  });
}

async function copyCreatedSecret() {
  try {
    await navigator.clipboard.writeText(createdSecret.value);
    Message.success('密钥已复制');
  } catch { Message.error('复制失败，请手动复制'); }
}

function closeCreatedKeyDialog() {
  createdKeyDialog.value = false;
  createdSecret.value = '';
}

watch(
  () => [providerForm.protocol, providerForm.baseUrl, providerForm.apiKey],
  () => {
    if (syncedModels.value.length > 0) clearSyncedModels();
  },
);

let monitoringTimer: number | undefined;
watch([view, loading, unauthorized], ([next, isLoading, isUnauthorized]) => {
  if (monitoringTimer) window.clearInterval(monitoringTimer);
  monitoringTimer = undefined;
  if (next === 'monitor' && !isLoading && !isUnauthorized) {
    monitoringPage.value = 1;
    void loadRequestLogs();
    monitoringTimer = window.setInterval(() => void loadRequestLogs(true), 5000);
  }
});

watch(monitoringPage, () => void loadRequestLogs());

onMounted(() => {
  systemTheme.addEventListener('change', followSystemTheme);
  void loadAll();
});
onUnmounted(() => {
  if (monitoringTimer) window.clearInterval(monitoringTimer);
  systemTheme.removeEventListener('change', followSystemTheme);
});
</script>

<template>
  <a-spin :loading="loading" class="app-spin">
    <div v-if="unauthorized" class="unlock-shell">
      <section class="unlock-panel">
        <div class="brand-mark">OA</div>
        <h1>连接 OmniApi</h1>
        <p>管理接口已启用访问令牌，请输入管理 Token 继续。</p>
        <a-input-password v-model="tokenInput" placeholder="管理 Token" allow-clear @press-enter="unlock" />
        <a-button type="primary" long @click="unlock">连接控制台</a-button>
      </section>
    </div>

    <div v-else class="shell">
      <aside class="sidebar">
        <div class="brand"><span class="brand-mark small">OA</span><div><strong>OmniApi</strong><small>LOCAL GATEWAY</small></div></div>
        <nav>
          <button :class="{ active: view === 'overview' }" @click="view = 'overview'"><icon-dashboard />概览</button>
          <button :class="{ active: view === 'providers' }" @click="view = 'providers'"><icon-storage />供应商</button>
          <button :class="{ active: view === 'monitor' }" @click="view = 'monitor'"><icon-safe />请求监控</button>
          <button :class="{ active: view === 'runtimeLogs' }" @click="view = 'runtimeLogs'"><icon-code />运行日志</button>
          <button :class="{ active: view === 'accessKeys' }" @click="view = 'accessKeys'"><icon-lock />鉴权密钥</button>
          <button :class="{ active: view === 'settings' }" @click="view = 'settings'"><icon-settings />访问设置</button>
        </nav>
        <div class="sidebar-footer">
          <a-tooltip :content="theme === 'dark' ? '切换到亮色模式' : '切换到暗色模式'" position="right">
            <button class="theme-toggle-btn" :aria-label="theme === 'dark' ? '切换到亮色模式' : '切换到暗色模式'" @click="toggleTheme">
              <icon-sun v-if="theme === 'dark'" />
              <icon-moon v-else />
              <span>{{ theme === 'dark' ? '亮色模式' : '暗色模式' }}</span>
            </button>
          </a-tooltip>
        </div>
      </aside>

      <main>
        <RuntimeLogs v-if="view === 'runtimeLogs' && !loading && !unauthorized" ref="runtimeLogsView" />

        <section v-if="view === 'overview'" class="stack">
          <div class="overview-metrics">
            <article><span>请求总数</span><strong>{{ formatNumber(requestLogTotal) }}</strong><small>最近样本 {{ overviewSummary.sampleSize }} 条</small></article>
            <article><span>Token 总量</span><strong>{{ formatCompact(overviewSummary.totalTokens) }}</strong><small>输入 {{ formatCompact(overviewSummary.inputTokens) }} · 输出 {{ formatCompact(overviewSummary.outputTokens) }}</small></article>
            <article><span>成功率</span><strong>{{ overviewSummary.successRate.toFixed(1) }}%</strong><small>成功 {{ overviewSummary.successCount }} · 失败 {{ overviewSummary.failedCount }}</small></article>
            <article><span>TPS</span><strong>{{ overviewSummary.tps.toFixed(1) }}</strong><small>基于样本内输出速度</small></article>
            <article><span>缓存命中率</span><strong>{{ overviewSummary.cacheRate.toFixed(1) }}%</strong><small>读取 {{ formatCompact(overviewSummary.cachedTokens) }} · 创建 {{ formatCompact(overviewSummary.cacheCreationTokens) }}</small></article>
          </div>
          <div class="overview-analytics">
            <section class="surface trend-panel">
              <div class="section-heading"><h2>请求与 Token 趋势</h2><div class="trend-legend"><span class="requests">请求</span><span class="tokens">Token</span></div></div>
              <a-empty v-if="trendBuckets.length === 0" description="暂无请求趋势数据" />
              <div v-else class="trend-chart">
                <svg viewBox="0 0 100 100" preserveAspectRatio="none" role="img" aria-label="请求与 Token 趋势">
                  <line v-for="line in [20, 40, 60, 80]" :key="line" x1="0" :y1="line" x2="100" :y2="line" class="chart-grid" />
                  <polyline :points="tokenTrendPoints" class="trend-line tokens" />
                  <polyline :points="requestTrendPoints" class="trend-line requests" />
                </svg>
                <div class="trend-axis"><span>{{ trendBuckets[0]?.label }}</span><span>{{ trendBuckets[Math.floor(trendBuckets.length / 2)]?.label }}</span><span>{{ trendBuckets[trendBuckets.length - 1]?.label }}</span></div>
              </div>
            </section>
            <section class="surface token-panel">
              <div class="section-heading"><h2>Token 构成</h2></div>
              <a-empty v-if="overviewSummary.totalTokens === 0 && overviewSummary.cacheCreationTokens === 0 && overviewSummary.reasoningTokens === 0" description="暂无 Token 用量数据" />
              <div v-else class="token-composition">
                <div v-for="item in tokenComposition" :key="item.label" class="token-part">
                  <div><strong>{{ item.label }}</strong><span>{{ formatCompact(item.value) }} <small>{{ item.percent.toFixed(1) }}%</small></span></div>
                  <div class="token-track"><i :style="{ width: `${item.percent}%`, background: item.color }"></i></div>
                </div>
              </div>
            </section>
          </div>
          <section class="surface">
            <div class="section-heading"><h2>可用模型</h2><a-button @click="view = 'providers'"><template #icon><icon-storage /></template>编辑模型</a-button></div>
            <a-empty v-if="exposedModels.length === 0" description="当前没有可用模型" />
            <div v-else class="model-chips">
              <a-tag v-for="model in exposedModels" :key="model.name" :color="model.aliased ? 'arcoblue' : undefined">
                {{ model.name }} · {{ model.sources.length }} 个供应商
              </a-tag>
            </div>
          </section>
        </section>

        <section v-if="view === 'providers'" class="surface">
          <div class="section-heading"><h2>供应商</h2><a-button type="primary" @click="resetProvider()"><template #icon><icon-plus /></template>新增供应商</a-button></div>
          <a-empty v-if="providers.length === 0" description="尚未配置供应商" />
          <div v-else class="data-list">
            <article v-for="provider in providers" :key="provider.id" class="data-row">
              <div class="data-main"><div class="title-line"><strong>{{ provider.name }}</strong><a-tag :color="provider.enabled ? 'green' : 'gray'">{{ provider.enabled ? '启用' : '停用' }}</a-tag></div><span>{{ protocolLabels[provider.protocol] }}</span><code>{{ provider.baseUrl }}</code></div>
              <div class="model-chips"><a-tag v-for="model in provider.models" :key="model.id" :color="model.alias?.trim() ? 'arcoblue' : undefined">{{ model.alias?.trim() || model.upstreamModel }}</a-tag></div>
              <div class="row-actions"><a-switch :model-value="provider.enabled" :loading="updatingProviders[provider.id]" :aria-label="provider.enabled ? '停用供应商' : '启用供应商'" @change="toggleProvider(provider, $event as boolean)" /><a-button :disabled="updatingProviders[provider.id]" @click="resetProvider(provider)"><template #icon><icon-edit /></template></a-button><a-button type="primary" status="danger" :disabled="updatingProviders[provider.id]" @click="confirmDeleteProvider(provider.id, provider.name)"><template #icon><icon-delete /></template></a-button></div>
            </article>
          </div>
        </section>

        <section v-if="view === 'monitor'" class="surface monitor-view">
          <div class="section-heading"><h2>请求监控</h2></div>
          <a-form :model="monitoringFilterForm" class="monitor-filters" layout="inline" @submit-success="applyMonitoringFilters">
            <a-form-item label="时间" class="monitor-filter-time"><a-range-picker v-model="monitoringFilterForm.timeRange" show-time value-format="YYYY-MM-DD HH:mm:ss" format="YYYY-MM-DD HH:mm:ss" /></a-form-item>
            <a-form-item label="请求模型"><a-select v-model="monitoringFilterForm.exposedModel" allow-clear allow-search placeholder="全部请求模型"><a-option v-for="modelName in monitoringExposedModelOptions" :key="modelName" :value="modelName">{{ modelName }}</a-option></a-select></a-form-item>
            <a-form-item label="实际模型"><a-select v-model="monitoringFilterForm.upstreamModel" allow-clear allow-search placeholder="全部实际模型"><a-option v-for="modelName in monitoringUpstreamModelOptions" :key="modelName" :value="modelName">{{ modelName }}</a-option></a-select></a-form-item>
            <a-form-item label="Provider"><a-select v-model="monitoringFilterForm.providerId" allow-clear placeholder="全部 Provider"><a-option v-for="provider in providers" :key="provider.id" :value="provider.id">{{ provider.name }}</a-option></a-select></a-form-item>
            <div class="monitor-filter-actions"><a-button @click="clearMonitoringFilters">重置</a-button><a-button type="primary" html-type="submit">查询</a-button></div>
          </a-form>
          <div class="monitor-metrics">
            <article><span>{{ monitoringFiltered ? '筛选结果' : '记录总数' }}</span><strong>{{ formatNumber(monitoringTotal) }}</strong><small>{{ monitoringFiltered ? `共 ${formatNumber(requestLogTotal)} 条记录` : '当前保存在本机 SQLite' }}</small></article>
            <article><span>{{ monitoringFiltered ? '筛选成功率' : '总体成功率' }}</span><strong>{{ monitorSummary.successRate }}%</strong><small>{{ formatNumber(monitorSummary.totalRequests ?? monitoringTotal) }} 条请求样本</small></article>
            <article><span>{{ monitoringFiltered ? '筛选 Token' : '消耗 Token' }}</span><strong>{{ formatTokens(monitorSummary.totalTokens) }}</strong><small>输入与输出合计</small></article>
            <article><span>平均耗时</span><strong>{{ monitorSummary.averageDuration }} ms</strong><small>仅统计成功请求</small></article>
          </div>
          <div class="section-heading monitor-table-heading"><h2>请求明细</h2></div>
          <a-spin :loading="monitoringLoading" class="monitor-spin">
            <a-empty v-if="requestLogs.length === 0" description="暂无代理请求记录" />
            <div v-else class="monitor-table-wrap">
              <table class="monitor-table">
                <thead><tr><th>时间</th><th>请求模型</th><th>实际模型</th><th>输入</th><th>输出</th><th>缓存读取</th><th>缓存创建</th><th>思考</th><th>缓存率</th><th>总计</th><th>生成速度</th><th>首字延迟</th><th>总耗时</th><th>结果</th><th>Provider</th></tr></thead>
                <tbody>
                  <tr v-for="item in requestLogs" :key="item.id">
                    <td>{{ formatTime(item.startedAt) }}</td>
                    <td><strong>{{ item.exposedModel }}</strong></td>
                    <td><strong>{{ item.upstreamModel || '-' }}</strong></td>
                    <td :title="formatNumber(item.usage.inputTokens)">{{ formatTokens(item.usage.inputTokens) }}</td>
                    <td :title="formatNumber(item.usage.outputTokens)">{{ formatTokens(item.usage.outputTokens) }}</td>
                    <td :title="formatNumber(item.usage.cachedTokens)">{{ formatTokens(item.usage.cachedTokens) }}</td>
                    <td :title="formatNumber(item.usage.cacheCreationTokens ?? 0)">{{ formatTokens(item.usage.cacheCreationTokens ?? 0) }}</td>
                    <td :title="formatNumber(item.usage.reasoningTokens ?? 0)">{{ formatTokens(item.usage.reasoningTokens ?? 0) }}</td>
                    <td>{{ cacheRate(item) }}</td>
                    <td :title="formatNumber(item.usage.inputTokens + item.usage.outputTokens)"><strong>{{ formatTokens(item.usage.inputTokens + item.usage.outputTokens) }}</strong></td>
                    <td>{{ generationSpeed(item) }}</td><td>{{ item.firstTokenMs ? formatNumber(item.firstTokenMs) + ' ms' : '-' }}</td><td>{{ formatNumber(item.durationMs) }} ms</td>
                    <td>
                      <a-tooltip v-if="item.error" :content="item.error" position="top">
                        <a-tag :color="item.status === 200 ? 'green' : 'red'" class="status-tag-error">
                          {{ item.status }}
                        </a-tag>
                      </a-tooltip>
                      <a-tag v-else :color="item.status === 200 ? 'green' : 'red'">
                        {{ item.status }}
                      </a-tag>
                      <small v-if="item.attempts > 1">{{ item.attempts }} 次重试</small>
                    </td>
                    <td><a-tag>{{ item.providerName || '-' }}</a-tag></td>
                  </tr>
                </tbody>
              </table>
            </div>
          </a-spin>
          <div v-if="monitoringTotal > 0" class="monitor-pagination"><span>共 {{ formatNumber(monitoringTotal) }} 条</span><a-pagination v-model:current="monitoringPage" :total="monitoringTotal" :page-size="monitoringPageSize" /></div>
        </section>

        <section v-if="view === 'settings'" class="surface settings-panel">
          <div class="section-heading"><h2>访问设置</h2></div>
          <a-alert type="warning">出于安全考虑，已保存的管理 Token 不会回显。留空表示不修改。</a-alert>
          <a-form :model="settingForm" layout="vertical" @submit-success="saveSettings">
            <a-form-item label="新管理 Token"><a-input-password v-model="settingForm.adminToken" :placeholder="settings.hasAdminToken ? '已设置，输入新值以替换' : '首次设置后控制台需要此 Token'" /></a-form-item>
            <a-button type="primary" html-type="submit" :loading="saving">保存管理 Token</a-button>
          </a-form>
        </section>

        <section v-if="view === 'accessKeys'" class="surface access-keys-surface">
          <div class="section-heading"><h2>鉴权密钥</h2><a-button type="primary" @click="openAccessKeyDialog()"><template #icon><icon-plus /></template>新增密钥</a-button></div>
          <a-empty v-if="accessKeys.length === 0" description="尚未创建鉴权密钥" />
          <div v-else class="access-key-grid">
            <article v-for="accessKey in accessKeys" :key="accessKey.id" class="access-key-card">
              <div class="access-key-card-head">
                <div class="data-main">
                  <div class="title-line"><strong>{{ accessKey.name }}</strong><a-tag :color="accessKey.enabled ? 'green' : 'gray'">{{ accessKey.enabled ? '启用' : '停用' }}</a-tag></div>
                  <span>创建于 {{ formatTime(accessKey.createdAt) }}</span>
                </div>
                <div class="row-actions">
                  <a-switch :model-value="accessKey.enabled" :aria-label="accessKey.enabled ? '停用密钥' : '启用密钥'" @change="toggleAccessKey(accessKey, $event as boolean)" />
                  <a-tooltip :content="visibleAccessKeySecrets[accessKey.id] ? '隐藏密钥' : '查看密钥'"><a-button :aria-label="visibleAccessKeySecrets[accessKey.id] ? '隐藏密钥' : '查看密钥'" @click="toggleAccessKeySecret(accessKey)"><template #icon><icon-eye-invisible v-if="visibleAccessKeySecrets[accessKey.id]" /><icon-eye v-else /></template></a-button></a-tooltip>
                  <a-tooltip content="复制密钥"><a-button aria-label="复制密钥" @click="copyAccessKeySecret(accessKey)"><template #icon><icon-copy /></template></a-button></a-tooltip>
                  <a-tooltip content="编辑名称"><a-button @click="openAccessKeyDialog(accessKey)"><template #icon><icon-edit /></template></a-button></a-tooltip>
                  <a-tooltip content="删除密钥"><a-button type="primary" status="danger" @click="confirmDeleteAccessKey(accessKey)"><template #icon><icon-delete /></template></a-button></a-tooltip>
                </div>
              </div>
              <div class="access-key-card-code">
                <code>{{ visibleAccessKeySecrets[accessKey.id] ? revealedAccessKeySecrets[accessKey.id] : accessKey.prefix }}</code>
              </div>
            </article>
          </div>
        </section>
      </main>
    </div>

    <a-drawer v-model:visible="providerDrawer" :width="560" :footer="false" unmount-on-close>
      <template #title>{{ editingProvider ? '编辑供应商' : '新增供应商' }}</template>
      <a-form :model="providerForm" layout="vertical" @submit-success="saveProvider">
        <div class="form-grid"><a-form-item label="名称" required><a-input v-model="providerForm.name" placeholder="例如 OpenAI 主线路" /></a-form-item><a-form-item label="启用"><a-switch v-model="providerForm.enabled" /></a-form-item></div>
        <a-form-item label="协议" required><a-select v-model="providerForm.protocol"><a-option v-for="(label, value) in protocolLabels" :key="value" :value="value">{{ label }}</a-option></a-select></a-form-item>
        <a-form-item label="Base URL" required><a-input v-model="providerForm.baseUrl" placeholder="https://api.openai.com" /></a-form-item>
        <a-form-item label="API Key"><a-input-password v-model="providerForm.apiKey" :placeholder="editingProvider ? '留空表示保留现有密钥' : '供应商 API Key'" /></a-form-item>
        <div class="subheading"><strong>上游模型</strong><div class="subheading-actions"><a-button size="small" :loading="syncingModels" @click="syncProviderModels"><template #icon><icon-refresh /></template>同步上游模型</a-button><a-button size="small" @click="addProviderModel"><template #icon><icon-plus /></template>手动添加</a-button></div></div>
        <section v-if="syncedModels.length > 0" class="model-picker">
          <div class="model-picker-toolbar"><a-input-search v-model="modelSearch" allow-clear placeholder="搜索上游模型" /><div><a-button size="mini" @click="updateSelectedModels(syncedModels)">全选</a-button><a-button size="mini" @click="updateSelectedModels([])">清空</a-button></div></div>
          <a-checkbox-group :model-value="selectedSyncedModels" class="model-options" @change="updateSelectedModels">
            <a-checkbox v-for="model in filteredSyncedModels" :key="model" :value="model">{{ model }}</a-checkbox>
          </a-checkbox-group>
          <a-empty v-if="filteredSyncedModels.length === 0" description="没有匹配的模型" />
          <small>已选择 {{ selectedSyncedModels.length }} / {{ syncedModels.length }} 个模型</small>
        </section>
        <a-empty v-if="providerForm.models.length === 0" description="至少添加一个上游模型" />
        <div v-else class="form-list model-row-list">
          <div class="model-row-head"><span>模型 ID</span><span>模型别名</span><span></span></div>
          <div v-for="(model, index) in providerForm.models" :key="model.id"><a-input v-model="model.upstreamModel" placeholder="例如 gpt-5-mini" /><a-input v-model="model.alias" allow-clear placeholder="留空则暴露模型 ID" /><a-button status="danger" @click="providerForm.models.splice(index, 1)"><template #icon><icon-delete /></template></a-button></div>
        </div>
        <div class="drawer-actions"><a-button @click="providerDrawer = false">取消</a-button><a-button type="primary" html-type="submit" :loading="saving">保存供应商</a-button></div>
      </a-form>
    </a-drawer>

    <a-modal v-model:visible="accessKeyDialog" :title="editingAccessKey ? '编辑鉴权密钥' : '新增鉴权密钥'" :ok-loading="saving" @ok="saveAccessKey" @cancel="accessKeyName = ''; accessKeySecret = ''">
      <a-form :model="{ accessKeyName, accessKeySecret }" layout="vertical">
        <a-form-item label="密钥名称" required><a-input v-model="accessKeyName" placeholder="例如 Claude Code - 笔记本" allow-clear /></a-form-item>
        <a-form-item v-if="!editingAccessKey" label="密钥" required>
          <div class="secret-field">
            <a-input v-model="accessKeySecret" placeholder="输入自定义密钥，或点击生成" allow-clear @press-enter="saveAccessKey" />
            <a-button @click="generateAccessKeySecret"><template #icon><icon-refresh /></template>生成</a-button>
          </div>
        </a-form-item>
      </a-form>
    </a-modal>

    <a-modal :visible="createdKeyDialog" title="鉴权密钥已创建" :hide-cancel="true" ok-text="我已保存" :mask-closable="false" :esc-to-close="false" @ok="closeCreatedKeyDialog">
      <a-alert type="warning">请立即保存此密钥，关闭后将无法再次查看。</a-alert>
      <div class="created-secret"><code>{{ createdSecret }}</code><a-tooltip content="复制密钥"><a-button @click="copyCreatedSecret"><template #icon><icon-copy /></template></a-button></a-tooltip></div>
    </a-modal>
  </a-spin>
</template>
