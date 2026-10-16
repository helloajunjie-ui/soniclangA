<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { modelPrefOf, refreshProviders, store } from '@/stores/app'

/**
 * 服务商 —— 给小白的第一眼就是「选厂商、粘 KEY、保存」。
 *
 * 页面只回答一件事：怎么让聊天跑起来。答完就走。
 *   · 还没配过 → 右侧一张引导卡：选厂商 + 粘 KEY + 添加，保存完就能去聊天；
 *   · 已配过   → 左侧列表选中，右侧显示启用状态 + 换 Key + 删除；
 *   · 其余一切（Base URL、协议、模型目录、多 Key 轮询、同步）都是高级能力，
 *     收进「高级」折叠里，普通用户永远不会展开。
 */

const selectedId = ref('')
const providerQuery = ref('')
const modelQuery = ref('')
const types = ref<any[]>([])
const busy = ref('')
const dialog = ref(false)
const draft = ref<any>({ name: '', type: 'openai', baseUrl: '', keys: '' })
const form = ref({ name: '', type: 'openai', baseUrl: '' })
const dirty = ref(false)
const newKey = ref('')
const newKeyLabel = ref('')
// 高级折叠：默认收起。普通用户只走「添加服务商」卡。
const advanced = ref<string[]>([])

const MODEL_PAGE = 80
const modelLimit = ref(MODEL_PAGE)

const current = computed(() => (store.providers as any[]).find((p) => p.id === selectedId.value) || null)

const shown = computed(() => {
  const kw = providerQuery.value.trim().toLowerCase()
  const all = store.providers as any[]
  if (!kw) return all
  return all.filter((p) => `${p.name || ''} ${p.type || ''} ${p.baseUrl || ''}`.toLowerCase().includes(kw))
})
const onList = computed(() => shown.value.filter((p) => p.enabled !== false))
const offList = computed(() => shown.value.filter((p) => p.enabled === false))

const addable = computed(() => {
  const used = new Set((store.providers as any[]).map((p) => p.type))
  const kw = providerQuery.value.trim().toLowerCase()
  return (types.value as any[]).filter(
    (t) =>
      t.type !== 'custom' &&
      !used.has(t.type) &&
      (!kw || `${t.name || ''} ${t.type || ''}`.toLowerCase().includes(kw)),
  )
})

const catalog = computed(() => {
  const p = current.value
  if (!p) return []
  const kw = modelQuery.value.trim().toLowerCase()
  return ((p.models || []) as any[])
    .filter((m) => !kw || `${m.id} ${m.name || ''}`.toLowerCase().includes(kw))
    .map((m) => ({ ...m, ...modelPrefOf(p, m.id) }))
})
const keptCount = computed(() => {
  const p = current.value
  if (!p) return 0
  return ((p.models || []) as any[]).filter((m) => !modelPrefOf(p, m.id).disabled).length
})
const visibleCatalog = computed(() => catalog.value.slice(0, modelLimit.value))

const endpoints = computed(() => {
  const base = (form.value.baseUrl || '').replace(/\/+$/, '')
  if (!base) return null
  return form.value.type === 'anthropic'
    ? { chat: `${base}/messages`, list: `${base}/models` }
    : { chat: `${base}/chat/completions`, list: `${base}/models` }
})

const typeTemplate = computed(() => (types.value as any[]).find((t) => t.type === form.value.type) || null)

function syncForm() {
  const p = current.value
  if (!p) return
  form.value = { name: p.name || '', type: p.type || 'openai', baseUrl: p.baseUrl || '' }
  dirty.value = false
}

watch(selectedId, () => {
  modelQuery.value = ''
  modelLimit.value = MODEL_PAGE
  syncForm()
})

watch(modelQuery, () => (modelLimit.value = MODEL_PAGE))

async function reload() {
  await refreshProviders()
  const list = store.providers as any[]
  if (!list.some((p) => p.id === selectedId.value)) selectedId.value = list[0]?.id || ''
  else syncFormIfPristine()
}

function syncFormIfPristine() {
  if (dirty.value) return
  syncForm()
}

