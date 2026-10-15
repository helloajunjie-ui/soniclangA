<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { modelsOfProvider, refreshRoles, roleBadgeColor, store } from '@/stores/app'

const loading = ref(false)
const dialog = ref(false)
const editing = ref<any>(null)
const preview = ref<{ prompt: string; missing: string[] | null; unknown: string[] | null } | null>(null)

const search = ref('')
const filterProvider = ref('')

const list = computed(() => {
  const kw = search.value.trim().toLowerCase()
  return store.roles.filter((r) => {
    if (filterProvider.value && r.providerId !== filterProvider.value) return false
    if (!kw) return true
    return (r.name + (r.description || '') + (r.systemPrompt || '')).toLowerCase().includes(kw)
  })
})

function blank() {
  return {
    id: '',
    name: '新角色',
    description: '',
    systemPrompt: '你是一名专业的助手。',
    defaultMode: 'chat',
    providerId: '',
    model: '',
    temperature: 0.7,
    maxTokens: 4096,
    topP: 1,
    tools: [] as string[],
    skills: [] as string[],
    knowledgeCollectionIds: [] as string[],
    methodologyIds: [] as string[],
    vars: [] as any[],
    missingVarPolicy: 'skip',
  }
}

/** Models of the provider this role points at — not every model of every
 *  provider. The role stores provider and model as one binding, so offering all
 *  vendors' models here lets you pair a provider with a model it does not serve,
 *  which only surfaces as a vendor error on the first chat turn. */
const modelChoices = computed(() => modelsOfProvider(editing.value?.providerId || '', editing.value?.model || ''))

function onProviderChange() {
  const kept = modelChoices.value.some((o) => o.value === editing.value.model)
  if (!kept) editing.value.model = ''
}

function edit(r: any) {
  editing.value = JSON.parse(JSON.stringify(r))
  preview.value = null
  dialog.value = true
}

function create() {
  editing.value = blank()
  preview.value = null
  dialog.value = true
}

