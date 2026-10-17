<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/client'
import { refreshProviders, store } from '@/stores/app'

/**
 * 绘画 —— 对齐 AQBot 的绘图工作台。
 * 左：参数面板（服务商/模型/尺寸/质量/输出格式/背景/批量/参考图）；
 * 右：结果按消息流排列（每次生成是一条消息），多图拼 2/4 列网格 + 底部常驻输入框。
 */

const providerId = ref('')
const model = ref('')
const size = ref('auto')
const quality = ref('auto')
const outputFormat = ref('png')
const background = ref('auto')
const n = ref(1)
const prompt = ref('')
const busy = ref(false)
// el-collapse 的 v-model 是「展开的面板 name 列表」，不是一个开关布尔值。
// 之前写成 ref(false)，语义上靠运行时被覆盖成 ['adv'] 才碰巧能用；
// 类型收窄之后这里就是显式约定。展开状态由 collapse 自己管。
const advanced = ref<string[]>([])
const refImages = ref('')

// 消息流：每条生成是一条消息（prompt + 图片列表）
interface PaintMsg {
  prompt: string
  model: string
  urls: string[]
  local?: string
  error?: string
}
const messages = ref<PaintMsg[]>([])
const feedEl = ref<HTMLElement | null>(null)
// lightbox：点击缩略图放大
const lightbox = ref('')

const IMAGE_KW = /dall|image|gpt-image|flux|cogview|seedream|grok.*image|imagen|dalle/i

const imageProviders = computed(() =>
  (store.providers as any[]).filter((p) => ((p.models || []) as any[]).some((m) => IMAGE_KW.test(m.id))),
)

const imageModels = computed(() => {
  const p = (store.providers as any[]).find((x) => x.id === providerId.value)
  if (!p) return []
  return ((p.models || []) as any[]).filter((m) => IMAGE_KW.test(m.id)).map((m) => m.id)
})

const isDallE = computed(() => /dall/i.test(model.value))

const sizes = computed(() => {
  const m = model.value.toLowerCase()
  if (m.includes('seedream') || m.includes('doubao')) {
    return ['2K', '4K', '1024x1024', '2048x2048']
  }
  if (m.includes('dall')) return ['auto', '1024x1024', '1792x1024', '1024x1792']
  if (m.includes('gpt-image')) return ['auto', '1024x1024', '1536x1024', '1024x1536']
  return ['auto', '512x512', '1024x1024', '1536x1024', '1024x1536', '1792x1024', '1024x1792']
})

const qualities = computed(() => {
  if (isDallE.value) return ['auto', 'standard', 'hd']
  return ['auto', 'low', 'medium', 'high']
})

onMounted(async () => {
  try {
    await refreshProviders()
    if (!imageProviders.value.length) return
    providerId.value = imageProviders.value[0].id
    if (imageModels.value.length) model.value = imageModels.value[0]
  } catch (e: any) {
    ElMessage.error(String(e?.message || e))
  }
})

function onProviderChange() {
  model.value = ''
  if (imageModels.value.length) model.value = imageModels.value[0]
}

