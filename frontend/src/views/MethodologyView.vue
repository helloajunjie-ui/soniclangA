<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { refreshMethodologies, store } from '@/stores/app'

const KIND_LABEL: Record<string, string> = {
  good: '好经验',
  bad: '反面案例',
  principle: '常驻原则',
}

const dialog = ref(false)
const editing = ref<any>(null)
// 内置规则默认收起：它们是系统自带的工作准则，展开只是为了「发现某条总在
// 误触发时能把它关掉」，不是让用户去逐条阅读和管理。
const showBuiltin = ref(false)

function group(list: any[]) {
  const out: { kind: string; list: any[] }[] = []
  for (const k of ['principle', 'good', 'bad']) {
    const l = list.filter((m: any) => m.kind === k)
    if (l.length) out.push({ kind: k, list: l })
  }
  return out
}

const mine = computed(() => store.methodologies.filter((m: any) => !m.builtin))
const builtin = computed(() => store.methodologies.filter((m: any) => m.builtin))
const groupedMine = computed(() => group(mine.value))
const groupedBuiltin = computed(() => group(builtin.value))

function create(kind = 'good') {
  editing.value = { id: '', name: '', content: '', kind, keywords: [], always: kind === 'principle', enabled: true }
  dialog.value = true
}

function edit(m: any) {
  editing.value = JSON.parse(JSON.stringify(m))
  dialog.value = true
}

