<script setup lang="ts">
/**
 * ToolsView — self-built tool management (DESIGN §8).
 *
 * Two surfaces live here because they answer the same question — "what can this
 * agent call, and who vouched for it":
 *   · 脚本工具  script tools, gated by the admission triple-check
 *   · MCP 服务器 stdio MCP servers, whose tools are proxied into the registry
 *
 * The registered-tool table at the bottom shows provenance (built-in / script /
 * MCP) so an operator can tell at a glance which entries a prompt is paying for.
 */
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import ControlView from './ControlView.vue'

const SOURCE_LABEL: Record<string, string> = {
  builtin: '内置',
  script: '脚本',
  mcp: 'MCP',
}

const STATUS_TAG: Record<string, string> = {
  running: 'success',
  stopped: 'info',
  starting: 'warning',
  error: 'danger',
}

// ---- state ----
const tab = ref('scripts')
const loading = ref(false)

const scripts = ref<any[]>([])
const servers = ref<any[]>([])
const tools = ref<any[]>([])
const runtimes = ref<Record<string, boolean>>({})

const dialog = ref(false)
const editingId = ref('')
const form = ref<any>(blankScript())
const submitting = ref(false)

const runDialog = ref(false)
const runTarget = ref<any>(null)
const runInput = ref('')
const runResult = ref<any>(null)
const running = ref(false)

const mcpDialog = ref(false)
const mcpEditingId = ref('')
const mcpForm = ref<any>(blankServer())
const mcpSubmitting = ref(false)
const mcpTestResult = ref<any>(null)

const callDialog = ref(false)
const callTarget = ref<any>(null)
const callToolName = ref('')
const callArgs = ref('{}')
const callResult = ref<any>(null)

function blankScript() {
  return {
    name: '',
    description: '',
    runtime: 'js',
    code: '// 从环境变量 AGENTHUB_INPUT 读取输入，把结果打印到 stdout\nconsole.log(process.env.AGENTHUB_INPUT || "")',
    timeoutSeconds: 30,
    schema: null as any,
    args: [] as string[],
  }
}

function blankServer() {
  return { name: '', command: '', argsText: '', dir: '', envText: '' }
}

const registeredTools = computed(() => tools.value)
const scriptTools = computed(() => tools.value.filter((t) => t.source === 'script'))
const mcpTools = computed(() => tools.value.filter((t) => t.source === 'mcp'))

/** Admission verdict shown as a tag on each script row. */
function admissionTag(s: any) {
  if (!s.admission) return { type: 'info', text: '未校验' }
  if (s.admission.ok) return { type: 'success', text: '已准入' }
  const gateName: Record<string, string> = {
    contract: '契约关',
    consistency: '一致性关',
    smoke: '冒烟关',
  }
  return { type: 'danger', text: `${gateName[s.admission.gate] || s.admission.gate}未过` }
}

