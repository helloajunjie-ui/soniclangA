<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { modelPrefOf, refreshProviders, store } from '@/stores/app'

/**
 * 服务商 —— master/detail, the shape the rest of the settings area takes.
 *
 * The list on the left answers "which provider"; the pane on the right answers
 * "what is it and can I use it". Before, every one of those answers was packed
 * into a card and the model catalogue was a count with a popover: the catalogue
 * is not decoration, it is where the user decides which of a vendor's several
 * hundred models exist as far as the rest of the app is concerned. That
 * decision needs a surface, so the catalogue gets a section of its own with a
 * switch per row.
 */

// Selection is local state, not a route: this is a master/detail pane, and
// putting it in the URL would reload the page on every row click.
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

// A vendor catalogue runs to hundreds of entries; rendering all of them makes
// the pane unusable and the DOM enormous. Search is the intended way through,
// so the remainder is behind a button rather than silently dropped.
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

/**
 * Preset vendors that are not configured yet, listed as rows in the same list
 * as the configured ones.
 *
 * They are the same kind of thing — a provider you could be using — so they
 * belong in the same place. Behind an "添加服务商" dialog they read as a form to
 * fill in, which is what they should instead be for: a vendor we already know
 * the address of.
 */
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

/**
 * The address the adapter will actually request.
 *
 * Base URL is the one field where a wrong guess stays invisible until the first
 * message comes back as a vendor 404, because there is nothing on screen that
 * says how it gets used. The openai adapter appends /chat/completions and
 * /models to whatever is stored; the anthropic adapter appends /messages and
 * /models. Showing the result turns "did I need the /v1?" into something the
 * user can check instead of something they learn from an error.
 */
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

/** Pull the stored values back in only when the form is not being edited — a
 *  model toggle must not discard a half-typed base URL. */
function syncFormIfPristine() {
  if (dirty.value) return
  syncForm()
}

