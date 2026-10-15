<script setup lang="ts">
/**
 * Software control (DESIGN §9.1/§9.3): the agent does the work inside a local
 * application for a user who cannot use that application themselves.
 *
 * The page follows the flow rather than the machinery. The user says what they
 * want; the agent finds out whether this machine can do it, and if something is
 * missing it says so and asks; while that is being sorted out it keeps talking
 * and writes the ideas down; then work starts. So the page is: probe the
 * machine, keep the notes, and — folded away at the bottom — the manual console
 * and the deny list for when a human wants to drive something by hand.
 *
 * Two things it does not have. It does not ask anyone to register applications
 * before they can be helped (that is a form, not a capability), and it does not
 * explain how any of it works underneath — which route drives a program is the
 * product's business, not the user's (DESIGN §17 原则十).
 */
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'

const status = ref<any>(null)
const apps = ref<any[]>([])
const deny = ref<any[]>([])
const tree = ref<any>(null)
const selected = ref<any>(null)
const selectedNode = ref<string>('')
const loading = ref(false)
const busy = ref(false)

const enabled = computed(() => !!status.value?.enabled)
const supported = computed(() => !!status.value?.supported)

// ---- environment probe ----
const probeQuery = ref('')
const probing = ref(false)
const report = ref<any>(null)

async function runProbe() {
  const q = probeQuery.value.trim()
  if (!q) {
    ElMessage.warning('先写一句你想做什么')
    return
  }
  probing.value = true
  try {
    report.value = await api.envProbe(q)
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    probing.value = false
  }
}

// ---- briefs ----
const briefs = ref<any[]>([])
const newBrief = ref('')
const openBrief = ref<any>(null)
// Separate from openBrief: the dialog wants a boolean, the panel wants the
// note, and a null note with an open dialog would render an empty sheet.
const briefOpen = ref(false)

async function loadBriefs() {
  try {
    briefs.value = await api.briefList()
  } catch {
    briefs.value = []
  }
}

