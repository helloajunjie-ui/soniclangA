<#
.SYNOPSIS
    一条命令构建 AgentHub：前端 → go:embed 目录 → 后端单二进制。

.DESCRIPTION
    以前这条链是手工三步（frontend 里 npm run build、把 frontend/dist 拷进
    backend/internal/server/dist、再 go build），其中"拷贝"是合并语义，
    旧的内容 hash 文件永远不会被删除。结果 embed 目录堆到 1658 个文件 / 141 MB，
    全部被打进 exe，二进制 167 MB。

    现在 vite 直接构建进 embed 目录并且每次清空，这个脚本只是把剩下的步骤
    串起来，外加两件容易踩的事：

      1. 类型检查是构建的一部分（npm run build 内含 vue-tsc --noEmit）。
         之前 build 只跑 vite，TypeScript 错误可以一路带到发布。
      2. TEMP/TMP 指向仓库内的 .tmp。这台机器上子进程写系统 %TEMP% 会被拒
         （esbuild 直接报 "remove ...Temp\esbuild-xxx: Access is denied"）。

.PARAMETER SkipFrontend
    只重编后端。前端产物已是最新时用，省二十几秒。

.PARAMETER Test
    构建前先跑 go test ./...，失败即停。

.PARAMETER Output
    exe 输出路径，默认 <repo>\backend\agenthub.exe。

.EXAMPLE
    .\build.ps1

.EXAMPLE
    .\build.ps1 -SkipFrontend -Test
#>
[CmdletBinding()]
param(
    [switch]$SkipFrontend,
    [switch]$Test,
    [string]$Output = ''
)

$ErrorActionPreference = 'Stop'

$root = $PSScriptRoot
$frontend = Join-Path $root 'frontend'
$backend = Join-Path $root 'backend'
$embedDir = Join-Path $backend 'internal/server/dist'

if (-not $Output) { $Output = Join-Path $backend 'agenthub.exe' }

function Write-Step([string]$text) {
    Write-Host ''
    Write-Host "==> $text" -ForegroundColor Cyan
}

function Invoke-Native([string]$exe, [string[]]$arguments, [string]$what) {
    & $exe @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$what 失败（exit $LASTEXITCODE）"
    }
}

# --- 环境修正：子进程无法写系统 %TEMP% --------------------------------
$tmp = Join-Path $root '.tmp'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$savedTemp = @{ TEMP = $env:TEMP; TMP = $env:TMP }
$env:TEMP = $tmp
$env:TMP = $tmp

$started = Get-Date
try {
    # --- 前端：构建 + 类型检查，直接写进 go:embed 目录 ----------------
    if (-not $SkipFrontend) {
        if (-not (Test-Path (Join-Path $frontend 'node_modules'))) {
            Write-Step '安装前端依赖（首次运行）'
            Invoke-Native 'npm.cmd' @('ci') 'npm ci'
        }

        Write-Step '构建前端（vue-tsc 类型检查 + vite build → go:embed 目录）'
        Push-Location $frontend
        try {
            Invoke-Native 'npm.cmd' @('run', 'build') 'npm run build'
        }
        finally {
            Pop-Location
        }

        if (-not (Test-Path (Join-Path $embedDir 'index.html'))) {
            throw "构建结束但 $embedDir\index.html 不存在 —— vite 的 build.outDir 配错了？"
        }
        $count = (Get-ChildItem $embedDir -Recurse -File | Measure-Object).Count
        $mb = [math]::Round(((Get-ChildItem $embedDir -Recurse -File | Measure-Object Length -Sum).Sum / 1MB), 2)
        Write-Host "    嵌入目录：$count 个文件 / $mb MB" -ForegroundColor DarkGray
    }
    else {
        Write-Step '跳过前端（-SkipFrontend）'
    }

    # --- 后端测试 ------------------------------------------------------
    if ($Test) {
        Write-Step '运行后端测试'
        Push-Location $backend
        try {
            Invoke-Native 'go' @('test', './...') 'go test ./...'
        }
        finally {
            Pop-Location
        }
    }

    # --- 后端：单二进制 ------------------------------------------------
    Write-Step "编译后端 → $Output"
    Push-Location $backend
    try {
        Invoke-Native 'go' @('build', '-o', $Output, './cmd/agenthub') 'go build'
    }
    finally {
        Pop-Location
    }

    $exeMb = [math]::Round((Get-Item -LiteralPath $Output).Length / 1MB, 1)
    $elapsed = [math]::Round(((Get-Date) - $started).TotalSeconds, 1)

    Write-Host ''
    Write-Host "完成：$Output（$exeMb MB，耗时 ${elapsed}s）" -ForegroundColor Green
    Write-Host '启动：' -NoNewline
    Write-Host ".\backend\agenthub.exe --addr 127.0.0.1:8787 --data .\backend\data" -ForegroundColor DarkGray
}
finally {
    $env:TEMP = $savedTemp.TEMP
    $env:TMP = $savedTemp.TMP
    if (Test-Path $tmp) {
        # 构建工具偶尔还攥着文件句柄，删不掉不影响结果
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
