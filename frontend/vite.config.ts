import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// The dev server proxies /api and /v1 to the Go backend so the browser sees a
// single origin — no CORS preflight, and cookies/headers stay simple.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5273,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/v1': { target: 'http://127.0.0.1:8787', changeOrigin: true },
    },
  },
  build: {
    // 直接构建进后端的 go:embed 目录，不再有「frontend/dist → 拷贝到
    // backend/internal/server/dist」这一步。手工拷贝是"合并"语义，旧的
    // hash 文件永远不会被清掉 —— 历史上这里堆到 1658 个文件 / 141 MB，
    // 全部被打进 exe（二进制 167 MB）。
    outDir: '../backend/internal/server/dist',
    // outDir 在项目根之外时 vite 默认不清理，必须显式打开。
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
})
