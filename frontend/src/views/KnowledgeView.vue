<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { refreshModules, store } from '@/stores/app'

const collections = ref<any[]>([])
const current = ref<any>(null)
const docs = ref<any[]>([])
const jobs = ref<any[]>([])
const newColl = ref('')
const query = ref('')
const hits = ref<any[]>([])
const uploading = ref(false)
const savingCite = ref(false)

/**
 * 资料库检索的开关属于资料库本身，所以它在这一页，而不是另开一个「能力」页。
 * 关掉是有意义的：文档全部保留，只是回答不再引用它们（比如资料还没整理好，
 * 或临时不想让它们影响回答）。
 */
const citeEnabled = computed(() => {
  const m = (store.modules || []).find((x: any) => x.id === 'rag.knowledge')
  return m ? m.enabled !== false : true
})

async function toggleCite() {
  savingCite.value = true
  try {
    await api.moduleToggle('rag.knowledge', !citeEnabled.value)
    await refreshModules()
    ElMessage.success(citeEnabled.value ? '回答时会引用资料库' : '已关闭引用，资料仍保留')
  } catch (e: any) {
    ElMessage.error('切换失败：' + String(e?.message || e))
    await refreshModules()
  } finally {
    savingCite.value = false
  }
}

async function load() {
  collections.value = (await api.collections()) || []
  if (!current.value && collections.value.length) await select(collections.value[0].id)
  jobs.value = ((await api.jobs()) || []).slice(0, 8)
  try {
    await refreshModules()
  } catch {
    // 取不到开关状态时按「开启」显示：这只是显示，不影响后端实际行为。
  }
}

async function select(id: string) {
  current.value = collections.value.find((c) => c.id === id) || null
  docs.value = (await api.documents(id)) || []
}