async function save() {
  try {
    if (editing.value.id) await api.methodologyUpdate(editing.value.id, editing.value)
    else await api.methodologyCreate(editing.value)
    dialog.value = false
    await refreshMethodologies()
    ElMessage.success('已保存')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function toggle(m: any) {
  await api.methodologyToggle(m.id)
  await refreshMethodologies()
}

async function remove(m: any) {
  try {
    await ElMessageBox.confirm(
      `删除「${m.name}」？${m.always ? '这是常驻原则，删除后每轮对话都不再注入。' : '删除后不再参与匹配。'}`,
      '确认删除',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  await api.methodologyDelete(m.id)
  await refreshMethodologies()
  ElMessage.success('已删除')
}

onMounted(refreshMethodologies)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">行为规则</h2>
        <p class="page-sub">
          助手做事时遵循的规则，命中后会自动参考，不需要你每轮重复交代。平时不用管 ——
          某条规则总在不合适的时候触发，再进来把它关掉。
        </p>
      </div>
      <div style="display: flex; gap: 8px">
        <el-button size="small" @click="create('principle')">加常驻原则</el-button>
        <el-button size="small" @click="create('good')">加好经验</el-button>
        <el-button size="small" @click="create('bad')">加反面案例</el-button>
      </div>
    </div>

    <el-empty v-if="!store.methodologies.length" description="还没有任何行为规则" />

    <!-- 用户自己写的：主视图 -->
    <template v-if="mine.length">
      <div v-for="g in groupedMine" :key="g.kind" class="group">
        <div class="section-title">{{ KIND_LABEL[g.kind] }}（{{ g.list.length }}）</div>
        <el-card v-for="m in g.list" :key="m.id" shadow="never" class="meth">
          <div class="m-row">
            <div style="flex: 1; min-width: 0">
              <div class="m-name">
                {{ m.name }}
                <el-tag v-if="m.always" size="small" type="warning" effect="plain">常驻</el-tag>
              </div>
              <div class="m-body">{{ m.content }}</div>
              <div v-if="m.keywords?.length" class="m-kw">
                <span class="dim">触发词</span>
                <el-tag v-for="k in m.keywords" :key="k" size="small" effect="plain">{{ k }}</el-tag>
              </div>
            </div>
            <el-switch :model-value="m.enabled" size="small" @change="toggle(m)" />
            <el-button text size="small" @click="edit(m)">编辑</el-button>
            <el-button text size="small" type="danger" @click="remove(m)">删除</el-button>
          </div>
        </el-card>
      </div>
    </template>

    <el-empty v-else-if="builtin.length" description="你还没有自定义的行为规则，AI 目前只遵循系统内置规则" />

    <!-- 内置规则：收起，只在需要关掉某条时展开 -->
    <div v-if="builtin.length" class="builtin-wrap">
      <div class="builtin-head" @click="showBuiltin = !showBuiltin">
        <el-icon class="caret" :class="{ open: showBuiltin }"><ArrowRight /></el-icon>
        <span>系统内置规则（{{ builtin.length }}）</span>
        <span class="dim">随系统提供，通常无需改动；若某条总在不合适的场景触发，可单独关闭</span>
      </div>

      <div v-if="showBuiltin" class="builtin-body">
        <div v-for="g in groupedBuiltin" :key="g.kind" class="group">
          <div class="section-title">{{ KIND_LABEL[g.kind] }}（{{ g.list.length }}）</div>
          <el-card v-for="m in g.list" :key="m.id" shadow="never" class="meth quiet">
            <div class="m-row">
              <div style="flex: 1; min-width: 0">
                <div class="m-name">
                  {{ m.name }}
                  <el-tag v-if="m.always" size="small" type="warning" effect="plain">常驻</el-tag>
                  <el-tag v-if="m.hits" size="small" type="info" effect="plain">已触发 {{ m.hits }} 次</el-tag>
                </div>
                <div class="m-body">{{ m.content }}</div>
                <div v-if="m.keywords?.length" class="m-kw">
                  <span class="dim">触发词</span>
                  <el-tag v-for="k in m.keywords" :key="k" size="small" effect="plain">{{ k }}</el-tag>
                </div>
              </div>
              <el-switch :model-value="m.enabled" size="small" @change="toggle(m)" />
            </div>
          </el-card>
        </div>
      </div>
    </div>

    <el-dialog v-model="dialog" :title="editing?.id ? '编辑行为规则' : '新建行为规则'" width="580px">
      <el-form v-if="editing" label-width="70px" size="small">
        <el-form-item label="名称"><el-input v-model="editing.name" /></el-form-item>
        <el-form-item label="类型">
          <el-select v-model="editing.kind" style="width: 160px">
            <el-option label="常驻原则" value="principle" />
            <el-option label="好经验" value="good" />
            <el-option label="反面案例" value="bad" />
          </el-select>
        </el-form-item>
        <el-form-item label="内容"><el-input v-model="editing.content" type="textarea" :rows="3" /></el-form-item>
        <el-form-item label="触发词">
          <el-select v-model="editing.keywords" multiple filterable allow-create default-first-option style="width: 100%" placeholder="回车创建；对话里出现这些词时本条才会生效" />
        </el-form-item>
        <el-form-item label="常驻">
          <el-switch v-model="editing.always" />
          <span class="dim" style="margin-left: 8px">常驻则每轮都生效，不依赖触发词</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button size="small" @click="dialog = false">取消</el-button>
        <el-button size="small" type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.meth {
  margin-bottom: 6px;
}
.meth :deep(.el-card__body) {
  padding: 10px 14px;
}
.meth.quiet {
  border-color: transparent;
  background: var(--ah-bg-sunken);
}
.m-row {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}
.m-name {
  font-size: 13px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 6px;
}
.m-body {
  font-size: 12.5px;
  color: var(--ah-text-dim);
  margin-top: 3px;
}
.m-kw {
  margin-top: 6px;
  display: flex;
  align-items: center;
  gap: 4px;
  flex-wrap: wrap;
}
.builtin-wrap {
  margin-top: 18px;
  border-top: 1px solid var(--ah-border);
  padding-top: 12px;
}
.builtin-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  cursor: pointer;
  user-select: none;
}
.builtin-head .caret {
  transition: transform 0.15s;
  font-size: 12px;
}
.builtin-head .caret.open {
  transform: rotate(90deg);
}
.builtin-body {
  margin-top: 10px;
}
.dim {
  font-size: 11.5px;
  color: var(--ah-text-dim);
}
</style>
