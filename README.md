# AgentHub

本机智能体代理：一个二进制，跑起来就能用。

**Key 保险箱 + 多服务商网关 + 角色/技能库 + 知识库与记忆 + 软件控制。**

## 快速开始

需要 Go 1.26+ 与 Node 18+。一条命令出包（前端类型检查 → 构建进 `go:embed` 目录 → 编后端）：

```powershell
.\build.ps1
.\backend\agenthub.exe --addr 127.0.0.1:8787 --data .\backend\data
```

打开 http://127.0.0.1:8787 即可使用。

`build.ps1` 就是下面两步的串联，手工执行等价：

```bash
cd frontend && npm run build    # 直接构建进 backend/internal/server/dist
cd ../backend && go build -o agenthub.exe ./cmd/agenthub
```

> 前端产物不进版本库：`backend/internal/server/dist` 每次构建整体重写。
> 全新 clone 直接 `go build` 也能过（里面留了 `favicon.svg` 当 embed 锚点），
> 但这种二进制的 `/` 会返回一页「前端未构建」，API 照常可用。

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 单二进制，零外部依赖 |
| 前端 | Vue 3 + TypeScript + Vite + Element Plus |
| 存储 | SQLite（纯 Go，无 CGO） |
| 部署 | 前端 build → `go:embed` 打进 exe，单文件分发 |

## 核心能力

- **多服务商接入**：OpenAI / DeepSeek / Anthropic / 智谱 / 通义 / Ollama 等，BYOK 模式，每商多 Key 轮询
- **角色库**：系统提示词 + 模型参数 + 技能集 + 工具集 + 知识库绑定，可复用、可导入导出
- **技能库**：提示词指令 + 工具绑定，兼容 SKILL.md 目录格式
- **知识库**：文档摄入 → 分块 → 检索 → 引用注入对话
- **记忆模块**：L0-L3 分层长期记忆，自动抽取、自动巩固
- **智能路由**：自动分辨简单对话 vs 任务编排，分类理由可见
- **容灾**：单服务商失败自动切换，冷却后恢复
- **软件控制**：一句话让 AI 替你操作本机软件（Blender 为样板）
- **OpenAI 兼容 API**：`/v1/chat/completions`、`/v1/models`

## 界面

- 左导航 + 中聊天 + 右上下文面板
- 三态主题：深色 / 温和（暖米白）/ 浅色
- 流式输出，thinking 折叠，工具进度实时可见
- 气泡操作：编辑 / 删除 / 复制 / 重新生成 / 续写

## 目录结构

```
agent-proxy/
├── build.ps1                # 一条命令：前端构建 + 类型检查 + 后端出包
├── backend/
│   ├── cmd/agenthub/        # 入口
│   ├── internal/
│   │   ├── orchestrator/     # 主循环
│   │   ├── provider/        # 服务商适配器
│   │   ├── conversation/    # 会话与消息流
│   │   ├── storage/         # SQLite / JSON 双后端
│   │   ├── server/handler/  # HTTP + SSE
│   │   ├── knowledge/        # 知识库检索
│   │   ├── memory/          # 记忆模块
│   │   ├── control/         # 软件控制
│   │   ├── envprobe/        # 环境探测
│   │   ├── brief/            # 任务简报
│   │   ├── logx/            # 全局运行日志
│   │   └── ...
│   ├── data/                # 运行时数据：SQLite DB + 日志 + 密钥（gitignore）
│   └── internal/server/dist/ # 前端产物（gitignore，构建生成，被 go:embed 打包）
├── frontend/
│   ├── public/              # 原样拷进 dist 的静态文件
│   └── src/
│       ├── views/            # Chat / Providers / Roles / Paint / Video / ...
│       ├── components/      # MessageItem / MarkdownView / ...
│       ├── stores/          # Pinia
│       └── api/             # fetch client
└── tools/                   # 联调/演练用的假服务商与脚本
```

## 开发

```bash
# 前端（改界面时用，跨域由 vite proxy 解决）
cd frontend
npm install
npm run dev      # 开发模式，/api 与 /v1 代理到 127.0.0.1:8787

# 前端构建：类型检查 + 清空 embed 目录 + 直接写进去
npm run build        # = vue-tsc --noEmit && clean:embed && vite build
npm run build:only   # 跳过类型检查，只求快（不要用它出包）
```

后端改完，回到仓库根跑 `.\build.ps1`；只动后端时用 `.\build.ps1 -SkipFrontend`，
顺手跑测试用 `.\build.ps1 -Test`。

```bash
cd backend && go test ./...   # 27 个包有测试，295 个测试函数
```

> `build.ps1` 会把 `TEMP`/`TMP` 指到仓库内的 `.tmp`：这台机器上子进程写系统
> `%TEMP%` 会被拒（esbuild 会直接报 `Access is denied`）。

> `npm run build` 先跑 `clean:embed`（`frontend/scripts/clean-embed.mjs`）再构建，
> 而不是依赖 vite 的 `emptyOutDir`：这台机器上 Node 25 的 `fs.rmSync()` 是静默
> 空操作（不报错也不删），vite 的 `emptyDir()` 正好用的它，于是 embed 目录只增不减。
> 清理脚本走 `unlinkSync`/`rmdirSync`，不碰 `rmSync`。

### 前端组件约定

Element Plus **按需引入**（`vite.config.ts` 里的 `unplugin-vue-components`）：
模板里写 `<el-xxx>` 就行，插件会 import 对应组件和它的样式，`src/components.d.ts`
由构建自动生成（已入库，别手改），`vue-tsc` 靠它校验组件 prop 类型。

两处例外要手写，**别再把 `app.use(ElementPlus)` 加回来**（那会让首屏 chunk 从
410 kB 涨回 1.22 MB）：

- 服务式 API `ElMessage` / `ElMessageBox` 的样式，在 `main.ts` 里显式 import；
- 指令 `v-loading` 需要 `app.use(ElLoading)` 显式注册。

图标仍是全量注册：模板里有 `<component :is="'CircleCheck'">` 这种按名字动态取用
的情况，按需引入漏掉一个就是特定状态下的空白图标。

首屏 JS（gzip）：`index` 391 → 134 kB，`ChatView` 279 → 114 kB；mermaid 单独懒加载。

## 安全边界

- 默认只监听 `127.0.0.1:8787`。
- **一旦把 `--addr` 改成非回环地址，就必须同时设置 `--token`**（或 `AGENTHUB_TOKEN`），
  否则进程拒绝启动。这个 API 能读会话与密钥、能操作桌面，裸露在网络上等于把本机交出去。
- 跨源请求一律 403：本机浏览器里任何别的页面都读不到这个 API。唯一的例外是无 `Origin`
  的原生客户端（curl 等），所以局域网访问真正依赖的是上面那条 token。
- `/api/health` 不需要 token（给存活探针用），只回 `status` 与版本号。

## 文档

- 设计文档 `DESIGN.md`（架构与十一原则）与 `UI-AUDIT.md` **不在本仓库内**，
  源码注释里的 `DESIGN §x` 引用指向它们。仓库内的说明只有本 README 与
  `tools/README.md`（各演练脚本的用途与步骤）。

## License

MIT