async function createColl() {
  if (!newColl.value.trim()) return
  try {
    const c = await api.collectionCreate({ name: newColl.value.trim(), description: '' })
    newColl.value = ''
    collections.value = (await api.collections()) || []
    await select(c.id)
    ElMessage.success('已创建')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function removeColl(c: any) {
  await ElMessageBox.confirm(`删除资料库「${c.name}」及其全部文档？`, '确认', { type: 'warning' })
  await api.collectionDelete(c.id)
  if (current.value?.id === c.id) current.value = null
  await load()
}

async function upload(file: File) {
  if (!current.value) {
    ElMessage.warning('先选择一个资料库')
    return
  }
  uploading.value = true
  try {
    await api.documentUpload(current.value.id, file)
    ElMessage.success('已提交摄取任务')
    await load()
    // Ingest is async; poll once for progress.
    setTimeout(load, 1500)
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    uploading.value = false
  }
}

async function removeDoc(d: any) {
  await ElMessageBox.confirm(`删除文档「${d.name || d.title || d.id}」？`, '确认', { type: 'warning' })
  await api.documentDelete(current.value.id, d.id)
  await load()
}

async function search() {
  if (!query.value.trim()) return
  try {
    const r = await api.knowledgeSearch(query.value, current.value ? [current.value.id] : undefined)
    hits.value = r?.citations || r?.items || r || []
    if (!Array.isArray(hits.value)) hits.value = []
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">资料库</h2>
        <p class="page-sub">你上传的文档会自动解析入库，回答时按需引用。</p>
      </div>
      <div style="display: flex; gap: 8px">
        <el-input v-model="newColl" size="small" placeholder="新资料库名称" style="width: 180px" @keyup.enter="createColl" />
        <el-button size="small" type="primary" @click="createColl">新建</el-button>
      </div>
    </div>

    <div class="cite-row">
      <div class="cite-text">
        <strong>回答时引用资料库</strong>
        <span class="dim">关掉后资料仍保留，只是回答不再引用它们。</span>
      </div>
      <el-switch :model-value="citeEnabled" :loading="savingCite" @change="toggleCite" />
    </div>

    <div class="split">
      <aside class="colls">
        <div
          v-for="c in collections"
          :key="c.id"
          class="coll"
          :class="{ active: current?.id === c.id }"
          @click="select(c.id)"
        >
          <div class="c-name">{{ c.name }}</div>
          <div class="c-meta">{{ c.docCount ?? c.documents?.length ?? 0 }} 篇 · {{ c.chunkCount ?? 0 }} 片段</div>
          <el-icon class="c-del" @click.stop="removeColl(c)"><Delete /></el-icon>
        </div>
        <div v-if="!collections.length" class="empty small">还没有资料库</div>
      </aside>

      <section class="detail">
        <template v-if="current">
          <div class="d-head">
            <strong>{{ current.name }}</strong>
            <label class="btn-file" :class="{ disabled: uploading }">
              {{ uploading ? '上传中…' : '上传文档' }}
              <input
                type="file"
                :disabled="uploading"
                @change="upload(($event.target as any).files[0])"
              />
            </label>
          </div>
          <el-table :data="docs" size="small" :show-header="true" style="width: 100%">
            <el-table-column prop="name" label="文档" />
            <el-table-column prop="chunks" label="片段" width="70" />
            <el-table-column label="索引" width="90">
              <template #default="{ row }">
                <!-- A degraded-but-ready document must not look identical to a
                     fully indexed one: BM25-only retrieval works, but semantic
                     recall is off and the operator would never know. -->
                <el-tooltip
                  v-if="row.warning"
                  :content="row.warning"
                  placement="top"
                >
                  <el-tag size="small" type="warning" effect="plain">
                    {{ row.indexStatus || 'pending' }} ·降级
                  </el-tag>
                </el-tooltip>
                <el-tag v-else size="small" :type="row.indexStatus === 'ready' ? 'success' : row.indexStatus === 'failed' ? 'danger' : 'info'" effect="plain">
                  {{ row.indexStatus || 'pending' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column width="60">
              <template #default="{ row }">
                <el-button text size="small" type="danger" @click="removeDoc(row)">删</el-button>
              </template>
            </el-table-column>
          </el-table>
        </template>
        <div v-else class="empty">选择或新建一个资料库</div>

        <details class="dev-fold"><summary>验证召回</summary><div class="section-title">检索测试</div>
        <div style="display: flex; gap: 8px">
          <el-input v-model="query" size="small" placeholder="输入问题，验证召回效果" @keyup.enter="search" />
          <el-button size="small" @click="search">检索</el-button>
        </div>
        <div v-if="hits.length" class="hits">
          <div v-for="(h, i) in hits" :key="i" class="hit">
            <div class="hit-head">
              [{{ i + 1 }}] {{ h.docName || h.doc }}#{{ h.chunk }}
              <span class="score">{{ (h.score ?? 0).toFixed(3) }}</span>
            </div>
            <div class="hit-body">{{ h.text }}</div>
          </div>
        </div>

        </details>
        <details class="dev-fold"><summary>摄取任务</summary><div class="section-title">最近任务</div>
        <div v-for="j in jobs" :key="j.id" class="job">
          <span>{{ j.type }}</span>
          <el-progress
            :percentage="j.progress ?? 0"
            :status="j.status === 'failed' ? 'exception' : j.status === 'done' ? 'success' : undefined"
            :stroke-width="6"
            style="flex: 1"
          />
          <span class="job-status">{{ j.status }}</span>
        </div>
      </details>
      </section>
    </div>
  </div>
</template>

<style scoped>
.cite-row {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 9px 12px;
  margin-bottom: 14px;
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  background: var(--ah-bg-elev);
}
.cite-text {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: baseline;
  gap: 10px;
  font-size: 13px;
}
.cite-text .dim {
  font-size: 12px;
  color: var(--ah-text-dim);
}
.split {
  display: flex;
  gap: 16px;
  align-items: flex-start;
}
.colls {
  width: 200px;
  flex: none;
}
.coll {
  padding: 8px 10px;
  border-radius: 8px;
  cursor: pointer;
  position: relative;
  margin-bottom: 2px;
}
.coll:hover {
  background: var(--ah-bg-sunken);
}
.coll.active {
  background: var(--ah-primary-soft);
}
.c-name {
  font-size: 13px;
  font-weight: 500;
}
.c-meta {
  font-size: 11px;
  color: var(--ah-text-dim);
}
.c-del {
  position: absolute;
  right: 8px;
  top: 10px;
  opacity: 0;
}
.coll:hover .c-del {
  opacity: 0.6;
}
.detail {
  flex: 1;
  min-width: 0;
}
.d-head {
  display: flex;
  align-items: center;
  margin-bottom: 10px;
  gap: 10px;
}
.hits {
  margin-top: 10px;
}
.hit {
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 8px 10px;
  margin-bottom: 6px;
  font-size: 12.5px;
}
.hit-head {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  margin-bottom: 4px;
}
.score {
  float: right;
}
.hit-body {
  white-space: pre-wrap;
  max-height: 90px;
  overflow: hidden;
}
.job {
  display: flex;
  gap: 10px;
  align-items: center;
  font-size: 12px;
  margin-bottom: 4px;
}
.job-status {
  color: var(--ah-text-dim);
  font-size: 11px;
  width: 60px;
}
.btn-file {
  border: 1px solid var(--ah-border);
  border-radius: 4px;
  padding: 4px 10px;
  font-size: 12px;
  cursor: pointer;
}
.btn-file.disabled {
  opacity: 0.6;
  pointer-events: none;
}
.btn-file input {
  display: none;
}
.empty.small {
  font-size: 12px;
  padding: 12px;
}

.dev-fold {
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 0 12px 10px;
  margin-bottom: 14px;
  background: var(--ah-bg-sunken);
}
.dev-fold summary {
  cursor: pointer;
  font-size: 12px;
  color: var(--ah-text-dim);
  padding: 8px 0;
  user-select: none;
}
.dev-fold summary:hover {
  color: var(--ah-primary);
}
.dev-fold[open] summary {
  color: var(--ah-primary);
}
</style>
