<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'

const memories = ref<any[]>([])
const persona = ref('')
const scenes = ref<any[]>([])
const conflicts = ref<any[]>([])
const drill = ref<any>(null)
const drillTitle = ref('')
const search = ref('')
const consolidating = ref(false)
// 明细默认收起：原子记忆与主题是 AI 的工作内务，日常不需要盯着看，
// 但出错时要能查、能改，所以留入口而不是摆在首屏。
const showDetail = ref(false)

const validCount = computed(() => memories.value.filter((m) => !m.invalidAt && !m.supersededBy).length)

async function load() {
  try {
    memories.value = (await api.memories()) || []
    const p = await api.persona()
    persona.value = p?.markdown || p?.content || ''
    scenes.value = (await api.scenes()) || []
    conflicts.value = ((await api.conflicts()) || []).filter((c: any) => c.status === 'open')
  } catch (e: any) {
    ElMessage.error('读取记忆失败：' + String(e?.message || e))
  }
}

async function savePersona() {
  try {
    await api.personaSave(persona.value)
    ElMessage.success('画像已保存')
  } catch (e: any) {
    ElMessage.error('保存失败：' + String(e?.message || e))
  }
}

async function consolidate() {
  try {
    await ElMessageBox.confirm(
      '让 AI 重新归纳你的记忆：合并重复内容、更新主题分组和长期画像。需要调用一次模型，可能耗时较久。',
      '重新整理记忆',
      { type: 'info', confirmButtonText: '开始整理', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  consolidating.value = true
  try {
    const r = await api.consolidate()
    ElMessage.success(r?.jobId ? '已提交，稍后自动完成' : '整理完成')
    setTimeout(load, 1500)
  } catch (e: any) {
    ElMessage.error('整理失败：' + String(e?.message || e))
  } finally {
    consolidating.value = false
  }
}

async function drillDown(level: string, id: string, title: string) {
  try {
    const r = await api.drilldown(level, id)
    drill.value = r
    drillTitle.value = `${title}（溯源）`
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function resolveConflict(c: any, keep: 'a' | 'b') {
  const keepText = keep === 'a' ? c.claimA : c.claimB
  try {
    await ElMessageBox.confirm(
      `保留「${keepText}」，并把另一条记忆标记为失效？失效后不再参与回答，原始记录仍保留可溯源。`,
      '确认裁定',
      { type: 'warning', confirmButtonText: '就这样定', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  // sourceIds 与 claimA/claimB 一一对应（见巩固提示词第 3 条），所以败者可以推断；
  // 数量对不上就不猜——宁可不失效，也不能停用错的那条记忆。
  const ids: string[] = c.sourceIds || []
  const loser = ids.length === 2 ? (keep === 'a' ? ids[1] : ids[0]) : ''
  try {
    await api.conflictResolve(c.id, keep === 'a' ? 'keep_a' : 'keep_b', loser)
    await load()
    ElMessage.success(
      loser
        ? '已裁定，另一条已标记失效'
        : '已记下你的判断；这次没能定位到要失效的记忆，请在下方明细里手动标记失效',
    )
  } catch (e: any) {
    ElMessage.error('裁定失败：' + String(e?.message || e))
  }
}

async function invalidate(m: any) {
  const text = String(m.content || '').slice(0, 40)
  try {
    await ElMessageBox.confirm(
      `将「${text}${text.length >= 40 ? '…' : ''}」标记为失效？此后不再参与回答，原始记录仍保留可溯源。`,
      '标记失效',
      { type: 'warning', confirmButtonText: '标记失效', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await api.memoryInvalidate(m.id)
    await load()
    ElMessage.success('已标记失效')
  } catch (e: any) {
    ElMessage.error('标记失效失败：' + String(e?.message || e))
  }
}

const filtered = () => {
  const kw = search.value.trim().toLowerCase()
  if (!kw) return memories.value
  return memories.value.filter((m) => (m.content || '').toLowerCase().includes(kw))
}

function flatten(node: any): any[] {
  if (!node) return []
  if (Array.isArray(node)) return node.flatMap(flatten)
  const out = [node]
  for (const k of ['children', 'items', 'memories', 'scenes', 'l1', 'l0']) {
    if (node[k]) out.push(...flatten(node[k]))
  }
  return out
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">记忆</h2>
        <p class="page-sub">助手会记住你的偏好和事实，供后续对话参考。记错了在这里改掉或标记失效即可。</p>
      </div>
    </div>

    <!-- 概览：先给规模感，而不是先给数据 -->
    <div class="overview">
      <div class="ov">
        <b>{{ validCount }}</b>
        <span>条记忆</span>
      </div>
      <div class="ov">
        <b>{{ scenes.length }}</b>
        <span>个主题</span>
      </div>
      <div class="ov" :class="{ warn: conflicts.length > 0 }">
        <b>{{ conflicts.length }}</b>
        <span>条待你确认</span>
      </div>
    </div>

    <!-- 唯一必须由人处理的队列：置顶 -->
    <div v-if="conflicts.length" class="block warn-block">
      <div class="block-head">
        <strong>有 {{ conflicts.length }} 处记忆互相矛盾</strong>
        <span class="dim">系统不会替你选，需要你确认哪个是对的（防止记忆被错误内容覆盖）</span>
      </div>
      <el-card v-for="c in conflicts" :key="c.id" class="conflict" shadow="never">
        <div class="cf">
          <div class="side">
            <div class="tag">说法一</div>
            <div>{{ c.claimA }}</div>
          </div>
          <div class="side">
            <div class="tag">说法二</div>
            <div>{{ c.claimB }}</div>
          </div>
        </div>
        <div v-if="c.conflict" class="cf-note">冲突点：{{ c.conflict }}</div>
        <div class="cf-actions">
          <div style="flex: 1" />
          <el-button size="small" @click="resolveConflict(c, 'a')">保留说法一</el-button>
          <el-button size="small" type="primary" plain @click="resolveConflict(c, 'b')">保留说法二</el-button>
        </div>
      </el-card>
    </div>

    <!-- 长期画像：用户主动教育 AI 的入口，保留可编辑 -->
    <div class="block">
      <div class="block-head">
        <strong>长期画像</strong>
        <span class="dim">AI 对你的长期理解，每轮对话都会作为背景参考。可以直接改，也可以留空让它自己积累。</span>
      </div>
      <el-input v-model="persona" type="textarea" :rows="10" placeholder="还没有画像内容。可以写一段关于你自己的说明，例如职业、常用技术栈、沟通偏好。" />
      <div class="block-foot">
        <el-button size="small" type="primary" @click="savePersona">保存画像</el-button>
      </div>
    </div>

    <!-- 明细：默认收起 -->
    <div class="detail-wrap">
      <div class="detail-head" @click="showDetail = !showDetail">
        <el-icon class="caret" :class="{ open: showDetail }"><ArrowRight /></el-icon>
        <span>记忆明细（{{ validCount }} 条记忆 · {{ scenes.length }} 个主题）</span>
        <span class="dim">AI 实际记住了哪些内容，可逐条查看、溯源或标记失效</span>
      </div>

      <div v-if="showDetail" class="detail-body">
        <div class="section-title">原子记忆</div>
        <el-input v-model="search" size="small" placeholder="搜索记忆内容" clearable style="width: 240px; margin-bottom: 10px" />
        <el-table :data="filtered()" size="small">
          <el-table-column prop="content" label="内容" />
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag v-if="row.invalidAt" size="small" type="info" effect="plain">已失效</el-tag>
              <el-tag v-else-if="row.supersededBy" size="small" type="warning" effect="plain">已被替代</el-tag>
              <el-tag v-else size="small" type="success" effect="plain">有效</el-tag>
            </template>
          </el-table-column>
          <el-table-column width="130">
            <template #default="{ row }">
              <el-button text size="small" @click="drillDown('l1', row.id, row.content)">溯源</el-button>
              <el-button v-if="!row.invalidAt" text size="small" type="danger" @click="invalidate(row)">失效</el-button>
            </template>
          </el-table-column>
        </el-table>

        <div class="section-title" style="margin-top: 16px">主题分组</div>
        <el-empty v-if="!scenes.length" description="还没有形成主题" :image-size="60" />
        <div v-for="s in scenes" :key="s.id" class="scene">
          <div class="scene-head" @click="drillDown('l2', s.id, s.title || s.name)">
            <strong>{{ s.title || s.name }}</strong>
            <span class="dim">{{ s.memberIds?.length || 0 }} 条相关内容 · 点击溯源</span>
          </div>
          <p class="scene-body">{{ s.summary || s.content }}</p>
        </div>

        <div class="detail-foot">
          <el-button size="small" :loading="consolidating" @click="consolidate">重新整理记忆</el-button>
          <span class="dim">合并重复内容、更新主题分组与长期画像。一般无须手动触发。</span>
        </div>
      </div>
    </div>

    <el-dialog v-model="drill" :title="drillTitle" width="640px" @close="drill = null">
      <div class="drill">
        <div v-for="(n, i) in flatten(drill)" :key="i" class="drill-node">
          <div class="dn-type">{{ n.level || n.kind || 'node' }}</div>
          <div class="dn-text">{{ n.content || n.text || n.summary || JSON.stringify(n) }}</div>
        </div>
      </div>
    </el-dialog>
  </div>
</template>

<style scoped>
.overview {
  display: flex;
  gap: 10px;
  margin-bottom: 16px;
}
.ov {
  flex: 1;
  border: 1px solid var(--ah-border);
  border-radius: 10px;
  padding: 10px 14px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.ov b {
  font-size: 20px;
  font-weight: 600;
}
.ov span {
  font-size: 11.5px;
  color: var(--ah-text-dim);
}
.ov.warn {
  border-color: var(--el-color-warning);
}
.ov.warn b {
  color: var(--el-color-warning);
}
.block {
  border: 1px solid var(--ah-border);
  border-radius: 10px;
  padding: 12px 14px;
  margin-bottom: 14px;
}
.warn-block {
  border-color: var(--el-color-warning);
}
.block-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 10px;
}
.block-head strong {
  font-size: 13.5px;
}
.block-foot {
  margin-top: 10px;
}
.cf-note {
  margin-top: 8px;
  font-size: 12px;
  color: var(--ah-text-dim);
  line-height: 1.5;
}
.conflict {
  margin-bottom: 8px;
}
.conflict:last-child {
  margin-bottom: 0;
}
.cf {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
  font-size: 12.5px;
}
.tag {
  font-size: 11px;
  color: var(--ah-text-dim);
  margin-bottom: 3px;
}
.cf-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
}
.detail-wrap {
  border-top: 1px solid var(--ah-border);
  padding-top: 12px;
}
.detail-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  cursor: pointer;
  user-select: none;
}
.detail-head .caret {
  transition: transform 0.15s;
  font-size: 12px;
}
.detail-head .caret.open {
  transform: rotate(90deg);
}
.detail-body {
  margin-top: 12px;
}
.detail-foot {
  margin-top: 16px;
  display: flex;
  align-items: center;
  gap: 10px;
}
.scene {
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 8px;
  cursor: pointer;
}
.scene:hover {
  background: var(--ah-bg-sunken);
}
.scene-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.scene-body {
  margin: 6px 0 0;
  color: var(--ah-text-dim);
  font-size: 12.5px;
}
.dim {
  color: var(--ah-text-dim);
  font-size: 11.5px;
}
.drill-node {
  border-left: 2px solid var(--ah-primary);
  padding: 4px 0 4px 10px;
  margin-bottom: 8px;
}
.dn-type {
  font-size: 11px;
  color: var(--ah-text-dim);
}
.dn-text {
  font-size: 12.5px;
  white-space: pre-wrap;
}
</style>

