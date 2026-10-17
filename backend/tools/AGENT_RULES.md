# AGENT 工具守则（本机实操版）

> 位置：backend/tools/AGENT_RULES.md　创建：2026-10-17
> 作用：约束自造工具的行为边界，避免每次临时判断。

## 0. 一页速查

- 单文件小脚本：直接在 tools/ 下建，不用问
- 新增文件、装单个 pip 包：先做再报备
- 改源码 / 重编译 / 重启服务 / 装系统软件 / 删改已有文件 / 数据外发：停下来给方案，等确认

## 1. 三档分级

| 档 | 范围 | 动作 |
|---|---|---|
| A 直接做 | 计算、读文件、抓网页、正则搜索、格式转换、tools/ 下单文件脚本、解析 PDF/Excel、批量改名 | 执行，做完汇报 |
| B 做完报备 | 仓库内新增文件、pip install 单个纯 Python 包、起临时进程、建目录 | 先做再说明，动手前查是否已存在 |
| C 必须先问 | 1 改现有源码/重编译/重启服务；2 winget 装系统级软件；3 删或覆盖已有文件；4 数据外发、付费 API；5 直接写 agenthub.db；6 任何不可逆操作 | 停手，给方案，等选择 |

## 2. C 类事项的标准报告格式

```
问题：<一句话说清卡在哪>
方案 A：<做法> ；代价：<时间/空间> ；风险：<...> ；回滚：<...>
方案 B：...
方案 C（最省事）：...
我的推荐：X，因为 <依据>
—— 等你选，我不动手
```

## 3. 自造工具规范

1. 一律放 backend/tools/，不动 internal/、cmd/
2. 文件头写清用法，自解释
3. 幂等，不覆盖输入文件；临时文件用完立即删
4. 优先用本机已有依赖，不轻易装新包
5. 单一职责，能组合就不写巨型脚本

## 4. 已有工具登记

### tools/ai_toolbox.py（2026-10-17 建立，约 4 KB，纯标准库加已装库）

| 子命令 | 作用 |
|---|---|
| fetch url out | 抓任意大小网页存盘，突破 8KB 截断，实测 3.1MB 成功 |
| read file [--head N] | 读文本，UTF-8/GBK 自动识别 |
| write file content | 强制 UTF-8 写入；content 传 - 表示读 stdin，绕开 PowerShell 引号问题 |
| grep dir pattern [--ext .go,.py] | 等价 rg，自动跳过 .git/node_modules/data |
| doc file | 解析 pdf / docx / xlsx |

调用示例：

```powershell
C:\Python310\python.exe tools\ai_toolbox.py grep internal\mcp "func " --ext .go
C:\Python310\python.exe tools\ai_toolbox.py fetch https://go.dev/dl/ tmp.html
```

## 5. 本机环境事实（踩过的坑，勿重犯）

- 工作目录：E:\刺客\agent-proxy\backend
- 两套数据：data/ 是当前活跃库（进程无 --data 参数，config.go:104 默认 ./data）；data-m3/ 是旧数据集，别搞混
- 编码：shell 命令开头加 chcp 65001 并设 [Console]::OutputEncoding=UTF8，否则中文乱码
- 引号：python -c 在本机 PowerShell 下会被吞引号（已连挂 2 次），改用 @'...'@ 加 Set-Content，或 ai_toolbox.py write
- curl：C:\Windows\System32\curl.exe 存在但不在 PATH，需全路径
- 已装可用：python3.10（requests/bs4/lxml/pandas/openpyxl/PyPDF2/python-docx/selenium4.15）、git、node、npm、go1.27、ffmpeg
- 缺：7z、rg、jq，均有替代方案

## 6. MCP 现状

- 代码齐全：internal/mcp/ 下 client.go 与 manager.go，入口 cmd/agenthub/main.go:179，API 为 /api/v1/mcp/servers
- 配置不在 json 文件里，在文档库 documents 表的 ns=mcp_servers
- 当前活跃库 data/agenthub.db 中 0 条记录，所以现在没有任何 MCP 工具
- 旧库 data-m3 里有 2 条测试桩（test-server、demo-mcp，工具 echo/boom/nodesc），指向的 mcptestserver.exe 已被系统清理掉，且两条都是 enabled=false
- 结论：框架通，缺真 server。待加优先级：文件系统类 > 浏览器自动化类 > 数据库/文档类

## 7. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-10-17 | 建立本守则；新增 tools/ai_toolbox.py（fetch/read/write/grep/doc） |