function scrollBottom() {
  nextTick(() => {
    const el = feedEl.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

async function generate() {
  if (!providerId.value || !model.value) {
    ElMessage.error('请先选模型')
    return
  }
  if (!prompt.value.trim()) {
    ElMessage.error('先写一句想画什么')
    return
  }
  const promptText = prompt.value.trim()
  busy.value = true
  const refs = refImages.value
    .split(/[,，\n]/)
    .map((s: string) => s.trim())
    .filter(Boolean)
  try {
    const r: any = await api.generateImage(providerId.value, model.value, promptText, {
      size: size.value === 'auto' ? '' : size.value,
      quality: quality.value === 'auto' ? '' : quality.value,
      output_format: outputFormat.value,
      background: background.value === 'auto' ? '' : background.value,
      n: n.value,
      ...(refs.length === 1 ? { image: refs[0] } : refs.length > 1 ? { images: refs } : {}),
    })
    if (r?.ok && r?.result) {
      const res = r.result
      const urls: string[] = []
      if (res.images && res.images.length) urls.push(...res.images)
      else if (res.url) urls.push(res.url)
      else if (res.b64) urls.push(`data:image/png;base64,${res.b64}`)
      messages.value.push({ prompt: promptText, model: model.value, urls, local: res.localPath })
    } else {
      messages.value.push({ prompt: promptText, model: model.value, urls: [], error: r?.error || r?.message || '生成失败' })
    }
  } catch (e: any) {
    messages.value.push({ prompt: promptText, model: model.value, urls: [], error: String(e?.message || e) })
  } finally {
    busy.value = false
    scrollBottom()
  }
}

function keyEnter() {
  if (!busy.value) generate()
}
</script>

<template>
  <div class="paint">
    <!-- 左：参数面板（对齐 AQBot） -->
    <aside class="paint-side">
      <el-form label-position="top" size="small" class="side-form">
        <div class="pf">
          <label class="pf-label">服务商</label>
          <el-select v-model="providerId" size="default" @change="onProviderChange">
            <el-option v-for="p in imageProviders" :key="p.id" :label="p.name" :value="p.id" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">模型</label>
          <el-select v-model="model" size="default" filterable>
            <el-option v-for="m in imageModels" :key="m" :label="m" :value="m" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">尺寸</label>
          <el-select v-model="size" size="default">
            <el-option v-for="s in sizes" :key="s" :label="s === 'auto' ? '自动' : s" :value="s" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">质量</label>
          <el-select v-model="quality" size="default">
            <el-option v-for="q in qualities" :key="q" :label="q === 'auto' ? '自动' : q" :value="q" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">输出格式</label>
          <el-select v-model="outputFormat" size="default">
            <el-option label="PNG" value="png" />
            <el-option label="JPEG" value="jpeg" />
            <el-option label="WEBP" value="webp" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">背景</label>
          <el-select v-model="background" size="default">
            <el-option label="自动" value="auto" />
            <el-option label="透明" value="transparent" />
            <el-option label="不透明" value="opaque" />
          </el-select>
        </div>
        <div class="pf">
          <label class="pf-label">批量张数</label>
          <el-input-number v-model="n" :min="1" :max="4" size="default" style="width: 100%" />
        </div>

        <el-collapse v-model="advanced" class="adv">
          <el-collapse-item title="高级设置" name="adv">
            <div class="pf">
              <label class="pf-label">参考图 URL（图生图）</label>
              <el-input
                v-model="refImages"
                type="textarea"
                :rows="3"
                resize="none"
                placeholder="粘贴图片网址，多张用逗号或换行分隔"
              />
              <p class="adv-hint">豆包 seedream 4.0/5.0 支持参考图改图，多图参考可用逗号分隔。</p>
            </div>
          </el-collapse-item>
        </el-collapse>
      </el-form>
    </aside>

    <!-- 右：消息流 + 底部输入 -->
    <section class="paint-main">
      <div ref="feedEl" class="feed">
        <div v-if="!messages.length && !busy" class="feed-empty">
          <el-icon style="font-size: 56px; color: var(--ah-text-dim)"><Picture /></el-icon>
          <p>暂无绘画记录</p>
          <p class="wait-hint">在下方输入你想生成的画面，点「生成」</p>
        </div>

        <!-- 聊天流：用户提示词右侧气泡，AI 结果左侧缩略图气泡 -->
        <div v-for="(m, idx) in messages" :key="idx" class="bubble-row">
          <div class="bubble user">{{ m.prompt }}</div>

          <div v-if="m.error" class="bubble ai bubble-err">{{ m.error }}</div>
          <div v-else class="bubble ai">
            <div class="thumb-grid" :class="m.urls.length > 3 ? 't4' : m.urls.length === 2 ? 't2' : m.urls.length === 3 ? 't3' : 't1'">
              <img
                v-for="(u, i) in m.urls"
                :key="i"
                :src="u"
                class="thumb"
                @click="lightbox = u"
              />
            </div>
            <div v-if="m.local" class="saved">已保存本地</div>
          </div>
        </div>

        <div v-if="busy" class="bubble-row">
          <div class="bubble ai loading">
            <el-icon class="is-loading"><Loading /></el-icon>
            <p>正在生成…（一般几十秒，别着急）</p>
          </div>
        </div>
      </div>

      <!-- 点击放大 -->
      <Teleport to="body">
        <div v-if="lightbox" class="lb-mask" @click="lightbox = ''">
          <img :src="lightbox" class="lb-img" alt="大图" @click.stop />
        </div>
      </Teleport>

      <div class="composer">
        <el-input
          v-model="prompt"
          class="composer-input"
          type="textarea"
          :rows="2"
          resize="none"
          placeholder="输入你想生成的画面"
          @keydown.enter.exact.prevent="keyEnter"
        />
        <el-button type="primary" :loading="busy" @click="generate">生成</el-button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.paint {
  display: flex;
  height: 100%;
  overflow: hidden;
}
/* 左参数面板 */
.paint-side {
  width: 260px;
  flex: none;
  border-right: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
  padding: 16px 14px;
  overflow-y: auto;
}
.paint-side :deep(.el-select__wrapper),
.paint-side :deep(.el-input__wrapper) {
  border-radius: 10px;
}
.side-form .pf {
  margin-bottom: 14px;
}
.pf-label {
  display: block;
  font-size: 12px;
  color: var(--ah-text-dim);
  margin-bottom: 5px;
}
.side-form :deep(.el-select) {
  width: 100%;
}
.adv {
  margin-top: 4px;
}
.adv-hint {
  font-size: 12px;
  color: var(--ah-text-dim);
  margin: 0;
}

/* 右主区 */
.paint-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  padding: 16px 20px;
  gap: 12px;
}
.feed {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  border: 1px solid var(--ah-border);
  border-radius: 18px;
  background: var(--ah-bg-sunken);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.03);
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 20px;
}
.feed-empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--ah-text-dim);
}
.feed-empty p {
  margin: 0;
}
/* 聊天气泡流 */
.bubble-row {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.bubble {
  max-width: 78%;
  border-radius: 16px;
  padding: 9px 14px;
  font-size: 13px;
  line-height: 1.55;
  word-break: break-word;
}
.bubble.user {
  align-self: flex-end;
  background: linear-gradient(
    135deg,
    var(--el-color-primary),
    color-mix(in srgb, var(--el-color-primary) 78%, #000)
  );
  color: #f5f8fc;
  border-bottom-right-radius: 6px;
  box-shadow: 0 4px 16px color-mix(in srgb, var(--el-color-primary) 28%, transparent);
}
.bubble.ai {
  align-self: flex-start;
  background: var(--ah-bg-elev);
  border: 1px solid var(--ah-border);
  border-bottom-left-radius: 6px;
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.06);
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.bubble-err {
  color: var(--el-color-danger);
}
.bubble.loading {
  flex-direction: row;
  align-items: center;
  gap: 8px;
  color: var(--ah-text-dim);
}
.bubble.loading p {
  margin: 0;
  font-size: 12.5px;
}
/* 缩略图网格：小图，2/3/4 列 */
.thumb-grid {
  display: grid;
  gap: 8px;
}
.t1 {
  grid-template-columns: 1fr;
}
.t2 {
  grid-template-columns: repeat(2, 1fr);
}
.t3 {
  grid-template-columns: repeat(3, 1fr);
}
.t4 {
  grid-template-columns: repeat(4, 1fr);
}
.thumb {
  width: 100%;
  aspect-ratio: 1 / 1;
  object-fit: cover;
  border-radius: 12px;
  border: 1px solid var(--ah-border);
  cursor: zoom-in;
  display: block;
  transition: transform 0.18s ease, box-shadow 0.18s ease;
}
.thumb:hover {
  transform: scale(1.02);
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.2);
}
.saved {
  font-size: 11px;
  color: var(--ah-text-dim);
}
.wait-hint {
  font-size: 12px;
}
/* 点击放大 lightbox */
.lb-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.75);
  z-index: 9999;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: zoom-out;
}
.lb-img {
  max-width: 90vw;
  max-height: 90vh;
  object-fit: contain;
  border-radius: 14px;
  cursor: default;
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.4);
}
/* 底部输入：整体柔和圆角容器 */
.composer {
  display: flex;
  gap: 10px;
  align-items: flex-end;
  padding: 8px 10px 8px 14px;
  border-radius: 18px;
  border: 1px solid var(--ah-border);
  background: var(--ah-bg-elev);
  box-shadow: 0 2px 14px rgba(0, 0, 0, 0.05);
  transition: border-color 0.2s ease, box-shadow 0.2s ease;
}
.composer:focus-within {
  border-color: color-mix(in srgb, var(--el-color-primary) 60%, var(--ah-border));
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--el-color-primary) 16%, transparent);
}
.composer-input {
  flex: 1;
}
/* 输入框内层去边框，由容器统一提供 */
.composer-input :deep(.el-textarea__inner) {
  border: none;
  box-shadow: none;
  background: transparent;
  padding: 4px 0;
  color: var(--ah-text);
  caret-color: var(--el-color-primary);
}
.composer-input :deep(.el-textarea__inner:focus) {
  box-shadow: none;
}
.composer .el-button {
  height: 38px;
  border-radius: 12px;
  padding: 0 22px;
  font-weight: 500;
}
.composer .el-button.is-loading {
  opacity: 0.8;
}
</style>