async function save() {
  try {
    if (editing.value.id) await api.roleUpdate(editing.value.id, editing.value)
    else await api.roleCreate(editing.value)
    dialog.value = false
    await refreshRoles()
    ElMessage.success('已保存')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function duplicate(r: any) {
  try {
    await api.roleDuplicate(r.id, r.name + ' 副本')
    await refreshRoles()
    ElMessage.success('已复制')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function remove(r: any) {
  await ElMessageBox.confirm(`删除角色「${r.name}」？`, '确认', { type: 'warning' })
  await api.roleDelete(r.id)
  await refreshRoles()
}

async function exportOne(r: any) {
  const doc = await api.roleExport(r.id)
  download(`${r.name}.json`, JSON.stringify(doc, null, 2))
}

async function exportAll() {
  const doc = await api.roleExportAll()
  download('roles.json', JSON.stringify(doc, null, 2))
}

async function importFile(file: File) {
  try {
    await api.roleImport(JSON.parse(await file.text()))
    await refreshRoles()
    ElMessage.success('导入成功')
  } catch (e: any) {
    ElMessage.error('导入失败：' + String(e?.message || e))
  }
}

function download(name: string, text: string) {
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
  a.download = name
  a.click()
  URL.revokeObjectURL(a.href)
}

async function renderPreview() {
  if (!editing.value.id) {
    ElMessage.info('先保存角色，再预览渲染结果')
    return
  }
  try {
    preview.value = await api.roleRender(editing.value.id, editing.value.__vars || {})
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

onMounted(refreshRoles)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">角色库</h2>
        <p class="page-sub">定义、复用与分发角色；模板变量会在每轮渲染进系统提示词</p>
      </div>
      <div style="display: flex; gap: 8px">
        <el-input v-model="search" size="small" placeholder="搜索角色" clearable style="width: 180px" />
        <el-select v-model="filterProvider" size="small" placeholder="按服务商" clearable style="width: 150px">
          <el-option v-for="p in store.providers" :key="p.id" :label="p.name" :value="p.id" />
        </el-select>
        <el-button size="small" @click="exportAll">导出全部</el-button>
        <label class="btn-file">
          导入
          <input type="file" accept="application/json" @change="importFile(($event.target as any).files[0])" />
        </label>
        <el-button size="small" type="primary" @click="create">新建角色</el-button>
      </div>
    </div>

    <el-empty v-if="!list.length" description="没有匹配的角色" />
    <div v-else class="card-grid">
      <el-card v-for="r in list" :key="r.id" shadow="hover" class="role-card">
        <div class="rc-head">
          <span class="role-badge" :style="{ background: roleBadgeColor(r.id) }">{{ r.name }}</span>
          <el-tag v-if="r.builtin" size="small" type="info" effect="plain">内置</el-tag>
          <el-tag v-if="r.defaultMode === 'task'" size="small" effect="plain">任务</el-tag>
        </div>
        <div class="rc-desc">{{ r.description || '（无描述）' }}</div>
        <div class="rc-meta">
          <span>{{ r.providerId ? r.providerId + '/' + r.model : '未指定模型' }}</span>
          <span v-if="r.skills?.length">· {{ r.skills.length }} 技能</span>
          <span v-if="r.vars?.length">· {{ r.vars.length }} 变量</span>
        </div>
        <div class="rc-actions">
          <el-button size="small" text @click="edit(r)">编辑</el-button>
          <el-button size="small" text @click="duplicate(r)">复制</el-button>
          <el-button size="small" text @click="exportOne(r)">导出</el-button>
          <el-button v-if="!r.builtin" size="small" text type="danger" @click="remove(r)">删除</el-button>
        </div>
      </el-card>
    </div>

    <el-dialog v-model="dialog" :title="editing?.id ? '编辑角色' : '新建角色'" width="720px" top="5vh">
      <el-form v-if="editing" label-width="86px" size="small">
        <el-form-item label="名称">
          <el-input v-model="editing.name" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="editing.description" />
        </el-form-item>
        <el-form-item label="系统提示词">
          <el-input v-model="editing.systemPrompt" type="textarea" :rows="6" />
        </el-form-item>
        <el-form-item label="模板变量">
          <div style="width: 100%">
            <div v-for="(v, i) in editing.vars" :key="i" class="var-row">
              <el-input v-model="v.name" placeholder="变量名" style="width: 130px" />
              <el-input v-model="v.description" placeholder="说明" style="width: 160px" />
              <el-input v-model="v.default" placeholder="默认值" style="width: 130px" />
              <el-checkbox v-model="v.required">必填</el-checkbox>
              <el-button text type="danger" @click="editing.vars.splice(i, 1)">删</el-button>
            </div>
            <el-button size="small" text @click="editing.vars.push({ name: '', description: '', required: false, default: '' })">
              + 添加变量
            </el-button>
          </div>
        </el-form-item>
        <el-form-item label="缺失策略">
          <el-select v-model="editing.missingVarPolicy" style="width: 180px">
            <el-option label="skip · 保留占位符" value="skip" />
            <el-option label="ask · 追问补齐" value="ask" />
            <el-option label="error · 直接报错" value="error" />
          </el-select>
        </el-form-item>
        <el-form-item label="服务商">
          <el-select v-model="editing.providerId" clearable style="width: 180px" @change="onProviderChange">
            <el-option
              v-for="p in store.providers"
              :key="p.id"
              :label="p.enabled === false ? p.name + '（已停用）' : p.name"
              :value="p.id"
              :disabled="p.enabled === false"
            />
          </el-select>
        </el-form-item>
        <el-form-item label="模型">
          <el-select
            v-model="editing.model"
            clearable
            filterable
            :disabled="!editing.providerId"
            :placeholder="editing.providerId ? '在该服务商的模型中选择' : '请先选服务商'"
            style="width: 260px"
          >
            <el-option v-for="o in modelChoices" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
          <span v-if="editing.providerId && !modelChoices.length" class="field-hint">
            该服务商还没有模型目录，去服务商页「刷新」
          </span>
        </el-form-item>
        <el-form-item label="绑定技能">
          <el-select v-model="editing.skills" multiple style="width: 100%">
            <el-option v-for="s in store.skills" :key="s.id" :label="s.name" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="温度">
          <el-slider v-model="editing.temperature" :min="0" :max="2" :step="0.1" style="width: 200px" />
        </el-form-item>
      </el-form>

      <el-collapse v-if="preview">
        <el-collapse-item title="渲染预览">
          <pre class="preview">{{ preview.prompt }}</pre>
        </el-collapse-item>
      </el-collapse>

      <template #footer>
        <el-button size="small" @click="renderPreview">渲染预览</el-button>
        <el-button size="small" @click="dialog = false">取消</el-button>
        <el-button size="small" type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.role-card :deep(.el-card__body) {
  padding: 14px;
}
.rc-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.rc-desc {
  margin: 8px 0 6px;
  color: var(--ah-text-dim);
  font-size: 12.5px;
  min-height: 34px;
}
.rc-meta {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
.rc-actions {
  margin-top: 10px;
  display: flex;
  gap: 2px;
  border-top: 1px solid var(--ah-border);
  padding-top: 8px;
}
.field-hint {
  margin-left: 8px;
  font-size: 11.5px;
  color: var(--ah-text-dim);
}
.var-row {
  display: flex;
  gap: 6px;
  margin-bottom: 6px;
  align-items: center;
}
.preview {
  white-space: pre-wrap;
  font-size: 12px;
  max-height: 260px;
  overflow: auto;
  margin: 0;
}
.btn-file {
  border: 1px solid var(--ah-border);
  border-radius: 4px;
  padding: 5px 12px;
  font-size: 12px;
  cursor: pointer;
  background: var(--ah-bg-elev);
}
.btn-file:hover {
  color: var(--ah-primary);
  border-color: var(--ah-primary);
}
.btn-file input {
  display: none;
}
</style>
