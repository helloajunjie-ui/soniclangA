<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, ApiError, streamChat } from '@/api/client'
import { modelGroups, refreshConversations, roleBadgeColor, roleById, store } from '@/stores/app'
import MessageItem, { type ChatMessage } from '@/components/MessageItem.vue'
import ContextPanel, { type PanelSection } from '@/components/ContextPanel.vue'

const route = useRoute()
const router = useRouter()

const convId = ref<string>(route.params.id as string || '')
const messages = ref<ChatMessage[]>([])
const input = ref('')
const busy = ref(false)
const selectedModel = ref('')
const roleVars = ref<Record<string, string>>({})
// The context panel is the model's internal bookkeeping (route / tools /
// knowledge / memory hits) — not something the user needs on screen every turn.
// It starts collapsed so the conversation owns the view.
const showRight = ref(false)
const mode = ref('chat')
const permission = ref('ask')
const scrollBox = ref<HTMLElement>()
let stopStream: (() => void) | null = null

const currentRole = computed(() => roleById(convId.value ? convOf.value?.roleId || '' : ''))
const convOf = computed(() => store.conversations.find((c) => c.id === convId.value))

// Role template variables: prompt the user for whatever the role declares that
// is still missing, using the role's own missing-var policy.
const missingVars = computed(() => {
  const rl = currentRole.value
  if (!rl?.vars?.length) return []
  return rl.vars
    .filter((v: any) => v.required && !roleVars.value[v.name]?.trim() && !v.default)
    .map((v: any) => v.name)
})

const roleName = computed(() => currentRole.value?.name || '通用助手')
const roleColor = computed(() => roleBadgeColor(currentRole.value?.id || 'general'))

const sections = computed<PanelSection[]>(() => {
  const last = [...messages.value].reverse().find((m) => m.role === 'assistant')
  if (!last) return []
  const out: PanelSection[] = []
  if (last.route) {
    out.push({
      key: 'route',
      title: '路由决策',
      icon: 'Share',
      items: [
        { label: `模式：${last.route.mode || 'chat'}`, detail: `置信度 ${last.route.confidence ?? '—'}`, tone: 'info' },
      ],
    })
  }
  if (last.tools?.length) {
    out.push({
      key: 'tools',
      title: '工具调用',
      icon: 'Tools',
      items: last.tools.map((t) => ({
        label: t.name,
        detail: t.failed ? '执行失败' : (t.output || t.summary || ''),
        tone: t.failed ? ('danger' as const) : undefined,
      })),
    })
  }
  if (last.citations?.length || last.knowledgeHits?.length) {
    const cites = last.citations?.length
      ? last.citations.map((c, i) => ({
          label: `[${i + 1}] ${c.docName || c.doc}`,
          detail: (c.text || '').slice(0, 120),
          tone: 'info' as const,
        }))
      : (last.knowledgeHits || []).map((c: any, i: number) => ({
          label: `[${i + 1}] ${c.docName || c.doc || c.id}`,
          detail: String(c.text || '').slice(0, 120),
          tone: 'info' as const,
        }))
    out.push({ key: 'knowledge', title: '知识命中', icon: 'Collection', items: cites })
  }
  if (last.memoryHits?.length) {
    out.push({
      key: 'memory',
      title: '记忆命中',
      icon: 'Cpu',
      items: last.memoryHits.map((m: any) => ({
        label: `${m.level || 'L1'} · score ${(m.score ?? 0).toFixed(2)}`,
        detail: String(m.content || m.text || '').slice(0, 120),
      })),
    })
  }
  return out
})

async function scrollDown() {
  await nextTick()
  const el = scrollBox.value
  if (el) el.scrollTop = el.scrollHeight
}

async function loadConversations() {
  // The sidebar renders this list, so it reads and writes the shared store
  // rather than keeping a page-local copy.
  await refreshConversations()
}

async function loadMessages(id: string) {
  const raw = (await api.conversationMessages(id)) || []
  messages.value = raw.map((m: any) => ({
    id: m.id,
    role: m.role,
    content: m.content,
  }))
  const conv = store.conversations.find((c) => c.id === id)
  if (conv?.mode) mode.value = conv.mode
  if (conv?.permission) permission.value = conv.permission
  await scrollDown()
}

async function newConversation() {
  stopStream?.()
  try {
    const c = await api.conversationCreate('', roleVars.value, mode.value)
    convId.value = c.id
    messages.value = []
    roleVars.value = {}
    await loadConversations()
    router.replace(`/chat/${c.id}`)
  } catch (e: any) {
    handleError(e)
  }
}

