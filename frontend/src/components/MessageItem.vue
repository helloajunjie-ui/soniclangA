<script setup lang="ts">
import { ref } from 'vue'
import MarkdownView from './MarkdownView.vue'
import ToolCallBlock from './ToolCallBlock.vue'
import { roleBadgeColor } from '@/stores/app'

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  time?: string
  thinking?: string
  tools?: { name: string; callId?: string; args?: any; output?: string; failed?: boolean; summary?: string; startAt?: number; duration?: number }[]
  citations?: { doc?: string; docName?: string; chunk?: number; text?: string }[]
  route?: { mode?: string; confidence?: number }
  fallback?: string
  knowledgeHits?: any
  memoryHits?: any
  streaming?: boolean
  failed?: boolean
}

const props = defineProps<{
  msg: ChatMessage
  roleName?: string
  roleColor?: string
}>()

// Thinking is collapsed by default — it is long and rarely the answer.
const showThinking = defineModel<boolean>('showThinking', { default: false })
// Tool calls are internal bookkeeping; collapsed to a compact strip by default.
const showTools = ref(false)
const copied = ref(false)

function toolCount(): number {
  return props.msg.tools?.length || 0
}
function toolFailed(): boolean {
  return (props.msg.tools || []).some((t) => t.failed)
}
// How many tools have finished (have an output or failed).
function toolDone(): number {
  return (props.msg.tools || []).filter((t) => t.output || t.failed).length
}
function toolRunning(): boolean {
  return (props.msg.tools || []).some((t) => !t.output && !t.failed)
}
// Current AI stage while streaming — gives the user a live sense of what the
// model is doing instead of an opaque "typing" indicator.
function currentStage(): string {
  if (!props.msg.streaming) return ''
  if (toolRunning()) return '正在调用工具'
  if (props.msg.thinking) return '思考中'
  if (props.msg.content) return '生成回答'
  return '等待模型响应'
}
// Format a duration in seconds for display.
function fmtDur(s?: number): string {
  if (s == null) return ''
  return s < 1 ? `${Math.round(s * 1000)}ms` : `${s}s`
}
async function copyContent() {
  const text = props.msg.content || ''
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // Clipboard API can be blocked on some contexts; fall back to select copy.
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
  }
  copied.value = true
  setTimeout(() => (copied.value = false), 1200)
}
</script>

<template>
  <div class="msg" :class="msg.role">
    <div v-if="msg.role === 'assistant' && roleName" class="who">
      <span class="role-badge" :style="{ background: roleColor || roleBadgeColor('general') }">{{ roleName }}</span>
      <span v-if="msg.route?.mode" class="route">{{ msg.route.mode }}</span>
    </div>

    <div v-if="msg.fallback" class="fallback">
      <el-icon><RefreshRight /></el-icon>{{ msg.fallback }}
    </div>

    <div v-if="msg.thinking" class="think">
      <div class="think-head" @click="showThinking = !showThinking">
        <el-icon class="caret" :class="{ open: showThinking }"><ArrowRight /></el-icon>
        <el-icon><MagicStick /></el-icon>
        <span>思考过程</span>
        <span class="dim">{{ msg.thinking.length }} 字</span>
      </div>
      <pre v-if="showThinking || msg.streaming" class="think-body">{{ msg.thinking }}</pre>
    </div>

    <!-- Tool calls: compact strip, expands on click — and auto-expands during
         streaming so the user always sees what the model is doing. -->
    <div v-if="toolCount()" class="tools" @click="showTools = !showTools">
      <el-icon class="caret" :class="{ open: showTools }"><ArrowRight /></el-icon>
      <span class="tools-count">{{ toolCount() }} 个工具</span>
      <!-- Live progress while the task runs: done/total + current stage. -->
      <span class="tools-progress" :class="{ running: toolRunning() }">
        <template v-if="toolRunning()">进行中 {{ toolDone() }}/{{ toolCount() }}</template>
        <template v-else>已完成 {{ toolDone() }}/{{ toolCount() }}</template>
      </span>
      <el-tag v-for="t in msg.tools" :key="t.name" size="small" :type="t.failed ? 'danger' : 'info'" effect="plain">
        {{ t.name }}
      </el-tag>
      <span v-if="toolFailed()" class="tools-fail">有失败</span>
    </div>
    <div v-if="showTools || msg.streaming" class="tools-body">
      <div v-for="(t, i) in msg.tools" :key="i" class="tool-line">
        <el-icon class="tl-icon" :class="t.failed ? 'tl-err' : ''">
          <component :is="t.failed ? 'CircleClose' : t.output ? 'CircleCheck' : 'Loading'" :class="t.failed || t.output ? '' : 'is-loading'" />
        </el-icon>
        <span class="tl-name">{{ t.name }}</span>
        <span v-if="!t.output && !t.failed" class="tl-run">运行中…</span>
        <span v-else-if="t.failed" class="tl-fail">失败</span>
        <span v-else class="tl-ok">完成</span>
        <span v-if="t.duration" class="tl-dur">{{ fmtDur(t.duration) }}</span>
        <span v-if="t.summary" class="tl-summary">{{ t.summary }}</span>
      </div>
    </div>

    <!-- Live stage badge while streaming: tells the user what the model is
         doing right now (thinking / tool / writing). -->
    <span v-if="msg.streaming && currentStage()" class="stage-badge">
      <el-icon class="is-loading"><Loading /></el-icon>{{ currentStage() }}
    </span>

    <div class="bubble" :class="{ failed: msg.failed }">
      <MarkdownView v-if="msg.content" :content="msg.content" />
      <span v-else-if="msg.streaming" class="thinking-hint">
        <el-icon class="is-loading"><Loading /></el-icon>
        <span v-if="msg.tools?.length">正在调用工具…</span>
        <span v-else>正在思考…</span>
      </span>
      <span v-if="msg.streaming && msg.content" class="cursor" />
      <div class="bubble-foot">
        <button v-if="msg.content" class="copy-btn" @click.stop="copyContent">
          <el-icon :size="12"><component :is="copied ? 'Check' : 'CopyDocument'" /></el-icon>
          <span>{{ copied ? '已复制' : '复制' }}</span>
        </button>
        <span v-if="msg.time" class="time">{{ msg.time }}</span>
      </div>
    </div>

    <div v-if="msg.citations?.length" class="cites">
      <span class="cites-label">引用</span>
      <el-tag
        v-for="(c, i) in msg.citations"
        :key="i"
        size="small"
        type="info"
        effect="plain"
        class="cite"
      >{{ c.docName || c.doc }}#{{ c.chunk }}</el-tag>
    </div>
  </div>
