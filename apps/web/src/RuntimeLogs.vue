<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { api } from './api';
import type { RuntimeLog } from './types';

const logs = ref<RuntimeLog[]>([]);
const capacity = ref(1000);
const busy = ref(false);
const error = ref('');
const updatedAt = ref('');
const requestId = ref('');
let timer: number | undefined;
let controller: AbortController | undefined;
let disposed = false;

const visibleLogs = computed(() => {
  const source = requestId.value
    ? logs.value.filter((entry) => entry.requestId === requestId.value)
    : logs.value;
  return source.slice(-22).reverse();
});

function time(value: string) {
  return new Date(value).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, fractionalSecondDigits: 3 });
}

async function refresh() {
  if (busy.value || disposed) return;
  busy.value = true;
  controller = new AbortController();
  const timeout = window.setTimeout(() => controller?.abort(), 8000);
  try {
    const result = await api.runtimeLogs(controller.signal);
    if (disposed) return;
    logs.value = result.data;
    capacity.value = result.capacity;
    updatedAt.value = time(new Date().toISOString());
    error.value = '';
  } catch (failure) {
    if (!disposed) {
      error.value = (failure as Error).name === 'AbortError' ? '日志连接超时，正在重试' : (failure as Error).message;
    }
  } finally {
    window.clearTimeout(timeout);
    busy.value = false;
  }
}

async function poll() {
  await refresh();
  if (!disposed) timer = window.setTimeout(() => void poll(), 1000);
}

onMounted(() => void poll());
onUnmounted(() => {
  disposed = true;
  window.clearTimeout(timer);
  controller?.abort();
});
defineExpose({ refresh });
</script>

<template>
  <section class="surface runtime-panel">
    <div class="section-heading">
      <h2>运行日志</h2>
      <div class="runtime-controls">
        <a-tag v-if="requestId" closable color="arcoblue" @close="requestId = ''">请求: {{ requestId }}</a-tag>
        <a-tag :color="error ? 'red' : 'green'">{{ error ? '连接异常' : '自动刷新中' }}</a-tag>
      </div>
    </div>
    <a-alert v-if="error" type="error" class="runtime-error">{{ error }}，上次日志已保留。</a-alert>
    <div class="runtime-list" role="region" aria-label="运行日志详情" tabindex="0">
      <a-empty v-if="visibleLogs.length === 0" :description="busy && !updatedAt ? '正在连接日志…' : '暂无运行日志'" />
      <article v-for="entry in visibleLogs" :key="entry.id" class="runtime-entry" :class="`runtime-${entry.level}`">
        <div class="runtime-line">
          <time :datetime="entry.time" :title="new Date(entry.time).toLocaleString()">{{ time(entry.time) }}</time>
          <span class="runtime-level">{{ entry.level.toUpperCase() }}</span>
          <span class="runtime-content" :title="entry.detail ? `${entry.message} ${entry.detail}` : entry.message">
            <strong class="runtime-message">{{ entry.message }}</strong>
            <span v-if="entry.detail" class="runtime-detail">{{ entry.detail }}</span>
          </span>
          <button v-if="entry.requestId" class="runtime-request" title="只看此请求的完整链路" @click="requestId = entry.requestId">{{ entry.requestId }}</button>
        </div>
      </article>
    </div>
  </section>
</template>

<style scoped>
.runtime-panel { padding: 20px 24px; min-width: 0; flex: 1; display: flex; flex-direction: column; min-height: 0; width: 100%; box-sizing: border-box; }
.runtime-controls { display: flex; align-items: center; gap: 12px; }
.runtime-error { margin-bottom: 14px; flex-shrink: 0; }
.runtime-list { flex: 1; min-height: 0; overflow: auto; border: 1px solid var(--line); border-radius: 8px; background: var(--surface-subtle); overflow-anchor: none; }
.runtime-entry { padding: 8px 16px; border-bottom: 1px solid var(--line); border-left: 3px solid transparent; }
.runtime-entry:last-child { border-bottom: 0; }
.runtime-line { display: flex; align-items: baseline; min-width: 0; gap: 10px; font-size: 12px; line-height: 1.5; }
.runtime-line time { color: var(--muted); font-family: Consolas, monospace; white-space: nowrap; flex-shrink: 0; }
.runtime-level { font: 600 11px Consolas, monospace; min-width: 40px; color: var(--accent); white-space: nowrap; flex-shrink: 0; }
.runtime-content { display: block; min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.runtime-message { margin-right: 10px; white-space: nowrap; }
.runtime-detail { color: var(--code); font: 12px/1.5 Consolas, "Microsoft YaHei UI", monospace; }
.runtime-warn { border-left-color: #ff9a2e; }
.runtime-warn .runtime-level { color: var(--amber); }
.runtime-error.runtime-entry { margin-top: 0; border-left-color: #f53f3f; }
.runtime-error .runtime-level { color: #f53f3f; }
.runtime-request { flex-shrink: 0; color: var(--nav-primary); cursor: pointer; border: 0; background: transparent; padding: 0; font: 11px Consolas, monospace; white-space: nowrap; margin-left: auto; }
.runtime-request:hover { text-decoration: underline; }
@media (max-width: 700px) {
  .runtime-panel { padding: 16px; }
}
</style>
