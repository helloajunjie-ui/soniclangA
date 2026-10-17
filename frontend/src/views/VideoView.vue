<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/client'
import { refreshProviders, store } from '@/stores/app'

/**
 * 视频 —— 文生视频。小白只需：选模型 → 写一句 → 生成。
 * 视频模型从服务商的模型目录里按 id 关键词识别（sora/luma/pika/kling/runway/t2v 等）。
 */

const providerId = ref('')
const model = ref('')
const prompt = ref('')
const busy = ref(false)
// 后端 provider.VideoResult 的字段是 url / taskId / model（见 backend/internal/provider/provider.go）；
// 异步服务商只回 taskId，视频要稍后才能通过它查到。
const result = ref<{ url?: string; taskId?: string; model?: string } | null>(null)
const error = ref('')

const VIDEO_KW = /sora|luma|pika|kling|runway|video|t2v|i2v|vid|pixverse|hailuo/i

// 有视频生成模型的服务商
const videoProviders = computed(() =>
  (store.providers as any[]).filter((p) => ((p.models || []) as any[]).some((m) => VIDEO_KW.test(m.id))),
)

const videoModels = computed(() => {
  const p = (store.providers as any[]).find((x) => x.id === providerId.value)
  if (!p) return []
  return ((p.models || []) as any[]).filter((m) => VIDEO_KW.test(m.id)).map((m) => m.id)
})

onMounted(async () => {
  try {
    await refreshProviders()
    if (!videoProviders.value.length) return
    providerId.value = videoProviders.value[0].id
    if (videoModels.value.length) model.value = videoModels.value[0]
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
})

function onProviderChange() {
  model.value = ''
  if (videoModels.value.length) model.value = videoModels.value[0]
}

async function generate() {
  if (!providerId.value || !model.value) {
    ElMessage.error('请先选模型')
    return
  }
  if (!prompt.value.trim()) {
    ElMessage.error('先写一句想生成什么')
    return
  }
  busy.value = true
  error.value = ''
  result.value = null
  try {
    const r: any = await api.generateVideo(providerId.value, model.value, prompt.value.trim())
    if (r?.ok && r?.result) {
      result.value = r.result
    } else {
      error.value = r?.error || r?.message || '生成失败'
    }
  } catch (e: any) {
    error.value = String(e?.message || e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="video">
    <div class="video-form">
      <el-card shadow="never" class="v-card">
        <template #header><span class="card-title">视频</span></template>

        <template v-if="videoProviders.length">
          <el-form label-position="top" size="default">
            <el-form-item label="模型">
              <el-select v-model="providerId" style="width: 100%" @change="onProviderChange">
                <el-option v-for="p in videoProviders" :key="p.id" :label="p.name" :value="p.id" />
              </el-select>
              <el-select v-model="model" style="width: 100%; margin-top: 8px" filterable>
                <el-option v-for="m in videoModels" :key="m" :label="m" :value="m" />
              </el-select>
            </el-form-item>
            <el-form-item label="想生成什么">
              <el-input
                v-model="prompt"
                type="textarea"
                :rows="4"
                placeholder="比如：夕阳下海浪拍打沙滩，无人机航拍"
              />
            </el-form-item>
            <el-button type="primary" :loading="busy" @click="generate">生成</el-button>
          </el-form>
          <p v-if="error" class="err">{{ error }}</p>
        </template>

        <template v-else>
          <el-empty description="还没有可用的视频模型">
            <p class="dim">去「服务商」配一个有视频生成能力的服务商（如 sora、luma、pika），模型目录里会自动识别出视频模型。</p>
          </el-empty>
        </template>
      </el-card>
    </div>

    <div class="video-result">
      <el-card shadow="never" class="v-card">
        <template #header>
          <span class="card-title">结果</span>
          <span v-if="result?.model" class="dim">{{ result.model }}</span>
        </template>
        <div v-if="busy" class="res-busy">
          <el-icon class="is-loading" style="font-size: 30px"><Loading /></el-icon>
          <p class="dim">正在生成…</p>
          <p class="wait-hint">视频生成要一到几分钟，别着急，慢慢等。</p>
        </div>
        <template v-else-if="result?.url">
          <video :src="result.url" class="res-video" controls />
        </template>
        <template v-else-if="result?.taskId">
          <div class="res-task">
            <el-icon style="font-size: 30px; color: var(--ah-warn)"><Loading class="is-loading" /></el-icon>
            <p>视频任务已提交，正在生成…</p>
            <p class="dim">任务ID：{{ result.taskId }}</p>
            <p class="dim">服务商生成成片需要一到几分钟；若长时间未返回，可稍后再次生成。</p>
          </div>
        </template>
        <el-empty v-else description="生成结果会显示在这里" />
      </el-card>
    </div>
  </div>
</template>

<style scoped>
.video {
  display: flex;
  gap: 16px;
  height: 100%;
  overflow-y: auto;
  padding: 20px 24px;
}
.video-form {
  width: 360px;
  flex: none;
}
.video-result {
  flex: 1;
  min-width: 0;
}
.v-card {
  border: 1px solid var(--ah-border);
}
.card-title {
  font-size: 14px;
  font-weight: 600;
}
.res-video {
  width: 100%;
  max-height: 70vh;
  border-radius: 8px;
  background: var(--ah-bg-sunken);
}
.res-busy {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 40px 0;
}
.res-task {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 40px 0;
  text-align: center;
}
.res-task p {
  margin: 0;
}
.wait-hint {
  color: var(--ah-text-dim);
  font-size: 12px;
  margin: 0;
}
.err {
  margin-top: 10px;
  color: var(--el-color-danger);
  font-size: 12.5px;
  word-break: break-all;
}
.dim {
  color: var(--ah-text-dim);
  font-size: 12px;
}
</style>
