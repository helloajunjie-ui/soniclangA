<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { refreshSkills, store } from '@/stores/app'

const dirs = ref<string[]>([])
const scanning = ref(false)
const dialog = ref(false)
const editing = ref<any>(null)

async function load() {
  await refreshSkills()
  dirs.value = (await api.skillDirs())?.dirs || []
}

async function scan() {
  scanning.value = true
  try {
    const r = await api.skillScan()
    ElMessage.success(`扫描完成：新增 ${r.added}，更新 ${r.update}，共 ${r.total} 个技能`)
    await refreshSkills()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  } finally {
    scanning.value = false
  }
}

async function toggle(s: any) {
  try {
    await api.skillToggle(s.id)
    await refreshSkills()
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

function edit(s: any) {
  editing.value = JSON.parse(JSON.stringify(s))
  dialog.value = true
}

function create() {
  editing.value = { id: '', name: '新技能', description: '', instructions: '', tools: [], enabled: true }
  dialog.value = true
}

async function save() {
  try {
    if (editing.value.id) await api.skillUpdate(editing.value.id, editing.value)
    else await api.skillCreate(editing.value)
    dialog.value = false
    await refreshSkills()
    ElMessage.success('已保存')
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
}

async function remove(s: any) {
  await ElMessageBox.confirm(`删除技能「${s.name}」？`, '确认', { type: 'warning' })
  await api.skillDelete(s.id)
  await refreshSkills()
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">技能库</h2>
        <p class="page-sub">技能 = 提示词指令 + 工具绑定；支持 SKILL.md 目录扫描</p>
      </div>
      <div style="display: flex; gap: 8px; align-items: center">
        <el-button size="small" :loading="scanning" @click="scan">扫描目录</el-button>
        <el-button size="small" type="primary" @click="create">新建技能</el-button>
      </div>
    </div>

    <div class="dirs">
      <span class="dirs-label">扫描目录：</span>
      <code v-for="d in dirs" :key="d">{{ d }}</code>
    </div>

    <el-empty v-if="!store.skills.length" description="技能库为空" />
    <div v-else class="card-grid">
      <el-card v-for="s in store.skills" :key="s.id" shadow="hover">
        <div class="sc-head">
          <strong>{{ s.name }}</strong>
          <el-tag v-if="s.builtin" size="small" type="info" effect="plain">内置</el-tag>
          <el-tag v-if="s.sourcePath" size="small" type="success" effect="plain">文件</el-tag>
        </div>
        <div class="sc-desc">{{ s.description || '（无描述）' }}</div>
        <pre class="sc-body">{{ s.instructions }}</pre>
        <div class="sc-actions">
          <el-switch
            :model-value="s.enabled"
            size="small"
            :active-text="s.enabled ? '启用' : '停用'"
            @change="toggle(s)"
          />
          <div style="flex: 1" />
          <el-button size="small" text @click="edit(s)">编辑</el-button>
          <el-button v-if="!s.builtin" size="small" text type="danger" @click="remove(s)">删除</el-button>
        </div>
      </el-card>
    </div>

    <el-dialog v-model="dialog" :title="editing?.id ? '编辑技能' : '新建技能'" width="640px">
      <el-form v-if="editing" label-width="70px" size="small">
        <el-form-item label="名称"><el-input v-model="editing.name" /></el-form-item>
        <el-form-item label="描述"><el-input v-model="editing.description" /></el-form-item>
        <el-form-item label="指令">
          <el-input v-model="editing.instructions" type="textarea" :rows="8" placeholder="这个技能要求模型如何工作" />
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
.dirs {
  display: flex;
  gap: 10px;
  align-items: center;
  font-size: 12px;
  color: var(--ah-text-dim);
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.dirs code {
  background: var(--ah-bg-sunken);
  padding: 2px 7px;
  border-radius: 4px;
  font-size: 11.5px;
}
.sc-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.sc-desc {
  color: var(--ah-text-dim);
  font-size: 12.5px;
  margin: 4px 0 8px;
}
.sc-body {
  background: var(--ah-bg-sunken);
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 11.5px;
  white-space: pre-wrap;
  max-height: 110px;
  overflow: auto;
  margin: 0;
  font-family: inherit;
}
.sc-actions {
  margin-top: 10px;
  display: flex;
  align-items: center;
  gap: 6px;
}
</style>
