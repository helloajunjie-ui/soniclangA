<script setup lang="ts">
/**
 * 设置页。
 *
 * 这里只放「需要你自己拍板的接入与维护选项」。凡是系统自己该决定的事，都不在
 * 这一页出现（DESIGN §17 原则五、原则六）：
 *
 * - 自动判断这轮是对话还是任务、用关键词还是让模型判断 —— 编排内部的事，用户
 *   没法「分配」，也不该被问「开不开」，因为不开并不会带来任何好处；
 * - 服务商故障怎么补救、冷却多久 —— 容灾是网关的基本职责，不是可选项；
 * - 记忆怎么提炼 —— 属于记忆模块，入口在「记忆」页，不该在设置里再开一块；
 * - 新对话用哪个角色 —— 新对话就是默认助手，用户要换角色是在对话里换，不需要
 *   预先设一个全局默认值。
 *
 * 把它们摆出来只会让用户以为这些是必须自己决定的，从而关掉能用的东西。
 * 对应的字段仍然保留在 settings.json 里，供手工编辑和后续接隐私控制使用。
 */
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/client'
import { refreshProviders, refreshRoles, store } from '@/stores/app'

const router = useRouter()

const s = ref<any>(null)
/** 高级默认展开：去掉那些内部开关后，这里就是本页的全部内容。 */
const advanced = ref<string[]>(['adv'])
/** Storage driver as it was on load, so a change can be called out. */
const driverAtLoad = ref('')
const saving = ref(false)

async function load() {
  const raw = JSON.parse(JSON.stringify((await api.settings()) || {}))
  s.value = raw
  driverAtLoad.value = raw.storageDriver || 'sqlite'
}

async function save() {
  if (s.value.storageDriver !== driverAtLoad.value) {
    try {
      await ElMessageBox.confirm(
        '更改数据存储方式后需要重启 AgentHub 才会生效。现在保存吗？',
        '需要重启',
        { type: 'warning', confirmButtonText: '保存', cancelButtonText: '取消' },
      )
    } catch {
      return
    }
  }
  saving.value = true
  try {
    await api.settingsSave(s.value)
    await load()
    ElMessage.success('已保存')
  } catch (e: any) {
    ElMessage.error('保存失败：' + String(e?.message || e))
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-if="s">
    <div class="page-head">
      <div>
        <h2 class="page-title">设置</h2>
        <p class="page-sub">
          接入与运行相关的选项。它们都有合理默认值，不动也能正常使用。
        </p>
      </div>
      <el-button size="small" type="primary" :loading="saving" @click="save">保存</el-button>
    </div>

    <el-collapse v-model="advanced" class="adv">
      <el-collapse-item name="adv">
        <template #title>高级</template>
        <el-card shadow="never" class="adv-card">
          <div class="section-title">数据存储方式</div>
          <el-radio-group v-model="s.storageDriver">
            <el-radio-button value="sqlite">默认（推荐）</el-radio-button>
            <el-radio-button value="json">文件模式</el-radio-button>
          </el-radio-group>
          <p class="hint block">
            文件模式把所有数据写成普通文件，方便手工查看与排查；数据量大时读写较慢。
            <strong>切换后需要重启 AgentHub 才会生效</strong>，已有数据会自动迁移。
          </p>
        </el-card>
        <el-card shadow="never" class="adv-card">
          <div class="section-title">助手的记忆与行为规则</div>
          <p class="hint block">
            它自己记下的东西、和它做事时遵循的规则。这些平时不用管 —— 只在它记错了，
            或某条规则总在不合适的时候触发，才需要进去处理。
          </p>
          <div class="adv-actions">
            <el-button size="small" @click="router.push('/memory')">查看它记住了什么</el-button>
            <el-button size="small" @click="router.push('/methodology')">查看行为规则</el-button>
          </div>
        </el-card>
        <el-card shadow="never" class="adv-card">
          <div class="section-title">维护</div>
          <el-button size="small" @click="refreshRoles(); refreshProviders()">重新读取角色与服务商</el-button>
          <p class="hint block">在其它窗口改过数据、但当前页面显示的还是旧内容时使用。</p>
        </el-card>
        <el-card shadow="never" class="adv-card">
          <div class="section-title">关于</div>
          <p class="hint block">
            AgentHub · 本机一体化 LLM 网关 + 智能体运行时<br />
            后端版本：{{ store.backendVersion || '未知' }}<br />
            数据与密钥都保存在本机，不会外传。
          </p>
        </el-card>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<style scoped>
.hint {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  margin-left: 8px;
}
.hint.block {
  display: block;
  margin: 0;
  line-height: 1.6;
}
.adv-actions {
  margin-top: 10px;
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.adv {
  margin-top: 4px;
}
.adv-card {
  margin-bottom: 12px;
}
.adv-card:last-child {
  margin-bottom: 0;
}
</style>
