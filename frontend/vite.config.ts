import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import { fileURLToPath, URL } from 'node:url'

// The dev server proxies /api and /v1 to the Go backend so the browser sees a
// single origin — no CORS preflight, and cookies/headers stay simple.
export default defineConfig({
  plugins: [
    vue(),
    // Element Plus 按需引入。
    //
    // 之前是 main.ts 里 app.use(ElementPlus) + 全量 element-plus/dist/index.css，
    // 于是首屏那个 chunk 里塞着整个组件库（1.22 MB / gzip 391 kB），用户为
    // 十几个用不到的组件付出下载与解析代价。这里改成模板里出现哪个 <el-xxx>
    // 就 import 哪个，样式同样按组件引入（每个组件的 style/css 自己会带上
    // base.css 及其依赖）。
    //
    // 解析不到的两类东西要手写，见 main.ts：服务式 API（ElMessage /
    // ElMessageBox）和指令（v-loading）。
    Components({
      resolvers: [ElementPlusResolver({ importStyle: 'css' })],
      // 生成 src/components.d.ts，让 vue-tsc / IDE 认识自动引入的组件。
      dts: 'src/components.d.ts',
    }),
  ],
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
  // 自动引入的组件类型声明在构建/开发时写入 src/components.d.ts；
  // 它不改运行时行为。
})
