import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  root: fileURLToPath(new URL('.', import.meta.url)),
  plugins: [vue()],
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': process.env.OMNI_API_SERVER ?? 'http://127.0.0.1:47832',
    },
  },
  build: {
    outDir: fileURLToPath(new URL('../../internal/webui/dist', import.meta.url)),
    emptyOutDir: true,
  },
});
