# AgentHub 桌面版

后端是 `backend/agenthub.exe`（内嵌前端，单文件）；本目录只负责托管窗口与托盘。

## ⚠️ 必须放在纯英文路径下使用

**不要**把本应用（或安装包）放在含中文、空格、日文等非 ASCII 字符的目录里，例如：

```
✗ E:\刺客\agent-proxy\desktop\dist\           ← 中文目录
✗ D:\我的软件\AgentHub\                        ← 中文目录
✗ C:\Program Files (x86)\AgentHub\            ← 空格
✓ C:\AgentHub\
✓ D:\AgentHub\
✓ C:\Users\<你>\AppData\Local\Programs\AgentHub\   （安装器默认位置，符合要求）
```

原因（实测，同一台机器、同一个文件）：

| 位置 | 结果 |
|---|---|
| `E:\刺客\agent-proxy\desktop\dist\AgentHub Setup 1.0.0.exe`（双击） | `NSIS Error: Error writing temporary file` |
| `E:\刺客\agent-proxy\desktop\dist\AgentHub 1.0.0.exe`（portable，同样 SFX） | 同上 |
| 把同一个文件拷到 `C:\AgentHubDist\setup.exe` 再运行 | 安装成功 |
| `C:\AgentHub\AgentHub.exe`（解压形态） | 正常 |

与磁盘空间（C 盘 87 GB 空闲）、TEMP 可写性（实测写入 95 MB 成功）、环境变量
（TEMP/TMP 都在）都无关，**只跟路径中是否有非 ASCII 字符有关**：NSIS 自解压桩
（SFX）无法处理非 ASCII 路径。

因此分发时：

1. 用 `AgentHub-1.0.0-win.zip`，让用户**解压到纯英文目录**再运行（推荐）；
2. 或把 `win-unpacked\` 整个目录拷到纯英文目录；
3. NSIS 安装包仅在纯 ASCII 路径下可用，不随发布提供。

## 开发运行

```powershell
npm install
Remove-Item Env:\ELECTRON_RUN_AS_NODE -ErrorAction SilentlyContinue
npx electron .
```

> 开发目录本身是中文路径（本仓库）时，`electron .` 通常还能跑，但一旦出现
> 「启动后没有窗口」「后端起不来」，先按上面的规则把产物移到纯英文目录复测，
> 再怀疑代码。

## 打包

```powershell
# 缓存与临时目录必须挪出系统目录（本机 %LOCALAPPDATA% 下的 electron 缓存不可写）
$env:TEMP='..\.tmp'; $env:TMP='..\.tmp'
$env:ELECTRON_CACHE='..\.tmp\eb-cache'; $env:ELECTRON_BUILDER_CACHE='..\.tmp\eb-cache'
npx electron-builder --win zip --publish never
```

## 排障

启动日志固定在 `%APPDATA%\AgentHub\desktop.log`，逐行记录：后端路径与是否存在、
后端 stdout/stderr、`runtime.json` 里的实际地址、窗口 `ready-to-show`、
`did-fail-load` 原因、后端退出码。窗口没出现时先看这个文件。

## 目录

- `main.js` — 单实例锁、拉起后端、读 `data/runtime.json`、托盘、退出时杀子进程
- `preload.js` — `contextIsolation` + `sandbox`，只暴露最小 API
- `build/icon.ico` — 六尺寸图标