#!/usr/bin/env node
// 清空 go:embed 目录（backend/internal/server/dist），供 npm run build 前置调用。
//
// 为什么不用 vite 的 build.emptyOutDir：
//   这台机器上 Node 25 的 fs.rmSync() 是**静默空操作** —— 不抛错、也不删。
//   (unlinkSync / rmdirSync 正常。)  vite 的 emptyDir() 正是用
//   fs.rmSync(..., {recursive:true, force:true}) 实现的，于是
//   "outDir 在项目根之外 + emptyOutDir: true" 实际什么都没清：
//   每次构建都往 embed 目录里叠加一批新的内容 hash 文件。
//   历史后果是 1658 个文件 / 141 MB 全部被 go:embed 打进 exe（167 MB）。
//
// 所以这里自己走 unlinkSync + rmdirSync，不依赖 rmSync。
// vite.config.ts 里的 emptyOutDir 仍然保留：在正常机器上它是有效的第二道保险。

import { existsSync, lstatSync, mkdirSync, readdirSync, rmdirSync, unlinkSync } from 'node:fs'
import { dirname, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const target = resolve(here, '../../backend/internal/server/dist')

// 安全阀：这个脚本递归删目录，路径一旦漂移就会清掉源码。
// 只接受以 backend/internal/server/dist 结尾的绝对路径。
const expectedSuffix = ['backend', 'internal', 'server', 'dist'].join(sep)
if (!target.endsWith(expectedSuffix)) {
  console.error(`[clean-embed] 拒绝删除意外路径：${target}`)
  process.exit(1)
}

function purge(dir) {
  for (const name of readdirSync(dir)) {
    const p = resolve(dir, name)
    if (lstatSync(p).isDirectory()) {
      purge(p)
      rmdirSync(p)
    } else {
      unlinkSync(p)
    }
  }
}

let removed = 0
if (existsSync(target)) {
  removed = countFiles(target)
  purge(target)
}
// 目录本身留下：go:embed 需要它存在，vite 会往里写。
mkdirSync(target, { recursive: true })
console.log(`[clean-embed] 已清空 ${removed} 个文件：${target}`)

function countFiles(dir) {
  let n = 0
  for (const name of readdirSync(dir)) {
    const p = resolve(dir, name)
    n += lstatSync(p).isDirectory() ? countFiles(p) : 1
  }
  return n
}