// Switching mode mid-thread must persist, otherwise the next turn silently
// snaps back to the mode the conversation was created with.
async function applyMode(next: string) {
  if (!convId.value) return
  try {
    await api.conversationPatch(convId.value, { mode: next })
  } catch (e: any) {
    ElMessage.error('切换模式失败：' + String(e?.message || e))
  }
}

// Permission is per conversation: "ask" only auto-runs read-only tools, so the
// model cannot mutate anything without the user picking a higher level.
async function applyPermission(next: string) {
  if (!convId.value) return
  try {
    await api.conversationPatch(convId.value, { permission: next })
  } catch (e: any) {
    ElMessage.error('切换权限失败：' + String(e?.message || e))
  }
}

function handleError(e: any) {
  if (e instanceof ApiError && e.status === 400 && e.payload?.missing) {
    // The backend refused to create the conversation because role variables are
    // missing — surface exactly what to fill in.
    roleVars.value = roleVars.value || {}
    ElMessageBox.alert(
      `请先补充角色变量：${e.payload.missing.join('、')}`,
      '缺少必填变量',
      { confirmButtonText: '去填写' },
    )
    return
  }
  ElMessage.error(String(e?.message || e))
}

function send() {
  const text = input.value.trim()
  if (!text || busy.value) return
  if (!convId.value) {
    newConversation().then(() => {
      if (convId.value) send()
    })
    return
  }

  messages.value.push({ id: 'u' + Date.now(), role: 'user', content: text })
  input.value = ''
  const reply: ChatMessage = { id: 'a' + Date.now(), role: 'assistant', content: '', streaming: true }
  messages.value.push(reply)
  busy.value = true
  scrollDown()

  const handle = streamChat(
    convId.value,
    { message: text, model: selectedModel.value || undefined },
    (type, data) => {
      switch (type) {
        case 'delta':
        case 'message': {
          const chunk = data?.delta ?? data?.content ?? data?.text ?? ''
          if (chunk) reply.content += chunk
          break
        }
        case 'reasoning':
          reply.thinking = (reply.thinking || '') + (data?.delta ?? data?.text ?? '')
          break
        case 'route':
          reply.route = { mode: data?.mode || data?.route, confidence: data?.confidence }
          break
        case 'fallback':
          reply.fallback = `服务商切换：${data?.from?.providerId || '?'} → ${data?.to?.providerId || '?'}（${data?.reason || '容灾'}）`
          break
        case 'context':
          // Knowledge / memory hits selected for this turn.
          reply.knowledgeHits = data?.knowledge || null
          reply.memoryHits = data?.memory || null
          break
        case 'tool_call': {
          // The provider stream sends only the id + argument fragment, so the
          // name is filled in when the matching tool_result arrives.
          reply.tools = reply.tools || []
          const tc = data?.toolCall || {}
          reply.tools.push({
            name: tc.name || '调用中…',
            callId: tc.id,
            args: tc.argumentsDelta ?? tc.arguments,
          })
          break
        }
        case 'tool_result': {
          reply.tools = reply.tools || []
          // Correlate by toolCallId; fall back to the last entry that has no
          // output yet so a missing id cannot orphan the result.
          let idx = reply.tools.findIndex((t: any) => data?.toolCallId && t.callId === data.toolCallId)
          if (idx < 0) idx = reply.tools.findIndex((t: any) => !t.output)
          if (idx < 0) idx = reply.tools.length - 1
          if (idx >= 0) {
            const t = reply.tools[idx]
            t.callId = data?.toolCallId || t.callId
            t.name = data?.name || t.name
            t.output = data?.result
            t.summary = String(data?.result || '').slice(0, 60)
          }
          break
        }
        case 'done':
          reply.citations = data?.citations || undefined
          reply.streaming = false
          busy.value = false
          scrollDown()
          loadConversations()
          break
        case 'error':
          reply.failed = true
          reply.streaming = false
          reply.content = reply.content || `出错了：${data?.message || '未知错误'}`
          busy.value = false
          break
      }
    },
  )
  stopStream = handle.abort
  scrollDown()
}

// The sidebar switches conversations by pushing a new URL, so this has to
// handle both directions: opening another thread, and going back to a blank
// "new chat" (which must clear the current one or the old thread stays on
// screen while the URL claims otherwise).
watch(
  () => route.params.id,
  async (id) => {
    const next = (id as string) || ''
    if (next === convId.value) return
    stopStream?.()
    convId.value = next
    messages.value = []
    if (next) await loadMessages(next)
  },
)

onMounted(async () => {
  await loadConversations()
  if (convId.value) await loadMessages(convId.value)
})
</script>