onMounted(async () => {
  try {
    types.value = (await api.providerTypes()) || []
  } catch {
    // The preset list is a convenience; a failure here must not blank the page.
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

/**
 * The whole entity is sent back, not just the three edited fields: the update
 * endpoint takes a complete provider, so a partial body would zero the fields
 * this form does not show (custom headers, timeout, proxy). Keys, models, the
 * enable switch and the model curation are ignored server-side — those have
 * their own single writers.
 */
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

/** Switching preset type refills the address from that vendor's template, but
 *  only when the field is empty — an address already typed by hand is a
 *  decision, not a placeholder. */
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

/** el-switch hands back its own value union, so the boolean is coerced here
 *  rather than annotating a template expression. */
function onToggleModel(m: any, on: any) {
  void setModel(m, { disabled: !on })
}

/** The create endpoint takes `{provider, secrets}` — the first key can ride
 *  along, so a vendor is usable in one step instead of three. */
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
    ElMessage.success('已添加')
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
    <!-- 左：服务商 -->
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

        <template v-if="addable.length">
          <div class="pv-group">可添加</div>
          <div v-for="t in addable" :key="t.type" class="pv-row add" @click="openCreate(t)">
            <el-icon><Plus /></el-icon>
            <span class="pv-name">{{ t.name }}</span>
          </div>
        </template>

        <div v-if="!shown.length && !addable.length" class="pv-empty">没有匹配的服务商</div>
      </div>

      <div class="pv-foot">
        <el-button size="small" text @click="openCreate()">
          <el-icon><Plus /></el-icon>&nbsp;自定义服务商
        </el-button>
      </div>
    </aside>

    <!-- 右：详情 -->
    <section class="pv-detail">
      <el-empty v-if="!current" description="从左边选一个服务商，或添加一个新的" />

      <template v-else>
        <header class="pv-head">
          <div class="pv-title">
            <strong>{{ current.name }}</strong>
            <el-tag size="small" effect="plain">{{ current.type }}</el-tag>
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

        <p class="pv-note">BYOK：Key 由你自持，本机加密存储，界面只回显掩码。</p>

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
                <el-button
                  size="small"
                  text
                  :disabled="!typeTemplate"
                  @click="restoreBaseUrl"
                >
                  恢复默认
                </el-button>
              </div>
            </el-form-item>
            <!-- Derived, read-only: what the stored address turns into at request
                 time. Editable-looking duplicates of it would be worse than
                 nothing, so these are plain text. -->
            <el-form-item v-if="endpoints" label="实际请求">
              <div class="derived">
                <div class="dr"><span>对话</span><code>{{ endpoints.chat }}</code></div>
                <div class="dr"><span>模型目录</span><code>{{ endpoints.list }}</code></div>
                <p class="dr-hint">上面两行是系统按 Base URL 拼出来的，不用你填，只是给你核对。</p>
              </div>
            </el-form-item>
            <el-form-item>
              <el-button
                size="small"
                type="primary"
                :disabled="!dirty"
                :loading="busy === 'save'"
                @click="saveBasics"
              >
                保存
              </el-button>
              <el-button size="small" :disabled="!dirty" @click="syncForm">撤销</el-button>
            </el-form-item>
          </el-form>
        </el-card>

        <!-- 密钥 -->
        <el-card shadow="never" class="pv-card">
          <template #header><span class="card-title">API 密钥 · {{ (current.keys || []).length }}</span></template>
          <div v-for="k in current.keys || []" :key="k.id" class="key-row">
            <code>{{ k.mask || '••••' }}</code>
            <span class="dim">{{ k.label }}</span>
            <span class="dim key-state">{{ k.status === 'cooldown' ? '冷却中' : '' }}</span>
            <div style="flex: 1" />
            <el-button text size="small" type="danger" :loading="busy === 'key'" @click="removeKey(k)">移除</el-button>
          </div>
          <p v-if="!(current.keys || []).length" class="dim">还没有 Key。填一把就能开始对话。</p>
          <div class="add-key">
            <el-input v-model="newKeyLabel" size="small" placeholder="备注（如：主号）" style="width: 140px" />
            <el-input v-model="newKey" size="small" type="password" show-password placeholder="sk-…" style="flex: 1" />
            <el-button size="small" type="primary" :loading="busy === 'key'" @click="addKey">添加</el-button>
          </div>
          <p class="hint">多把 Key 会在这家服务商的请求之间自动轮询；失败的那把进入冷却，不会拖住其它请求。</p>
        </el-card>

        <!-- 模型目录 -->
        <el-card shadow="never" class="pv-card">
          <template #header>
            <div class="models-head">
              <span class="card-title">模型目录 · {{ catalog.length }}</span>
              <span class="dim">保留 {{ keptCount }} 个</span>
              <div style="flex: 1" />
              <el-input
                v-model="modelQuery"
                size="small"
                placeholder="搜索模型"
                clearable
                style="width: 160px"
              >
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
            这里是<strong>取舍</strong>的地方：厂商给了几十上百个，只把在用的留成启用，
            角色和对话的模型选择就只会列出这些。星标会把常用的排在选择器最前面。
          </p>

          <div v-for="m in visibleCatalog" :key="m.id" class="model-row" :class="{ off: m.disabled }">
            <span class="m-name">{{ m.name || m.id }}</span>
            <span v-if="m.name && m.name !== m.id" class="m-id">{{ m.id }}</span>
            <div style="flex: 1" />
            <el-button
              text
              size="small"
              :class="{ pinned: m.favorite }"
              :title="m.favorite ? '取消星标' : '标为常用'"
              @click="setModel(m, { favorite: !m.favorite })"
            >
              <el-icon><component :is="m.favorite ? 'StarFilled' : 'Star'" /></el-icon>
            </el-button>
            <el-switch
              size="small"
              :model-value="!m.disabled"
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
      </template>
    </section>

    <el-dialog v-model="dialog" :title="draft.type === 'custom' ? '自定义服务商' : '添加服务商'" width="560px">
      <el-form label-width="86px" size="small">
        <el-form-item label="名称"><el-input v-model="draft.name" /></el-form-item>
        <el-form-item label="协议">
          <el-select v-model="draft.type" style="width: 220px" @change="onDraftTypeChange($event)">
            <el-option v-for="t in types" :key="t.type" :label="`${t.name}（${t.protocol}）`" :value="t.type" />
          </el-select>
        </el-form-item>
        <el-form-item label="Base URL"><el-input v-model="draft.baseUrl" /></el-form-item>
        <el-form-item label="API Key">
          <el-input
            v-model="draft.keys"
            type="textarea"
            :rows="2"
            placeholder="可留空，稍后再填；一把一行，多把会轮询"
          />
        </el-form-item>
      </el-form>
      <p class="hint">
        模型目录不用手填 —— 添加后点「从服务商同步」，厂商有什么就有什么。
      </p>
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
.pv-row.add {
  color: var(--ah-text-dim);
}
.pv-row.add:hover {
  color: var(--ah-primary);
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
  padding: 16px 18px 28px;
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
.pv-note {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  margin: 8px 0 14px;
}
.pv-card {
  margin-bottom: 14px;
  border: 1px solid var(--ah-border);
}
.card-title {
  font-size: 13px;
  font-weight: 600;
}
.url-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
/* Derived values read as output, not as fields: monospace, no border, and a
   label column so it is obvious the system produced them. */
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