async function load() {
  loading.value = true
  try {
    const [s, m, t, r] = await Promise.all([
      api.scripts(),
      api.mcpServers(),
      api.tools(),
      api.scriptRuntimes().catch(() => ({})),
    ])
    scripts.value = s || []
    servers.value = m || []
    tools.value = t || []
    runtimes.value = r || {}
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ---- script tool actions ----

function openCreate() {
  editingId.value = ''
  form.value = blankScript()
  dialog.value = true
}

function openEdit(s: any) {
  editingId.value = s.id
  form.value = {
    name: s.name,
    description: s.description,
    runtime: s.runtime,
    code: s.code,
    timeoutSeconds: s.timeoutSeconds || 30,
    schema: s.schema || null,
    args: s.args || [],
  }
  dialog.value = true
}

/** JSON Schema is optional; an empty box means "use the default {input} shape". */
function schemaText() {
  const raw = (form.value.schemaText ?? '').trim()
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch (e: any) {
    throw new Error('参数 schema 不是合法 JSON: ' + e.message)
  }
}

async function save() {
  submitting.value = true
  try {
    const payload = { ...form.value, schema: schemaText() }
    delete (payload as any).schemaText
    if (editingId.value) {
      await api.scriptUpdate(editingId.value, payload)
      ElMessage.success('已保存')
    } else {
      const r: any = await api.scriptCreate(payload)
      ElMessage.success(r?.notice || '已保存')
    }
    dialog.value = false
    await load()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    submitting.value = false
  }
}

async function admit(s: any) {
  try {
    const r: any = await api.scriptAdmit(s.id)
    if (r.ok) ElMessage.success('三关校验通过，可启用')
    else ElMessage.error(`${r.gate} 未通过：${r.message}`)
    await load()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function toggle(s: any) {
  try {
    await api.scriptToggle(s.id)
    await load()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function remove(s: any) {
  try {
    await ElMessageBox.confirm(`删除脚本工具「${s.name}」？该工具会立即从可调用列表中移除。`, '确认删除', {
      type: 'warning',
    })
  } catch {
    return
  }
  await api.scriptDelete(s.id).catch((e: any) => ElMessage.error(String(e?.message || e)))
  await load()
}

function openRun(s: any) {
  runTarget.value = s
  runInput.value = 'hello'
  runResult.value = null
  runDialog.value = true
}

async function doRun() {
  running.value = true
  runResult.value = null
  try {
    runResult.value = await api.scriptRun(runTarget.value.id, runInput.value)
  } catch (e: any) {
    runResult.value = { ok: false, error: String(e?.message || e) }
  } finally {
    running.value = false
  }
}

// ---- MCP actions ----

function openMcpCreate() {
  mcpEditingId.value = ''
  mcpForm.value = blankServer()
  mcpTestResult.value = null
  mcpDialog.value = true
}

function openMcpEdit(s: any) {
  mcpEditingId.value = s.id
  // Secrets come back masked; showing the mask in the editor keeps the operator
  // from overwriting a stored token with the literal text "sk-o****e974".
  const env = { ...(s.env || {}) }
  for (const [k, v] of Object.entries(s.secretEnv || {})) env[k] = v
  mcpForm.value = {
    name: s.name,
    command: s.command,
    argsText: (s.args || []).join(' '),
    dir: s.dir || '',
    envText: Object.entries(env)
      .map(([k, v]) => `${k}=${v}`)
      .join('\n'),
  }
  mcpTestResult.value = null
  mcpDialog.value = true
}

function parseEnv(text: string) {
  const out: Record<string, string> = {}
  for (const line of (text || '').split('\n')) {
    const t = line.trim()
    if (!t || t.startsWith('#')) continue
    const i = t.indexOf('=')
    if (i <= 0) continue
    out[t.slice(0, i).trim()] = t.slice(i + 1)
  }
  return out
}

async function mcpSave() {
  mcpSubmitting.value = true
  try {
    const payload: any = {
      name: mcpForm.value.name,
      command: mcpForm.value.command,
      args: mcpForm.value.argsText ? mcpForm.value.argsText.split(/\s+/).filter(Boolean) : [],
      dir: mcpForm.value.dir,
      env: parseEnv(mcpForm.value.envText),
    }
    if (mcpEditingId.value) await api.mcpUpdate(mcpEditingId.value, payload)
    else await api.mcpCreate(payload)
    mcpDialog.value = false
    ElMessage.success('已保存')
    await load()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    mcpSubmitting.value = false
  }
}

async function mcpTest() {
  try {
    const payload: any = {
      name: mcpForm.value.name,
      command: mcpForm.value.command,
      args: mcpForm.value.argsText ? mcpForm.value.argsText.split(/\s+/).filter(Boolean) : [],
      dir: mcpForm.value.dir,
      env: parseEnv(mcpForm.value.envText),
    }
    mcpTestResult.value = await api.mcpTest(payload)
  } catch (e: any) {
    mcpTestResult.value = { ok: false, error: String(e?.message || e) }
  }
}

async function mcpToggle(s: any) {
  try {
    if (s.status === 'running') await api.mcpStop(s.id)
    else await api.mcpStart(s.id)
    await load()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
    await load()
  }
}

async function mcpRemove(s: any) {
  try {
    await ElMessageBox.confirm(
      `删除 MCP 服务器「${s.name}」？其工具会立即失效，已保存的密钥也会一并清除。`,
      '确认删除',
      { type: 'warning' },
    )
  } catch {
    return
  }
  await api.mcpDelete(s.id).catch((e: any) => ElMessage.error(String(e?.message || e)))
  await load()
}

function openCall(s: any) {
  callTarget.value = s
  callToolName.value = (s.toolNames || [])[0] || ''
  callArgs.value = '{}'
  callResult.value = null
  callDialog.value = true
}

async function doCall() {
  try {
    const args = JSON.parse(callArgs.value || '{}')
    callResult.value = { ok: true, output: await api.mcpCall(callTarget.value.id, callToolName.value, args) }
  } catch (e: any) {
    callResult.value = { ok: false, error: String(e?.message || e) }
  }
}
</script>

<template>
  <div class="page" v-loading="loading">
    <header class="head">
      <div>
        <h2>工具</h2>
        <p class="sub">内置工具 + 脚本工具 + MCP 服务器。自建工具须通过准入三关（契约 → 一致性 → 实弹冒烟）才可启用。</p>
      </div>
      <el-button @click="load">刷新</el-button>
    </header>

    <el-tabs v-model="tab">
      <!-- 脚本工具 -->
      <el-tab-pane label="脚本工具" name="scripts">
        <div class="bar">
          <el-button type="primary" @click="openCreate">新建脚本工具</el-button>
          <span class="hint">
            运行时可用：
            <el-tag size="small" :type="runtimes.js ? 'success' : 'danger'">node {{ runtimes.js ? '✓' : '✗' }}</el-tag>
            <el-tag size="small" :type="runtimes.python ? 'success' : 'danger'">python {{ runtimes.python ? '✓' : '✗' }}</el-tag>
          </span>
        </div>
        <el-alert
          type="warning"
          :closable="false"
          show-icon
          title="脚本在新进程里运行，是进程隔离而非安全沙箱——脚本可读写当前用户的文件。仅运行你自己信任的代码。"
          class="warn"
        />
        <el-table :data="scripts" border>
          <el-table-column label="名称" min-width="150">
            <template #default="{ row }">
              <div class="name">{{ row.name }}</div>
              <div class="mono dim">{{ row.runtime }} · {{ row.timeoutSeconds }}s</div>
            </template>
          </el-table-column>
          <el-table-column prop="description" label="描述" min-width="220" show-overflow-tooltip />
          <el-table-column label="准入" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="admissionTag(row).type">{{ admissionTag(row).text }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="启用" width="80">
            <template #default="{ row }">
              <el-switch :model-value="row.enabled" @change="toggle(row)" />
            </template>
          </el-table-column>
          <el-table-column label="最近一次运行" min-width="180">
            <template #default="{ row }">
              <span v-if="!row.lastRun" class="dim">—</span>
              <span v-else :class="row.lastRun.ok ? 'ok' : 'bad'">
                {{ row.lastRun.ok ? '成功' : '失败' }} · {{ row.lastRun.durationMs }}ms
              </span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="250" fixed="right">
            <template #default="{ row }">
              <el-button size="small" @click="admit(row)">准入校验</el-button>
              <el-button size="small" @click="openRun(row)">试跑</el-button>
              <el-button size="small" @click="openEdit(row)">编辑</el-button>
              <el-button size="small" type="danger" plain @click="remove(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-empty v-if="!loading && !scripts.length" description="还没有脚本工具" />
      </el-tab-pane>

      <!-- MCP -->
      <el-tab-pane label="MCP 服务器" name="mcp">
        <div class="bar">
          <el-button type="primary" @click="openMcpCreate">添加 MCP 服务器</el-button>
          <span class="hint">v1 仅支持 stdio 传输；工具以 mcp__&lt;服务器&gt;__&lt;工具&gt; 命名注册。</span>
        </div>
        <el-table :data="servers" border>
          <el-table-column label="名称" min-width="150">
            <template #default="{ row }">
              <div class="name">{{ row.name }}</div>
              <div class="mono dim">{{ row.command }}</div>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="110">
            <template #default="{ row }">
              <el-tooltip v-if="row.lastError" :content="row.lastError" placement="top">
                <el-tag size="small" :type="STATUS_TAG[row.status] || 'info'">{{ row.status }}</el-tag>
              </el-tooltip>
              <el-tag v-else size="small" :type="STATUS_TAG[row.status] || 'info'">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="工具" min-width="180">
            <template #default="{ row }">
              <span v-if="!row.toolCount" class="dim">—</span>
              <span v-else>{{ row.toolCount }} 个：{{ (row.toolNames || []).join('、') }}</span>
            </template>
          </el-table-column>
          <el-table-column label="协议" width="140">
            <template #default="{ row }">
              <span v-if="row.protocol" class="mono">{{ row.protocol }}</span>
              <span v-else class="dim">—</span>
            </template>
          </el-table-column>
          <el-table-column label="密钥 env" min-width="140">
            <template #default="{ row }">
              <span v-if="!row.secretEnv" class="dim">—</span>
              <el-tag v-for="(v, k) in row.secretEnv" :key="k" size="small" type="warning" class="tag">
                {{ k }}={{ v }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="240" fixed="right">
            <template #default="{ row }">
              <el-button size="small" :type="row.status === 'running' ? 'warning' : 'success'" @click="mcpToggle(row)">
                {{ row.status === 'running' ? '停止' : '启动' }}
              </el-button>
              <el-button size="small" :disabled="row.status !== 'running'" @click="openCall(row)">调用</el-button>
              <el-button size="small" @click="openMcpEdit(row)">编辑</el-button>
              <el-button size="small" type="danger" plain @click="mcpRemove(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-empty v-if="!loading && !servers.length" description="还没有 MCP 服务器" />
      </el-tab-pane>

      <!-- 已注册工具 -->
      <el-tab-pane :label="`已注册工具（${registeredTools.length}）`" name="registered">
        <el-table :data="registeredTools" border>
          <el-table-column prop="name" label="名称" min-width="220" />
          <el-table-column label="来源" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.source === 'builtin' ? 'info' : row.source === 'mcp' ? 'warning' : 'success'">
                {{ SOURCE_LABEL[row.source] || row.source }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="description" label="描述" min-width="260" show-overflow-tooltip />
        </el-table>
      </el-tab-pane>

      <!-- 软件控制：让代理替你在本机软件里做事（默认开启） -->
      <el-tab-pane label="软件控制" name="control">
        <ControlView />
      </el-tab-pane>
    </el-tabs>

    <!-- 脚本编辑 -->
    <el-dialog v-model="dialog" :title="editingId ? '编辑脚本工具' : '新建脚本工具'" width="760px">
      <el-form label-width="92px">
        <el-form-item label="名称">
          <el-input v-model="form.name" placeholder="如 reverse_text（字母/数字/下划线，不能有空格）" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" placeholder="这个工具做什么用、模型何时该调用它" />
        </el-form-item>
        <el-form-item label="运行时">
          <el-radio-group v-model="form.runtime">
            <el-radio-button label="js">JavaScript (node)</el-radio-button>
            <el-radio-button label="python">Python</el-radio-button>
          </el-radio-group>
          <span class="hint" style="margin-left: 12px">超时 {{ form.timeoutSeconds }}s</span>
        </el-form-item>
        <el-form-item label="超时(秒)">
          <el-input-number v-model="form.timeoutSeconds" :min="1" :max="600" />
        </el-form-item>
        <el-form-item label="代码">
          <el-input v-model="form.code" type="textarea" :rows="10" class="mono" />
        </el-form-item>
        <el-form-item label="参数 schema">
          <el-input
            v-model="form.schemaText"
            type="textarea"
            :rows="4"
            class="mono"
            placeholder='留空则使用默认 {"input": "string"}；填写则形如 {"type":"object","properties":{"city":{"type":"string"}}}'
          />
        </el-form-item>
        <el-form-item label="参数名">
          <el-select v-model="form.args" multiple filterable allow-create placeholder="以 AGENTHUB_ARG_<名称> 传入脚本" />
        </el-form-item>
      </el-form>
      <el-alert
        type="info"
        :closable="false"
        show-icon
        title="输入通过环境变量 AGENTHUB_INPUT 与标准输入提供；结果打印到 stdout。保存后需执行「准入校验」通过才能启用。"
      />
      <template #footer>
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <!-- 试跑 -->
    <el-dialog v-model="runDialog" :title="`试跑：${runTarget?.name || ''}`" width="640px">
      <el-input v-model="runInput" placeholder="输入文本" />
      <pre v-if="runResult" class="out" :class="runResult.ok ? 'ok' : 'bad'">{{
        runResult.ok ? runResult.output : runResult.error
      }}</pre>
      <template #footer>
        <el-button @click="runDialog = false">关闭</el-button>
        <el-button type="primary" :loading="running" @click="doRun">执行</el-button>
      </template>
    </el-dialog>

    <!-- MCP 编辑 -->
    <el-dialog v-model="mcpDialog" :title="mcpEditingId ? '编辑 MCP 服务器' : '添加 MCP 服务器'" width="680px">
      <el-form label-width="92px">
        <el-form-item label="名称">
          <el-input v-model="mcpForm.name" placeholder="如 filesystem / github" />
        </el-form-item>
        <el-form-item label="命令">
          <el-input v-model="mcpForm.command" placeholder="npx" />
        </el-form-item>
        <el-form-item label="参数">
          <el-input v-model="mcpForm.argsText" placeholder="-y @modelcontextprotocol/server-filesystem C:/data" />
        </el-form-item>
        <el-form-item label="工作目录">
          <el-input v-model="mcpForm.dir" placeholder="可留空" />
        </el-form-item>
        <el-form-item label="环境变量">
          <el-input
            v-model="mcpForm.envText"
            type="textarea"
            :rows="4"
            class="mono"
            placeholder="每行一个 KEY=VALUE。名称含 TOKEN/KEY/SECRET/PASSWORD 的值会自动加密存储。"
          />
        </el-form-item>
      </el-form>
      <el-alert
        v-if="mcpTestResult"
        :type="mcpTestResult.ok ? 'success' : 'error'"
        :closable="false"
        show-icon
        :title="mcpTestResult.ok ? `连接成功：${mcpTestResult.serverVersion}，${mcpTestResult.toolCount} 个工具` : '连接失败'"
        :description="mcpTestResult.ok ? (mcpTestResult.tools || []).join('、') : mcpTestResult.error"
        class="test"
      />
      <template #footer>
        <el-button @click="mcpTest">测试连接</el-button>
        <el-button @click="mcpDialog = false">取消</el-button>
        <el-button type="primary" :loading="mcpSubmitting" @click="mcpSave">保存</el-button>
      </template>
    </el-dialog>

    <!-- MCP 调用 -->
    <el-dialog v-model="callDialog" :title="`调用：${callTarget?.name || ''}`" width="640px">
      <el-form label-width="72px">
        <el-form-item label="工具">
          <el-select v-model="callToolName" style="width: 100%">
            <el-option v-for="t in callTarget?.toolNames || []" :key="t" :label="t" :value="t" />
          </el-select>
        </el-form-item>
        <el-form-item label="参数">
          <el-input v-model="callArgs" type="textarea" :rows="4" class="mono" />
        </el-form-item>
      </el-form>
      <pre v-if="callResult" class="out" :class="callResult.ok ? 'ok' : 'bad'">{{
        callResult.ok ? callResult.output : callResult.error
      }}</pre>
      <template #footer>
        <el-button @click="callDialog = false">关闭</el-button>
        <el-button type="primary" @click="doCall">调用</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  margin-bottom: 12px;
}
.sub {
  margin: 4px 0 0;
  opacity: 0.7;
  font-size: 13px;
  max-width: 720px;
}
.bar {
  display: flex;
  align-items: center;
  gap: 14px;
  margin-bottom: 12px;
}
.hint {
  font-size: 12px;
  opacity: 0.75;
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.warn {
  margin-bottom: 12px;
}
.name {
  font-weight: 600;
}
.dim {
  opacity: 0.6;
  font-size: 12px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-danger);
}
.tag {
  margin-right: 4px;
}
.out {
  margin-top: 12px;
  padding: 10px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
  max-height: 260px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.test {
  margin-top: 8px;
}
</style>