<template>
  <div class="chat">
    <!-- center: stream -->
    <section class="center">
      <header class="topbar">
        <span class="role-badge" :style="{ background: roleColor }">{{ roleName }}</span>
        <el-select
          v-model="selectedModel"
          size="small"
          placeholder="模型（留空用角色默认）"
          style="width: 220px"
          clearable
          filterable
          :disabled="!modelGroups.length"
        >
          <el-option-group v-for="g in modelGroups" :key="g.label" :label="g.label">
            <el-option v-for="o in g.options" :key="o.value" :label="o.label" :value="o.value" />
          </el-option-group>
        </el-select>
        <el-radio-group v-model="mode" size="small" @change="applyMode">
          <el-radio-button value="chat">对话</el-radio-button>
          <el-radio-button value="auto">自动</el-radio-button>
          <el-radio-button value="task">任务</el-radio-button>
        </el-radio-group>
        <el-select v-model="permission" size="small" style="width: 126px" @change="applyPermission">
          <el-option label="权限：询问" value="ask" />
          <el-option label="权限：自动" value="auto" />
          <el-option label="权限：完全" value="full" />
        </el-select>
        <div style="flex: 1" />
        <el-tooltip content="显式停止本轮生成">
          <el-button v-if="busy" size="small" type="danger" plain @click="stopStream?.()">停止</el-button>
        </el-tooltip>
        <!-- 上下文面板是模型内务（路由/工具/知识/记忆命中），默认收起，
             需要核对这一轮发生了什么时才点开。 -->
        <el-tooltip :content="showRight ? '收起本轮内务' : '查看本轮内务'">
          <el-button size="small" text @click="showRight = !showRight">
            <el-icon><component :is="showRight ? 'DArrowRight' : 'DArrowLeft'" /></el-icon>
          </el-button>
        </el-tooltip>
      </header>

      <div ref="scrollBox" class="stream">
        <div v-if="!messages.length" class="welcome">
          <h2>开始一次对话</h2>
          <p>左侧选择历史对话，或直接输入开始新会话。</p>
          <el-alert
            v-if="!store.providers.length"
            type="warning"
            show-icon
            :closable="false"
            title="还没有配置服务商"
            description="先到「服务商」页添加 API Key，否则无法发起对话。"
            style="max-width: 460px; margin: 12px auto"
          />
        </div>

        <MessageItem
          v-for="m in messages"
          :key="m.id"
          :msg="m"
          :role-name="roleName"
          :role-color="roleColor"
        />
      </div>

      <!-- role variables -->
      <div v-if="currentRole?.vars?.length" class="vars">
        <span class="vars-label">角色变量</span>
        <el-input
          v-for="v in currentRole.vars"
          :key="v.name"
          v-model="roleVars[v.name]"
          size="small"
          :placeholder="v.description || v.name"
          :style="{ width: '170px' }"
        >
          <template #prepend>{{ v.name }}</template>
        </el-input>
        <span v-if="missingVars.length" class="missing">缺 {{ missingVars.join('、') }}</span>
      </div>

      <footer class="composer">
        <el-input
          v-model="input"
          type="textarea"
          :rows="3"
          resize="none"
          placeholder="输入消息，Enter 发送，Shift+Enter 换行"
          @keydown.enter.exact.prevent="send"
        />
        <el-button type="primary" :loading="busy" :disabled="!input.trim()" @click="send">发送</el-button>
      </footer>
    </section>

    <!-- right: context (model's internal bookkeeping, collapsed by default) -->
    <aside v-if="showRight" class="right">
      <ContextPanel :sections="sections" />
    </aside>
  </div>
</template>

<style scoped>
.chat {
  display: flex;
  height: 100vh;
  overflow: hidden;
}

.center {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  height: 100vh;
}
.topbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 16px;
  border-bottom: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
}
.stream {
  flex: 1;
  overflow: auto;
  padding: 18px 22px;
}
.welcome {
  text-align: center;
  margin-top: 12vh;
  color: var(--ah-text-dim);
}

.vars {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 16px;
  border-top: 1px solid var(--ah-border);
  flex-wrap: wrap;
}
.vars-label {
  font-size: 12px;
  color: var(--ah-text-dim);
}
.missing {
  font-size: 11.5px;
  color: var(--ah-warn);
}

.composer {
  display: flex;
  gap: 10px;
  align-items: flex-end;
  padding: 12px 16px;
  border-top: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
}

.right {
  width: 260px;
  flex: none;
  border-left: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
  overflow: auto;
}
</style>
