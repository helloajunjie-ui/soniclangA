<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import { boot, deleteConversation, roleById, store, toggleTheme } from '@/stores/app'

const route = useRoute()
const router = useRouter()

// The active conversation is whatever the URL says. Sidebar and chat page share
// this single source of truth, so switching here drives the chat view directly.
const activeConv = computed(() => (route.params.id as string) || '')

function startConversation() {
  // Landing on /chat with no id means "new": the conversation is created on the
  // first message, so an abandoned "new chat" never leaves an empty record.
  router.push('/chat')
}

function openConversation(id: string) {
  router.push(`/chat/${id}`)
}

async function removeConversation(id: string) {
  try {
    await ElMessageBox.confirm('删除这次对话？删除后无法恢复。', '确认删除', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  await deleteConversation(id)
  if (activeConv.value === id) router.push('/chat')
}

onMounted(boot)
</script>

<template>
  <div class="shell">
    <nav class="side">
      <div class="brand">
        <span class="logo">⬢</span>
        <span class="brand-name">AgentHub</span>
      </div>

      <div class="nav-scroll">
        <div class="nav-group">工作台</div>
        <RouterLink to="/chat" class="nav-item" title="聊天"><el-icon><ChatDotRound /></el-icon><span>聊天</span></RouterLink>
        <RouterLink to="/roles" class="nav-item" title="角色"><el-icon><User /></el-icon><span>角色</span></RouterLink>
        <RouterLink to="/skills" class="nav-item" title="技能"><el-icon><MagicStick /></el-icon><span>技能</span></RouterLink>
        <RouterLink to="/knowledge" class="nav-item" title="资料库"><el-icon><Collection /></el-icon><span>资料库</span></RouterLink>
        <RouterLink to="/paint" class="nav-item" title="绘画"><el-icon><Picture /></el-icon><span>绘画</span></RouterLink>
        <RouterLink to="/video" class="nav-item" title="视频"><el-icon><VideoCamera /></el-icon><span>视频</span></RouterLink>

        <div class="nav-group">配置</div>
        <RouterLink to="/providers" class="nav-item" title="服务商"><el-icon><Connection /></el-icon><span>服务商</span></RouterLink>
        <RouterLink to="/tools" class="nav-item" title="工具"><el-icon><Tools /></el-icon><span>工具</span></RouterLink>

        <div class="nav-group">系统</div>
        <RouterLink to="/settings" class="nav-item" title="设置"><el-icon><Setting /></el-icon><span>设置</span></RouterLink>
        <!-- 记忆与行为规则是助手的内务：日常用不到，入口收在设置里，避免新用户面对
             一屏看不懂的数据。需要看的时候从「设置 → 高级」进去。 -->
      </div>

      <div class="conv-block">
        <div class="conv-head">
          <span>对话记录</span>
          <el-icon class="add" title="新对话" @click="startConversation"><Plus /></el-icon>
        </div>
        <div class="conv-scroll">
          <div
            v-for="c in store.conversations"
            :key="c.id"
            class="conv"
            :class="{ active: c.id === activeConv }"
            @click="openConversation(c.id)"
          >
            <div class="conv-title">{{ c.title || '新对话' }}</div>
            <div class="conv-meta">
              <span>{{ roleById(c.roleId)?.name || '通用助手' }}</span>
              <el-icon class="del" @click.stop="removeConversation(c.id)"><Delete /></el-icon>
            </div>
          </div>
          <div v-if="!store.conversations.length" class="conv-empty">还没有对话</div>
        </div>
      </div>

      <div class="side-foot">
        <div class="nav-item clickable" @click="toggleTheme">
          <el-icon><component :is="store.theme === 'dark' ? 'Sunny' : 'Moon'" /></el-icon>
          <span>{{ store.theme === 'dark' ? '浅色' : '深色' }}</span>
        </div>
        <div class="ver">后端 {{ store.backendVersion || '…' }}</div>
      </div>
    </nav>
    <main class="main">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  height: 100vh;
  overflow: hidden;
}

.side {
  width: 216px;
  flex: none;
  background: var(--ah-bg-elev);
  border-right: 1px solid var(--ah-border);
  display: flex;
  flex-direction: column;
  padding: 14px 10px 10px;
  overflow: hidden;
}

.brand {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 10px 10px;
  flex: none;
}

.logo {
  color: var(--ah-primary);
  font-size: 18px;
}

.brand-name {
  font-weight: 650;
  letter-spacing: 0.02em;
}

.nav-scroll {
  flex: none;
  max-height: 52vh;
  overflow-y: auto;
}

.nav-group {
  font-size: 11px;
  color: var(--ah-text-dim);
  padding: 12px 10px 4px;
  letter-spacing: 0.04em;
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 10px;
  border-radius: 8px;
  color: var(--ah-text-dim);
  text-decoration: none;
  font-size: 13px;
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.nav-item:hover {
  background: var(--ah-bg-sunken);
  color: var(--ah-text);
}

.nav-item.router-link-active {
  background: var(--ah-primary-soft);
  color: var(--ah-primary);
  font-weight: 500;
}

.conv-block {
  flex: 1;
  min-height: 80px;
  display: flex;
  flex-direction: column;
  border-top: 1px solid var(--ah-border);
  margin-top: 10px;
  padding-top: 8px;
}

.conv-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 2px 10px 6px;
  font-size: 11px;
  color: var(--ah-text-dim);
  letter-spacing: 0.04em;
  flex: none;
}

.conv-head .add {
  cursor: pointer;
  font-size: 13px;
  padding: 2px;
  border-radius: 4px;
}

.conv-head .add:hover {
  color: var(--ah-primary);
  background: var(--ah-bg-sunken);
}

.conv-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}

.conv {
  padding: 6px 10px;
  border-radius: 8px;
  cursor: pointer;
  margin-bottom: 1px;
}

.conv:hover {
  background: var(--ah-bg-sunken);
}

.conv.active {
  background: var(--ah-primary-soft);
}

.conv-title {
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.conv-meta {
  display: flex;
  align-items: center;
  font-size: 11px;
  color: var(--ah-text-dim);
  margin-top: 1px;
}

.conv-meta .del {
  margin-left: auto;
  opacity: 0;
}

.conv:hover .del {
  opacity: 0.7;
}

.conv-meta .del:hover {
  opacity: 1;
  color: var(--el-color-danger);
}

.conv-empty {
  font-size: 12px;
  color: var(--ah-text-dim);
  padding: 8px 10px;
}

.side-foot {
  flex: none;
  border-top: 1px solid var(--ah-border);
  padding-top: 6px;
  margin-top: 6px;
}

.ver {
  font-size: 11px;
  color: var(--ah-text-dim);
  padding: 4px 10px 0;
}

.main {
  flex: 1;
  min-width: 0;
  height: 100vh;
  overflow: hidden;
}
</style>
