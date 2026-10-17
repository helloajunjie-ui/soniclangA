# AgentHub 桌面版

后端是 `backend/agenthub.exe`（内嵌前端，单文件）；本目录只负责托管窗口与托盘。

## 分发方式（重要）

**不要用 NSIS 安装包。** electron-builder 的 NSIS 自解压桩（SFX）在含中文的路径下
必失败，报：

```
NSIS Error
Error writing temporary file. Make sure your temp folder is valid.
```

实测同一台机器、同一个文件：

| 位置 | 结果 |
|---|---|
| `E:\刺客\agent-proxy\desktop\dist\AgentHub Setup 1.0.0.exe`（双击） | 报错 |
| `C:\AgentHubDist\setup.exe`（同文件） | 静默安装成功 |
| `dist\win-unpacked\AgentHub.exe` | 正常（无 SFX） |
| `dist\AgentHub-1.0.0-win.zip` 解压后运行 | 正常（无 SFX） |

与磁盘空间（C 盘 87 GB 空闲）、TEMP 可写性（实测写入 95 MB 成功）、环境变量（TEMP/TMP
都在）都无关，**只跟路径里有没有非 ASCII 字符有关**。

所以交付用这两种：

1. `dist\AgentHub-1.0.0-win.zip` —— 解压即用（推荐）；
2. `dist\win-unpacked\` —— 整个目录拷走即可运行。

NSIS 安装包只有在纯 ASCII 路径下运行才有意义，不随发布提供。

## 开发运行

```powershell
npm install
Remove-Item Env:\ELECTRON_RUN_AS_NODE -ErrorAction SilentlyContinue
npx electron .
```

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