onMounted(async () => {
  try {
    types.value = (await api.providerTypes()) || []
  } catch {
    types.value = []
  }
  try {
    await reload()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
})

async function toggleEnabled(p: any) {
  const next = p.enabled === false
  if (!next) {
    try {
      await ElMessageBox.confirm(
        `停用「${p.name}」？停用后它不再参与对话，Key 仍然保留。使用它的角色需要改选其它服务商。`,
        '停用服务商',
        { type: 'warning', confirmButtonText: '停用', cancelButtonText: '取消' },
      )
    } catch {
      return
    }
  }
  busy.value = 'toggle'
  try {
    await api.providerToggle(p.id, next)
    await reload()
    ElMessage.success(next ? '已启用' : '已停用')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
    await reload()
  } finally {
    busy.value = ''
  }
}

async function test() {
  const p = current.value
  if (!p) return
  busy.value = 'test'
  try {
    const r: any = await api.providerTest(p.id)
    if (r?.ok) {
      await reload()
      ElMessage.success('连通正常')
    } else {
      ElMessage.error('连通失败：' + (r?.error || r?.message || '未知原因'))
    }
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function syncModels() {
  const p = current.value
  if (!p) return
  busy.value = 'sync'
  try {
    const r: any = await api.providerRefreshModels(p.id)
    await reload()
    if (r?.ok === false) ElMessage.error('拉取失败：' + (r?.error || '未知原因'))
    else ElMessage.success(`已同步 ${r?.models?.length ?? 0} 个模型`)
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function saveBasics() {
  const p = current.value
  if (!p) return
  if (!form.value.name.trim()) {
    ElMessage.error('名称不能为空')
    return
  }
  busy.value = 'save'
  try {
    await api.providerUpdate(p.id, {
      ...p,
      name: form.value.name.trim(),
      type: form.value.type,
      baseUrl: form.value.baseUrl.trim(),
    })
    await reload()
    ElMessage.success('已保存')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function addKey() {
  const p = current.value
  if (!p) return
  const secret = newKey.value.trim()
  if (!secret) {
    ElMessage.error('请填写 API Key')
    return
  }
  busy.value = 'key'
  try {
    await api.providerKeysAdd(p.id, secret, newKeyLabel.value.trim())
    newKey.value = ''
    newKeyLabel.value = ''
    await reload()
    ElMessage.success('Key 已加密保存')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function removeKey(k: any) {
  const p = current.value
  if (!p) return
  busy.value = 'key'
  try {
    await api.providerKeysRemove(p.id, k.id)
    await reload()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function setModel(m: any, patch: { disabled?: boolean; favorite?: boolean }) {
  const p = current.value
  if (!p) return
  busy.value = `m:${m.id}`
  try {
    await api.providerSetModelPref(p.id, m.id, patch)
    await reload()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function setAll(disabled: boolean) {
  const p = current.value
  if (!p) return
  const total = ((p.models || []) as any[]).length
  if (disabled) {
    try {
      await ElMessageBox.confirm(
        `停用「${p.name}」的全部 ${total} 个模型？停用后它们不再出现在角色和对话的模型选择里，随时可以再启用。`,
        '全部停用',
        { type: 'warning', confirmButtonText: '全部停用', cancelButtonText: '取消' },
      )
    } catch {
      return
    }
  }
  busy.value = 'all'
  try {
    await api.providerSetAllModelPrefs(p.id, { disabled })
    await reload()
    ElMessage.success(disabled ? '已全部停用' : '已全部启用')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

function openCreate(t?: any) {
  draft.value = t
    ? { name: t.name || '', type: t.type || 'openai', baseUrl: t.defaultBaseURL || '', keys: '' }
    : { name: '', type: 'openai', baseUrl: '', keys: '' }
  dialog.value = true
}

function onDraftTypeChange(type: string) {
  const tpl = (types.value as any[]).find((t) => t.type === type)
  if (!draft.value.baseUrl?.trim() && tpl) draft.value.baseUrl = tpl.defaultBaseURL || ''
}

function restoreBaseUrl() {
  const tpl = typeTemplate.value
  if (!tpl) return
  form.value.baseUrl = tpl.defaultBaseURL || ''
  dirty.value = true
}

function onToggleModel(m: any, on: any) {
  void setModel(m, { disabled: !on })
}

async function create() {
  const d = draft.value
  if (!d.name.trim()) {
    ElMessage.error('名称不能为空')
    return
  }
  const tpl = (types.value as any[]).find((t) => t.type === d.type)
  busy.value = 'create'
  try {
    const created: any = await api.providerCreate({
      provider: {
        name: d.name.trim(),
        type: d.type,
        baseUrl: (d.baseUrl || tpl?.defaultBaseURL || '').trim(),
      },
      secrets: String(d.keys || '')
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean),
    })
    dialog.value = false
    await reload()
    if (created?.id) selectedId.value = created.id
    ElMessage.success('已添加，可以去聊天了')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}

async function remove() {
  const p = current.value
  if (!p) return
  try {
    await ElMessageBox.confirm(`删除服务商「${p.name}」？它的 Key 会一并删除，且无法恢复。`, '确认删除', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  busy.value = 'del'
  try {
    await api.providerDelete(p.id)
    selectedId.value = ''
    await reload()
    ElMessage.success('已删除')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <div class="pv">
    <!-- 左：已配置的服务商 -->
    <aside class="pv-list">
      <div class="pv-search">
        <el-input v-model="providerQuery" size="small" placeholder="搜索服务商" clearable>
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
      </div>

      <div class="pv-scroll">
        <template v-if="onList.length">
          <div class="pv-group">已启用 · {{ onList.length }}</div>
          <div
            v-for="p in onList"
            :key="p.id"
            class="pv-row"
            :class="{ active: p.id === selectedId }"
            @click="selectedId = p.id"
          >
            <span class="pv-name">{{ p.name }}</span>
            <el-switch
              size="small"
              :model-value="true"
              :loading="busy === 'toggle'"
              @click.stop
              @change="toggleEnabled(p)"
            />
          </div>
        </template>

        <template v-if="offList.length">
          <div class="pv-group">未启用 · {{ offList.length }}</div>
          <div
            v-for="p in offList"
            :key="p.id"
            class="pv-row off"
            :class="{ active: p.id === selectedId }"
            @click="selectedId = p.id"
          >
            <span class="pv-name">{{ p.name }}</span>
            <el-switch size="small" :model-value="false" @click.stop @change="toggleEnabled(p)" />
          </div>
        </template>

        <div v-if="!shown.length" class="pv-empty">还没有服务商，从右边添加一个。</div>
      </div>

      <div class="pv-foot">
        <el-button size="small" text @click="openCreate()">
          <el-icon><Plus /></el-icon>&nbsp;添加服务商
        </el-button>
      </div>
    </aside>

    <!-- 右：小白主路径 或 已配置详情 -->
    <section class="pv-detail">
      <!-- 没配过：一张引导卡，选厂商 + 粘 KEY + 添加 -->
      <template v-if="!current">
        <el-card shadow="never" class="pv-welcome">
          <h3 class="welcome-title">添加一个服务商</h3>
          <p class="welcome-sub">选一个厂商，粘上你的 API Key，点添加就能回去聊天了。</p>

          <el-form label-position="top" size="default" class="welcome-form">
            <el-form-item label="选择厂商">
              <el-select v-model="draft.type" style="width: 100%" @change="onDraftTypeChange($event)">
                <el-option v-for="t in types.filter((x) => x.type !== 'custom')" :key="t.type" :label="t.name" :value="t.type" />
              </el-select>
            </el-form-item>
            <el-form-item label="API Key">
              <el-input
                v-model="draft.keys"
                type="password"
                show-password
                placeholder="粘贴你的 Key（sk-…）"
              />
            </el-form-item>
            <el-button type="primary" size="default" :loading="busy === 'create'" @click="create">
              添加并开始使用
            </el-button>
            <p class="welcome-hint">Key 保存在本机，只有你自己能看到。</p>
          </el-form>
        </el-card>
      </template>

      <!-- 已配过：状态 + Key，其余收进高级折叠 -->
      <template v-else>
        <header class="pv-head">
          <div class="pv-title">
            <strong>{{ current.name }}</strong>
            <el-tag
              size="small"
              :type="current.enabled === false ? 'info' : 'success'"
              effect="plain"
            >
              {{ current.enabled === false ? '未启用' : '启用中' }}
            </el-tag>
          </div>
          <div class="pv-head-actions">
            <el-button size="small" :loading="busy === 'test'" @click="test">测试连通</el-button>
            <el-button size="small" text type="danger" @click="remove">删除</el-button>
          </div>
        </header>

        <!-- API 密钥：小白日常要管的就是这个 -->
        <el-card shadow="never" class="pv-card">
          <template #header><span class="card-title">API 密钥 · {{ (current.keys || []).length }}</span></template>
          <div v-if="(current.keys || []).length" class="key-list">
            <div v-for="k in current.keys || []" :key="k.id" class="key-row">
              <code>{{ k.mask || '••••' }}</code>
              <span v-if="k.label" class="dim">{{ k.label }}</span>
              <span class="dim key-state">{{ k.status === 'cooldown' ? '冷却中' : '' }}</span>
              <div style="flex: 1" />
              <el-button text size="small" type="danger" :loading="busy === 'key'" @click="removeKey(k)">移除</el-button>
            </div>
          </div>
          <p v-else class="dim">还没有 Key。填一把就能开始对话。</p>
          <div class="add-key">
            <el-input v-model="newKeyLabel" size="small" placeholder="备注（可留空）" style="width: 120px" />
            <el-input v-model="newKey" size="small" type="password" show-password placeholder="粘贴新的 Key" style="flex: 1" />
            <el-button size="small" type="primary" :loading="busy === 'key'" @click="addKey">添加</el-button>
          </div>
        </el-card>

        <!-- 高级：普通用户不展开 -->
        <el-collapse v-model="advanced" class="pv-adv">
          <el-collapse-item name="adv">
            <template #title>高级设置</template>

            <!-- 基本信息 -->
            <el-card shadow="never" class="pv-card">
              <template #header><span class="card-title">基本信息</span></template>
              <el-form label-width="86px" size="small">
                <el-form-item label="名称">
                  <el-input v-model="form.name" style="width: 260px" @input="dirty = true" />
                </el-form-item>
                <el-form-item label="协议">
                  <el-select v-model="form.type" style="width: 200px" @change="dirty = true">
                    <el-option label="OpenAI 兼容" value="openai" />
                    <el-option label="Anthropic 原生" value="anthropic" />
                  </el-select>
                </el-form-item>
                <el-form-item label="Base URL">
                  <div class="url-row">
                    <el-input v-model="form.baseUrl" style="width: 340px" @input="dirty = true" />
                    <el-button size="small" text :disabled="!typeTemplate" @click="restoreBaseUrl">恢复默认</el-button>
                  </div>
                </el-form-item>
                <el-form-item v-if="endpoints" label="实际请求">
                  <div class="derived">
                    <div class="dr"><span>对话</span><code>{{ endpoints.chat }}</code></div>
                    <div class="dr"><span>模型目录</span><code>{{ endpoints.list }}</code></div>
                    <p class="dr-hint">上面两行是系统按 Base URL 拼出来的，不用你填，只是给你核对。</p>
                  </div>
                </el-form-item>
                <el-form-item>
                  <el-button size="small" type="primary" :disabled="!dirty" :loading="busy === 'save'" @click="saveBasics">保存</el-button>
                  <el-button size="small" :disabled="!dirty" @click="syncForm">撤销</el-button>
                </el-form-item>
              </el-form>
            </el-card>

            <!-- 模型目录 -->
            <el-card shadow="never" class="pv-card">
              <template #header>
                <div class="models-head">
                  <span class="card-title">模型目录 · {{ catalog.length }}</span>
                  <span class="dim">保留 {{ keptCount }} 个</span>
                  <div style="flex: 1" />
                  <el-input v-model="modelQuery" size="small" placeholder="搜索模型" clearable style="width: 140px">
                    <template #prefix><el-icon><Search /></el-icon></template>
                  </el-input>
                  <el-button size="small" :loading="busy === 'sync'" @click="syncModels">从服务商同步</el-button>
                  <el-button size="small" :loading="busy === 'all'" @click="setAll(false)">全部启用</el-button>
                  <el-button size="small" :loading="busy === 'all'" @click="setAll(true)">全部停用</el-button>
                </div>
              </template>

              <p v-if="!(current.models || []).length" class="dim">
                还没有目录。点「测试连通」或「从服务商同步」拉一次。
              </p>
              <p v-else class="hint">
                厂商给了几十上百个，只把在用的留成启用，角色和对话的模型选择就只会列出这些。星标把常用的排最前。
              </p>

              <div v-for="m in visibleCatalog" :key="m.id" class="model-row" :class="{ off: m.disabled }">
                <span class="m-name">{{ m.name || m.id }}</span>
                <span v-if="m.name && m.name !== m.id" class="m-id">{{ m.id }}</span>
                <div style="flex: 1" />
                <el-button
                  text size="small" :class="{ pinned: m.favorite }"
                  :title="m.favorite ? '取消星标' : '标为常用'"
                  @click="setModel(m, { favorite: !m.favorite })"
                >
                  <el-icon><component :is="m.favorite ? 'StarFilled' : 'Star'" /></el-icon>
                </el-button>
                <el-switch
                  size="small" :model-value="!m.disabled"
                  :loading="busy === `m:${m.id}`"
                  @change="onToggleModel(m, $event)"
                />
              </div>

              <div v-if="catalog.length > visibleCatalog.length" class="more">
                <el-button size="small" text @click="modelLimit += MODEL_PAGE">
                  还有 {{ catalog.length - visibleCatalog.length }} 个，显示更多
                </el-button>
              </div>
              <p v-if="(current.models || []).length && !catalog.length" class="dim">没有匹配的模型。</p>
            </el-card>
          </el-collapse-item>
        </el-collapse>
      </template>
    </section>

    <!-- 自定义服务商 -->
    <el-dialog v-model="dialog" title="添加服务商" width="560px">
      <el-form label-width="86px" size="small">
        <el-form-item label="名称"><el-input v-model="draft.name" /></el-form-item>
        <el-form-item label="协议">
          <el-select v-model="draft.type" style="width: 220px" @change="onDraftTypeChange($event)">
            <el-option v-for="t in types" :key="t.type" :label="`${t.name}（${t.protocol}）`" :value="t.type" />
          </el-select>
        </el-form-item>
        <el-form-item label="Base URL"><el-input v-model="draft.baseUrl" /></el-form-item>
        <el-form-item label="API Key">
          <el-input v-model="draft.keys" type="textarea" :rows="2" placeholder="可留空，稍后再填；一把一行，多把会轮询" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button size="small" @click="dialog = false">取消</el-button>
        <el-button size="small" type="primary" :loading="busy === 'create'" @click="create">添加</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.pv {
  display: flex;
  height: 100%;
  overflow: hidden;
}

/* ---- 左栏 ---- */
.pv-list {
  width: 232px;
  flex: none;
  display: flex;
  flex-direction: column;
  border-right: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
  overflow: hidden;
}
.pv-search {
  padding: 10px 10px 6px;
  flex: none;
}
.pv-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding-bottom: 8px;
}
.pv-group {
  font-size: 11px;
  color: var(--ah-text-dim);
  padding: 10px 12px 4px;
  letter-spacing: 0.04em;
}
.pv-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 12px;
  cursor: pointer;
  border-left: 2px solid transparent;
}
.pv-row:hover {
  background: var(--ah-bg-sunken);
}
.pv-row.active {
  background: var(--ah-primary-soft);
  border-left-color: var(--ah-primary);
}
.pv-row.off .pv-name {
  opacity: 0.6;
}
.pv-name {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.pv-empty {
  font-size: 12px;
  color: var(--ah-text-dim);
  padding: 12px;
}
.pv-foot {
  flex: none;
  border-top: 1px solid var(--ah-border);
  padding: 6px;
}

/* ---- 右栏 ---- */
.pv-detail {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: 20px 24px 32px;
}
.pv-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.pv-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 15px;
}
.pv-head-actions {
  margin-left: auto;
  display: flex;
  gap: 4px;
}
.pv-card {
  margin-bottom: 14px;
  border: 1px solid var(--ah-border);
}
.card-title {
  font-size: 13px;
  font-weight: 600;
}
.pv-adv {
  margin-top: 4px;
}

/* 小白引导卡 */
.pv-welcome {
  max-width: 480px;
  margin: 8px auto 0;
  padding: 8px 6px;
}
.welcome-title {
  margin: 0 0 4px;
  font-size: 18px;
  font-weight: 600;
}
.welcome-sub {
  margin: 0 0 16px;
  color: var(--ah-text-dim);
  font-size: 13px;
}
.welcome-form .el-button {
  margin-top: 4px;
}
.welcome-hint {
  margin: 10px 0 0;
  color: var(--ah-text-dim);
  font-size: 12px;
}

.url-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.derived {
  font-size: 11.5px;
  line-height: 1.9;
}
.dr {
  display: flex;
  gap: 8px;
}
.dr span {
  color: var(--ah-text-dim);
  width: 56px;
  flex: none;
}
.dr code {
  background: var(--ah-bg-sunken);
  padding: 1px 6px;
  border-radius: 4px;
  word-break: break-all;
}
.dr-hint {
  color: var(--ah-text-dim);
  margin: 2px 0 0;
}
.key-list {
  margin-bottom: 4px;
}
.key-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 0;
  border-bottom: 1px solid var(--ah-border);
}
.key-row code {
  background: var(--ah-bg-sunken);
  padding: 2px 7px;
  border-radius: 4px;
}
.key-state {
  font-size: 11px;
}
.add-key {
  display: flex;
  gap: 8px;
  margin-top: 10px;
}
.models-head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.model-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 2px;
  border-bottom: 1px solid var(--ah-border);
  font-size: 12.5px;
}
.model-row.off .m-name,
.model-row.off .m-id {
  opacity: 0.45;
}
.m-name {
  flex: none;
  max-width: 46%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.m-id {
  font-size: 11px;
  color: var(--ah-text-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.pinned {
  color: var(--el-color-warning);
}
.more {
  padding-top: 8px;
}
.hint {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  margin: 6px 0 10px;
  line-height: 1.6;
}
.dim {
  color: var(--ah-text-dim);
  font-size: 12px;
}
</style>