</template>

<style scoped>
.msg {
  margin-bottom: 18px;
}
.msg.user .bubble {
  background: var(--ah-primary-soft);
  border-color: transparent;
  margin-left: auto;
  max-width: 78%;
  width: fit-content;
}
.who {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.route {
  font-size: 12px;
  color: var(--ah-text-dim);
  border: 1px solid var(--ah-border);
  padding: 0 6px;
  border-radius: 4px;
}
.bubble {
  background: var(--ah-bg-elev);
  border: 1px solid var(--ah-border);
  border-radius: 12px;
  padding: 10px 14px;
  display: inline-block;
  min-width: 120px;
}
.bubble.failed {
  border-color: var(--ah-danger);
}
.cursor {
  display: inline-block;
  width: 7px;
  height: 14px;
  background: var(--ah-primary);
  vertical-align: text-bottom;
  animation: blink 1s steps(2) infinite;
}
.thinking-hint {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--ah-text-dim);
  font-size: 12.5px;
}
@keyframes blink {
  50% {
    opacity: 0;
  }
}
.think {
  border: 1px dashed var(--ah-border);
  border-radius: 8px;
  margin-bottom: 6px;
  font-size: 13.5px;
}
.think-head {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 10px;
  cursor: pointer;
  color: var(--ah-text-dim);
}
.caret {
  transition: transform 0.15s;
  font-size: 12px;
}
.caret.open {
  transform: rotate(90deg);
}
.think-body {
  margin: 0;
  padding: 8px 12px;
  border-top: 1px dashed var(--ah-border);
  white-space: pre-wrap;
  color: var(--ah-text-dim);
  max-height: 320px;
  overflow: auto;
}
.fallback {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12.5px;
  color: var(--ah-warn);
  border: 1px solid var(--ah-warn);
  border-radius: 6px;
  padding: 2px 8px;
  margin-bottom: 6px;
}
/* Tool strip — compact, one line, expands on click */
.tools {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 3px 8px;
  margin-bottom: 6px;
  cursor: pointer;
  font-size: 12.5px;
  color: var(--ah-text-dim);
  background: var(--ah-bg-sunken);
}
.tools:hover {
  border-color: var(--ah-primary);
}
.tools-count {
  font-weight: 500;
}
.tools-progress {
  font-size: 12px;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--ah-bg-sunken);
  border: 1px solid var(--ah-border);
  color: var(--ah-text-dim);
}
.tools-progress.running {
  color: var(--ah-primary);
  border-color: var(--ah-primary);
  animation: ah-pulse 1.4s ease-in-out infinite;
}
@keyframes ah-pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.55; }
}
.tools-fail {
  color: var(--ah-danger);
}
.tl-dur {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  background: var(--ah-bg-sunken);
  padding: 0 6px;
  border-radius: 6px;
  flex: none;
}
/* 阶段徽章：流式时显示 AI 当前行为（思考/工具/回答） */
.stage-badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: var(--ah-primary);
  background: var(--ah-primary-soft);
  border: 1px solid var(--ah-primary);
  padding: 2px 10px;
  border-radius: 999px;
  margin-bottom: 6px;
}
.tools-body {
  margin-bottom: 6px;
}
/* 每个工具一行：状态图标 + 名称 + 状态 + 摘要 */
.tool-line {
  display: flex;
  align-items: center;
  gap: 7px;
  font-size: 12.5px;
  color: var(--ah-text-dim);
  padding: 4px 8px;
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  margin-bottom: 4px;
  background: var(--ah-bg-sunken);
}
.tl-icon {
  color: var(--ah-primary);
}
.tl-icon.tl-err {
  color: var(--ah-danger);
}
.tl-name {
  font-weight: 500;
  color: var(--ah-text);
}
.tl-run {
  color: var(--ah-text-dim);
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.tl-ok {
  color: var(--ah-success);
}
.tl-fail {
  color: var(--ah-danger);
}
.tl-summary {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 240px;
  color: var(--ah-text-dim);
}
.cites {
  margin-top: 6px;
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  align-items: center;
}
.cites-label {
  font-size: 12px;
  color: var(--ah-text-dim);
}
.cite {
  cursor: default;
}
.dim {
  margin-left: auto;
  opacity: 0.7;
}
/* 气泡底部：复制按钮（左）+ 时间戳（右） */
.bubble-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: 6px;
  padding-top: 5px;
  border-top: 1px solid color-mix(in srgb, var(--ah-border) 55%, transparent);
}
.copy-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: none;
  background: transparent;
  color: var(--ah-text-dim);
  font-size: 11.5px;
  cursor: pointer;
  padding: 1px 6px;
  border-radius: 5px;
}
.copy-btn:hover {
  background: var(--ah-bg-sunken);
  color: var(--ah-primary);
}
.time {
  font-size: 11px;
  color: var(--ah-text-dim);
  opacity: 0.75;
}
</style>
