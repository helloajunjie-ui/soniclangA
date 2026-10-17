# AgentHub 桌面版

后端是 `backend/agenthub.exe`（内嵌前端，单文件）；本目录只负责托管窗口与托盘。

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
npx electron-builder --win nsis portable zip --publish never
```

## 已知限制：安装包不能在中文路径下运行

NSIS 自解压桩（SFX）处理非 ASCII 路径会失败，报
`NSIS Error: Error writing temporary file. Make sure your temp folder is valid.`
这与磁盘空间、TEMP 是否可写无关 —— 实测同一台机器上：

| 安装包所在路径 | 结果 |
|---|---|
| `E:\刺客\agent-proxy\desktop\dist\...` | 报错退出 |
| `C:\AgentHubDist\setup.exe` | 静默安装成功（exit 0） |

规避顺序：

1. 把安装包**拷到纯 ASCII 路径**再运行（`C:\Temp\` 之类）；
2. 或直接用 `dist\win-unpacked\`，整个目录拷走即可运行，不需要安装；
3. 或分发 zip 产物（解压即用，不受路径编码影响）。

## 目录

- `main.js` — 单实例锁、拉起后端、读 `data/runtime.json` 拿实际地址、退出时杀子进程
- `preload.js` — `contextIsolation` + `sandbox`，只暴露最小 API
- `build/icon.ico` — 六尺寸图标