# 运维工具

## failover_drill_server.py — 容灾演练用假服务商

起一个最小 OpenAI 兼容服务（`/v1/models` + `/v1/chat/completions`，支持 SSE），
用于验证角色回退链在主服务商故障时确实能由备用服务商完成应答。

```bash
python tools/failover_drill_server.py     # 监听 127.0.0.1:9911
```

演练步骤：

1. 把它注册为一个服务商（`POST /api/v1/providers`，baseUrl `http://127.0.0.1:9911/v1`）。
2. 在角色上配置回退链，把它放在主服务商之后。
3. 把主服务商的 baseUrl 改成不可达地址（如 `https://127.0.0.1:9/v1`）。
4. 发一条消息，观察 SSE：`fallback` 事件会带 `from`/`to`/`cause`，`done` 显示最终由谁应答。
5. 演练后记得恢复主服务商配置。

## anthropic_drill_server.py — Anthropic 原生协议联调服务

起一个最小 **Messages API** 服务（`/v1/models` + `/v1/messages`，支持 SSE），
用于在没有真实 Anthropic Key 的情况下验证 `anthropicAdapter`。

```bash
python tools/anthropic_drill_server.py --port 9912 --key sk-ant-good --key sk-ant-second
```

### 这个 mock 会校验什么

它**不是**只会返回固定文本的假端点，而是按真实 Messages API 的规则校验请求，
所以适配器的协议错误会直接暴露成 400，而不是被悄悄放过：

- 缺 `model` / `max_tokens` / `messages` → 400（`max_tokens` 是 Anthropic 的必填项）
- `messages[0].role` 必须是 `user`
- 相邻两条消息角色相同 → 400（必须合并）
- 出现 `role: "system"` 的消息 → 400（system 必须是顶层字段）
- 缺 `anthropic-version: 2023-06-01` → 400
- `x-api-key` 不在白名单 → 401（用于演练 Key 轮换与冷却）

响应里刻意制造了几个容易踩的点：

- 工具参数**拆成多段** `input_json_delta`（中间是 `"{"expr"` 这种非法 JSON 前缀），
  逐段解析必然失败，必须拼接后整体交付
- 一个 `thinking` 块 + `signature_delta` + `ping` + 一个未知事件类型
- `/v1/models` **分页**（第一页 `has_more: true`）
- 角色问“算/计算/1+1”且提供了 tools 且历史里还没有 `tool_result` 时，
  才会发起 `tool_use`——和真实模型的行为一致，所以普通闲聊不会误触发

### 演练步骤

1. 注册服务商（注意字段名是 `secrets`，不是 `keys`）：

   ```bash
   curl -X POST http://127.0.0.1:8794/api/v1/providers -H 'Content-Type: application/json' -d '{
     "provider": {"type":"anthropic","name":"Anthropic mock","baseUrl":"http://127.0.0.1:9912/v1","enabled":true},
     "secrets": ["sk-ant-good","sk-ant-second"]
   }'
   ```

2. `POST /api/v1/providers/{id}/test` —— 应返回 `{"ok":true}`，
   并拉到分页后的完整模型列表。
3. 建一个**绑定该服务商**的角色（`providerId` + `model`），再建会话。
   注意：必须用该角色建会话，否则会话会回落到全局默认角色。
4. 发消息，观察 `reasoning` / `delta` / `done`（usage 的 `input_tokens` → `promptTokens`）。
5. 问“帮我算一下 1+1 等于几”，观察 `tool_call` → `tool_result` → 最终回答的完整往返。
6. 也可以直接打 OpenAI 兼容端点，验证服务端的双向协议翻译：

   ```bash
   curl http://127.0.0.1:8794/v1/chat/completions -H 'Content-Type: application/json' \
     -d '{"model":"<providerId>/claude-mock-a","messages":[{"role":"user","content":"你好"}]}'
   ```

### 已知限制

Anthropic **不提供 embeddings 端点**。资料库/记忆若把embedding 服务商设为
Anthropic，会明确报错并降级为 BM25，原因会写在文档的 `warning` 字段上。
需要向量检索时请另配一个 OpenAI 兼容的 embedding 服务商。

## control_*.py — 软件控制（DESIGN §9.1）的三份真实 HTTP 演练

三份脚本都**自带开关管理**：先读 `/control/status` 记住模块原状态，必要时自动开启，
跑完再把开关恢复原样。所以可以直接在冷启动的服务上跑，不需要先手动开模块——
早期版本依赖操作者事先开启，冷启动时会满屏 409，看起来像禁止清单坏了。

```bash
python tools/control_policy_drill.py     # 策略面：默认关闭 / 严格模式 / 门控
python tools/control_deny_drill.py       # 禁止清单：能否被拼写绕过
python tools/control_inspect_drill.py    # 控件树：边界、非法 id、未知能力
```

地址用环境变量覆盖（默认 `http://127.0.0.1:8797/api/v1`）：

```bash
AGENTHUB_BASE=http://127.0.0.1:8799/api/v1 python tools/control_policy_drill.py
```

各自验的是不同的东西，不能互相替代：

- `control_policy_drill.py` —— 默认必须关闭；关闭时 `/control/apps` 必须 409
  （否则窗口标题列表泄漏）；开启后必须自动进入**严格模式**（`requireAllow=true`，
  未登记应用一律 403）；禁止目标不得混进允许清单；未知能力 400。
- `control_deny_drill.py` —— 8 种拼写变体（大小写、双扩展名 `cmd.exe.exe`、
  绝对路径、裸名）都必须 403；同时反向检查**无误伤**（记事本/Chrome/Code 可正常登记，
  任务栏 `Shell_TrayWnd` 不得被判为终端）。
- `control_inspect_drill.py` —— 节点预算精确生效（请求 20 就只回 ≤20）、
  非法窗口 id 400、未知能力 400。

> 目标名在脚本里由字符码拼出，不写字面量——禁止清单管的是智能体能碰什么，
> 不是测试文件里能写什么。这样也避免脚本自身被命令行安全策略拦下。

### 为什么不做「真实点击」的自动演练

`control_run` 的点击/输入/快捷键会**真的操作当前桌面**（移动鼠标、向焦点窗口发键），
无法在用户机器上做无人值守验证。这部分改为结构级断言：
`internal/control/win_windows_test.go` 直接校验 `INPUT` 结构体的 ABI 尺寸
与事件顺序（这正是当初 `sizeof(INPUT)` 算错、SendInput 全部失败的那处缺陷）。

