<script setup lang="ts">
import { ref } from 'vue'
import MarkdownView from './MarkdownView.vue'
import ToolCallBlock from './ToolCallBlock.vue'
import { roleBadgeColor } from '@/stores/app'

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  thinking?: string
  tools?: { name: string; callId?: string; args?: any; output?: string; failed?: boolean; summary?: string }[]
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

function toolCount(): number {
  return props.msg.tools?.length || 0
}
function toolFailed(): boolean {
  return (props.msg.tools || []).some((t) => t.failed)
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
      <pre v-if="showThinking" class="think-body">{{ msg.thinking }}</pre>
    </div>

    <!-- Tool calls: a compact strip that expands to full blocks only when asked.
         This keeps the model's internal work from owning the reply. -->
    <div v-if="toolCount()" class="tools" @click="showTools = !showTools">
      <el-icon class="caret" :class="{ open: showTools }"><ArrowRight /></el-icon>
      <span class="tools-count">{{ toolCount() }} 个工具</span>
      <el-tag v-for="t in msg.tools" :key="t.name" size="small" :type="t.failed ? 'danger' : 'info'" effect="plain">
        {{ t.name }}
      </el-tag>
      <span v-if="toolFailed()" class="tools-fail">有失败</span>
    </div>
    <div v-if="showTools" class="tools-body">
      <ToolCallBlock
        v-for="(t, i) in msg.tools"
        :key="i"
        :name="t.name"
        :summary="t.summary"
        :output="t.output"
        :failed="t.failed"
      />
    </div>

    <div class="bubble" :class="{ failed: msg.failed }">
      <MarkdownView :content="msg.content" />
      <span v-if="msg.streaming" class="cursor" />
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
  font-size: 11px;
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
@keyframes blink {
  50% {
    opacity: 0;
  }
}
.think {
  border: 1px dashed var(--ah-border);
  border-radius: 8px;
  margin-bottom: 6px;
  font-size: 12.5px;
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
  font-size: 11px;
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
  font-size: 11.5px;
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
  font-size: 11.5px;
  color: var(--ah-text-dim);
  background: var(--ah-bg-sunken);
}
.tools:hover {
  border-color: var(--ah-primary);
}
.tools-count {
  font-weight: 500;
}
.tools-fail {
  color: var(--ah-danger);
}
.tools-body {
  margin-bottom: 6px;
}
.cites {
  margin-top: 6px;
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  align-items: center;
}
.cites-label {
  font-size: 11px;
  color: var(--ah-text-dim);
}
.cite {
  cursor: default;
}
.dim {
  margin-left: auto;
  opacity: 0.7;
}
</style>