async function createBrief() {
  const title = newBrief.value.trim()
  if (!title) return
  try {
    const b: any = await api.briefCreate(title)
    newBrief.value = ''
    await loadBriefs()
    await viewBrief(b.id)
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function viewBrief(id: string) {
  try {
    openBrief.value = await api.briefGet(id)
    briefOpen.value = true
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

// ---- module switch ----
async function load() {
  loading.value = true
  try {
    status.value = await api.controlStatus()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    loading.value = false
  }
}

async function toggleModule(v: boolean | string | number) {
  loading.value = true
  try {
    await api.moduleToggle('control.desktop', !!v)
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    // Re-read the authoritative state: the service may refuse to enable on an
    // unsupported platform, and the switch must reflect reality.
    await load()
  }
}

async function loadDeny() {
  try {
    deny.value = await api.controlDenyList()
  } catch {
    deny.value = []
  }
}

async function loadApps() {
  busy.value = true
  try {
    apps.value = await api.controlApps()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = false
  }
}

async function inspect(app: any) {
  if (!app) return
  selected.value = app
  selectedNode.value = ''
  busy.value = true
  try {
    tree.value = await api.controlInspect(app.id, { maxDepth: 5, maxNodes: 400 })
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = false
  }
}

async function runAction(cap: string, extra: Record<string, any> = {}) {
  if (!selected.value) {
    ElMessage.warning('请先选择一个窗口')
    return
  }
  // 关闭窗口是唯一会丢数据的动作：应用若有未保存内容会直接丢失。
  if (cap === 'close') {
    try {
      await ElMessageBox.confirm(
        `关闭「${selected.value.title || selected.value.process}」？若该应用有未保存的内容会直接丢失。`,
        '确认关闭窗口',
        { type: 'warning', confirmButtonText: '关闭窗口', cancelButtonText: '取消' },
      )
    } catch {
      return
    }
  }
  busy.value = true
  try {
    const res = await api.controlRun({
      cap,
      appId: selected.value.id,
      nodeId: selectedNode.value || undefined,
      ...extra,
    })
    ElMessage.success(res?.result || '已执行')
    if (cap === 'close') {
      tree.value = null
      selected.value = null
      await loadApps()
    }
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    busy.value = false
  }
}

const typeText = ref('')
const hotkeys = ref('ctrl+s')

function sendText() {
  if (!typeText.value) return
  runAction('type_text', { text: typeText.value }).then(() => (typeText.value = ''))
}

function sendHotkey() {
  const keys = hotkeys.value
    .split('+')
    .map((k) => k.trim())
    .filter(Boolean)
  if (!keys.length) return
  runAction('hotkey', { keys })
}

/** Flattens the tree for rendering; depth drives the indent. */
function flatten(node: any, out: any[] = []): any[] {
  if (!node) return out
  out.push(node)
  for (const c of node.children ?? []) flatten(c, out)
  return out
}

const treeRows = computed(() => flatten(tree.value?.root))

/** RFC3339 → local "YYYY-MM-DD HH:mm". The raw string is both long and
 *  unreadable next to a window title. */
function formatTime(s: string) {
  if (!s) return ''
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return s
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

onMounted(async () => {
  await load()
  await loadDeny()
  await loadBriefs()
  if (enabled.value) await loadApps()
})
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">软件控制</h2>
        <p class="page-sub">说出你想要什么，它替你在那个软件里做出来</p>
      </div>
      <el-switch
        :model-value="enabled"
        :loading="loading"
        active-text="已开启"
        inactive-text="已关闭"
        @change="toggleModule"
      />
    </div>

    <div class="intro">
      <p class="lead">
        你不会用 Blender，也不用去学——说一句「帮我做一个……」，它去操作 Blender，把东西做出来。
      </p>
      <p class="lead">就跟聊天一样：不满意就说，随时改、随时重做。</p>
    </div>

    <el-alert v-if="!supported && status" type="info" :closable="false" show-icon class="plat-alert">
      当前平台（{{ status.platform }}）还不支持替你操作界面。另外，用 Chromium / Electron 写的软件（浏览器、VS Code 这类）只能看到窗口本身，看不到里面的按钮和输入框。
    </el-alert>

    <div v-if="!enabled" class="hint">软件控制未开启。打开右上角开关后即可使用。</div>

    <template v-else>
      <!-- 1. 环境探测：这件事，这台电脑做得了吗 -->
      <section class="section">
        <h3 class="section-title">这件事，这台电脑做得了吗</h3>
        <p class="hint-line">
          说一句你想做什么，它去本机找合用的工具：有的直接用，没有的告诉你要装什么，只能操作界面的会先花点时间摸清怎么驱动。
        </p>
        <div class="probe-row">
          <el-input
            v-model="probeQuery"
            placeholder="例如：我要做一个 2.5D 游戏"
            :disabled="probing"
            @keyup.enter="runProbe"
          />
          <el-button :loading="probing" @click="runProbe">探测</el-button>
        </div>

        <template v-if="report">
          <div v-if="report.ready?.length" class="group">
            <h4>可以开工</h4>
            <div v-for="p in report.ready" :key="p.name" class="item">
              <span class="item-name">{{ p.name }}<span class="ver">{{ p.version ? ' ' + p.version : '' }}</span></span>
              <span class="item-path">{{ p.path }}</span>
              <span v-if="p.driveNote" class="item-note">用法：{{ p.driveNote }}</span>
            </div>
          </div>

          <div v-if="report.needsInit?.length" class="group">
            <h4>装了，但只能操作界面</h4>
            <p class="hint-line">要先花点时间摸清怎么驱动它，这段等待可以拿来把想法记进下面的任务简报。</p>
            <div v-for="p in report.needsInit" :key="p.name" class="item">
              <span class="item-name">{{ p.name }}<span class="ver">{{ p.version ? ' ' + p.version : '' }}</span></span>
              <span class="item-path">{{ p.path }}</span>
            </div>
          </div>

          <div v-if="report.missing?.length" class="group">
            <h4>本机没有</h4>
            <div v-for="w in report.missing" :key="w.name" class="item">
              <span class="item-name">{{ w.name }}</span>
              <span class="item-note">{{ w.why }}</span>
              <span class="item-path">安装：{{ w.install }}</span>
            </div>
            <p v-if="report.missingMore" class="hint-line">另有 {{ report.missingMore }} 个同类工具未列出。</p>
          </div>

          <p v-if="report.noMatch" class="hint-line">
            没认出这类需求。可以直接说想用哪个软件，或从下面「本机还装了」里挑。
          </p>
          <p v-if="report.others?.length" class="dim-line">本机还装了：{{ report.others.join('、') }}</p>
        </template>
      </section>

      <!-- 2. 任务简报：等的时候先把想法记下来 -->
      <section class="section">
        <h3 class="section-title">任务简报</h3>
        <p class="hint-line">
          等安装、等准备的时候，把想要的东西记在这里，边等边说；准备好了直接照着开工。每份都是硬盘上的一个 Markdown 文件，用什么编辑器都能打开。
        </p>
        <div class="probe-row">
          <el-input v-model="newBrief" placeholder="开一份新简报，例如「2.5D 游戏」" @keyup.enter="createBrief" />
          <el-button @click="createBrief">新建</el-button>
        </div>
        <el-table :data="briefs" size="small">
          <el-table-column prop="title" label="标题" min-width="160" />
          <el-table-column label="更新" width="150">
            <template #default="{ row }">{{ formatTime(row.updatedAt) }}</template>
          </el-table-column>
          <el-table-column prop="path" label="文件" min-width="240" show-overflow-tooltip />
          <el-table-column label="" width="80">
            <template #default="{ row }">
              <el-button size="small" text type="primary" @click="viewBrief(row.id)">查看</el-button>
            </template>
          </el-table-column>
          <template #empty><el-empty description="还没有简报" /></template>
        </el-table>
      </section>

      <!-- 3. 兜底：想自己动手的时候 -->
      <el-collapse class="section">
        <el-collapse-item title="手动操作（想自己动手时用）" name="manual">
          <div class="toolbar">
            <el-button size="small" :loading="busy" @click="loadApps">刷新窗口</el-button>
            <span class="hint-inline">
              共 {{ apps.length }} 个窗口<template v-if="apps.filter((a) => a.blocked).length">
                ，{{ apps.filter((a) => a.blocked).length }} 个由禁止清单拦截</template>
            </span>
          </div>

          <div class="split">
            <div class="pane">
              <el-table
                :data="apps"
                size="small"
                height="360"
                highlight-current-row
                @current-change="(r: any) => r && !r.blocked && inspect(r)"
              >
                <el-table-column label="标题" min-width="180">
                  <template #default="{ row }">
                    <div class="title-cell">
                      <span :class="{ blocked: row.blocked }">{{ row.title || '（无标题）' }}</span>
                      <el-tag v-if="row.blocked" size="small" type="info" effect="plain">禁止</el-tag>
                    </div>
                  </template>
                </el-table-column>
                <el-table-column prop="process" label="进程" min-width="150" show-overflow-tooltip />
                <template #empty><el-empty description="没有找到可控窗口" /></template>
              </el-table>
            </div>

            <div class="pane">
              <div v-if="!tree" class="hint">在左侧选择一个窗口，读取它的控件</div>
              <template v-else>
                <div class="tree-head">
                  <span>{{ tree.app.title || tree.app.process }}</span>
                  <el-tag v-if="tree.truncated" size="small" type="info" effect="plain">已截断</el-tag>
                  <el-button size="small" text @click="inspect(selected)">重新读取</el-button>
                </div>
                <el-table
                  :data="treeRows"
                  size="small"
                  height="320"
                  highlight-current-row
                  @current-change="(r: any) => (selectedNode = r?.id || '')"
                >
                  <el-table-column label="控件" min-width="240">
                    <template #default="{ row }">
                      <!-- A notice node is synthetic: it carries no id, so it
                           must not look selectable or the model will try to
                           click a control that does not exist. -->
                      <span v-if="row.type === 'notice'" class="notice-row">{{ row.name }}</span>
                      <span v-else :style="{ paddingLeft: row.depth * 14 + 'px' }">
                        <span :class="['type-tag', row.type || 'window']">{{ row.type || 'window' }}</span>
                        {{ row.name || '（未命名）' }}
                      </span>
                    </template>
                  </el-table-column>
                  <el-table-column label="状态" width="80">
                    <template #default="{ row }">
                      <span v-if="row.type === 'notice'"></span>
                      <el-tag v-else-if="!row.enabled" size="small" type="info" effect="plain">禁用</el-tag>
                      <el-tag v-else-if="row.value && !row.onScreen" size="small" type="info" effect="plain">屏外</el-tag>
                      <span v-else class="ok-dot">●</span>
                    </template>
                  </el-table-column>
                </el-table>
              </template>
            </div>
          </div>

          <el-card v-if="selected" shadow="never" class="action-card">
            <template #header>
              <div class="action-head">
                <span>对「{{ selected.title || selected.process }}」执行操作</span>
              </div>
            </template>
            <div class="actions">
              <el-button size="small" :loading="busy" @click="runAction('focus')">聚焦窗口</el-button>
              <el-button size="small" :loading="busy" @click="runAction('click')">
                点击{{ selectedNode ? '选中控件' : '窗口中心' }}
              </el-button>
              <el-input v-model="typeText" size="small" placeholder="输入文本后回车发送" style="width: 240px" @keyup.enter="sendText" />
              <el-input v-model="hotkeys" size="small" placeholder="如 ctrl+s" style="width: 120px" @keyup.enter="sendHotkey" />
              <el-button size="small" :loading="busy" @click="sendHotkey">发送快捷键</el-button>
              <el-button size="small" type="danger" plain :loading="busy" @click="runAction('close')">关闭窗口</el-button>
            </div>
          </el-card>
        </el-collapse-item>

        <el-collapse-item :title="`系统内置的禁止清单（${deny.length} 项）`" name="deny">
          <p class="hint-line">
            终端、任务管理器、密码管理器、安全软件这类目标命中即拒绝，智能体和这一页都一样拦。
          </p>
          <el-table :data="deny" size="small" max-height="300">
            <el-table-column prop="reason" label="原因" min-width="180" />
            <el-table-column prop="match" label="匹配" min-width="200" />
            <template #empty><el-empty description="禁止清单为空" /></template>
          </el-table>
        </el-collapse-item>
      </el-collapse>
    </template>

    <el-dialog
      v-model="briefOpen"
      :title="openBrief?.title || '任务简报'"
      width="640px"
      class="brief-dialog"
    >
      <p class="hint-line">{{ openBrief?.path }}</p>
      <pre class="brief-body">{{ openBrief?.content }}</pre>
      <p class="hint-line">想加内容，直接在对话里跟它说，或者用任何编辑器打开这个文件改。</p>
    </el-dialog>
  </div>
</template>

<style scoped>
.page {
  padding: 20px 24px 40px;
  max-width: 1080px;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 12px;
}

.page-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
}

.page-sub {
  margin: 4px 0 0;
  color: var(--el-text-color-secondary);
  font-size: 13px;
}

.intro {
  margin: 0 0 14px;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
  line-height: 1.7;
}

.intro .lead {
  margin: 0 0 4px;
  color: var(--el-text-color-regular);
}

.plat-alert {
  margin-bottom: 14px;
}

.hint {
  color: var(--el-text-color-secondary);
  font-size: 13px;
  padding: 24px 0;
}

.section {
  margin-bottom: 22px;
}

.section-title {
  margin: 0 0 6px;
  font-size: 14px;
  font-weight: 600;
}

.hint-line {
  margin: 0 0 10px;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
  line-height: 1.7;
}

.dim-line {
  margin: 10px 0 0;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  line-height: 1.7;
}

.probe-row {
  display: flex;
  gap: 8px;
  margin-bottom: 14px;
  max-width: 620px;
}

.group {
  margin: 0 0 14px;
}

.group h4 {
  margin: 0 0 6px;
  font-size: 13px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.item {
  display: flex;
  flex-direction: column;
  padding: 6px 0 6px 12px;
  border-left: 2px solid var(--el-border-color);
  margin-bottom: 4px;
}

.item-name {
  font-size: 13px;
  color: var(--el-text-color-primary);
}

.item-name .ver {
  color: var(--el-text-color-secondary);
}

.item-path {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
  word-break: break-all;
}

.item-note {
  font-size: 12px;
  color: var(--el-text-color-regular);
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 10px;
}

.hint-inline {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.notice-line {
  margin: 0 0 10px;
  display: block;
}

.split {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
  margin-bottom: 14px;
}

.pane {
  min-width: 0;
}

.title-cell {
  display: flex;
  align-items: center;
  gap: 6px;
}

.title-cell .blocked {
  color: var(--el-text-color-secondary);
}

.tree-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-size: 13px;
}

.notice-row {
  color: var(--el-text-color-secondary);
  font-style: italic;
}

.type-tag {
  display: inline-block;
  margin-right: 6px;
  padding: 0 4px;
  border-radius: 3px;
  font-size: 11px;
  color: var(--el-text-color-secondary);
  background: var(--el-fill-color-light);
}

.ok-dot {
  color: var(--el-color-success);
}

.action-card {
  margin-top: 4px;
}

.action-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}

.actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.brief-body {
  margin: 0;
  padding: 10px 12px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  font-size: 12.5px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 420px;
  overflow: auto;
  font-family: inherit;
}
</style>
