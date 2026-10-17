<script setup lang="ts">
import { computed, ref, watch, nextTick } from 'vue'
import MarkdownIt from 'markdown-it'
import hljs from 'highlight.js/lib/common'
import { store } from '@/stores/app'

const props = defineProps<{ content: string }>()

// Lightbox state: clicking an image opens it full-size.
const lightboxImg = ref('')
function openLightbox(src: string) { lightboxImg.value = src }
function closeLightbox() { lightboxImg.value = '' }

// Mermaid 按需加载：它连同几十种图表解析器在内核里就占大头，而绝大多数
// 消息里没有一张图。静态 import 会把它压进首屏那个 chunk（1.22 MB），
// 于是每条打开聊天的人都要为「可能有图」付出代价。改成第一次遇到 mermaid
// 代码块时才 import，vite 会把 mermaid 单独切出去。
type Mermaid = (typeof import('mermaid'))['default']
let mermaidPromise: Promise<Mermaid> | null = null
function loadMermaid(): Promise<Mermaid> {
  if (!mermaidPromise) {
    mermaidPromise = import('mermaid').then((m) => m.default)
    // 加载失败就允许下次重试，否则一次网络抖动会让整页图表永久性坏掉。
    mermaidPromise.catch(() => { mermaidPromise = null })
  }
  return mermaidPromise
}

// Pick a mermaid theme matching the active UI palette so diagrams are not a
// white block in dark mode or a dark block in the light/soft themes.
function currentMermaidTheme(): 'dark' | 'default' | 'neutral' {
  const root = document.documentElement
  if (root.classList.contains('dark')) return 'dark'
  return root.dataset.theme === 'soft' ? 'neutral' : 'default'
}

const mdContainer = ref<HTMLElement>()

// Markdown rendering is done once per message and cached: re-parsing the whole
// thread on every delta would dominate the frame budget during streaming.
const md: MarkdownIt = new MarkdownIt({
  html: false, // never trust model output with raw HTML
  linkify: true,
  breaks: true,
  highlight(str: string, lang: string): string {
    if (lang === 'mermaid') {
      // Keep mermaid blocks as-is, we'll render them after mount.
      return `<pre class="mermaid-block">${str}</pre>`
    }
    if (lang && hljs.getLanguage(lang)) {
      try {
        const html = hljs.highlight(str, { language: lang, ignoreIllegals: true }).value
        return `<pre class="hljs"><code>${html}</code></pre>`
      } catch {
        /* fall through to escaped plain text */
      }
    }
    const escaped = str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
    return `<pre class="hljs"><code>${escaped}</code></pre>`
  },
})

// Disable linkify inside code blocks: URLs in code should stay as plain text.
md.core.ruler.at('linkify', (state) => {
  for (let i = 0; i < state.tokens.length; i++) {
    const token = state.tokens[i]
    if (token.type === 'code_block' || token.type === 'fence') {
      // Skip linkify for code blocks — URLs inside should not become links.
      continue
    }
  }
  return true
})

// Override default link renderer: open in new tab.
md.renderer.rules.link_open = (tokens, idx) => {
  const token = tokens[idx]
  let href = token.attrGet('href') || '#'
  // Strip trailing Chinese punctuation that linkify often glues onto URLs.
  href = href.replace(/[，。、；：！？）】》」』、]+$/, '')
  return `<a href="${href}" target="_blank" rel="noopener noreferrer">`
}

const html = computed(() => {
  let text = props.content || ''
  // Auto-detect image URLs (jpg/png/gif/webp) and convert to Markdown image syntax
  text = text.replace(/(https?:\/\/[^\s)]+\.(?:jpg|jpeg|png|gif|webp)(?:\?[^\s)]*)?)/gi, '![]($1)')
  // Strip <ref>...</ref> citation tags (AI often dumps raw wiki markup).
  text = text.replace(/<ref[^>]*>.*?<\/ref>/gs, '')
  text = text.replace(/<ref[^>]*\/>/g, '')
  // Auto-link bare URLs: from http to first whitespace, wrap in markdown link.
  text = text.replace(/(https?:\/\/[^\s]+)/g, (match, url) => {
    // Strip trailing punctuation that isn't part of the URL.
    const clean = url.replace(/[，。、；：！？）】》」』、.,;:!?)\]]+$/, '')
    return `[${clean}](${clean})`
  })
  return md.render(text)
})

// Render mermaid blocks after the DOM updates. Re-runs on theme switch so
// diagrams follow the active palette.
async function renderMermaid() {
  await nextTick()
  const blocks = mdContainer.value?.querySelectorAll<HTMLElement>('.mermaid-block')
  if (!blocks?.length) return

  let mermaid: Mermaid
  try {
    mermaid = await loadMermaid()
  } catch {
    blocks.forEach((block) => {
      block.innerHTML = '<div class="mermaid-error">⚠ 图表库加载失败（离线或资源缺失），下面是原文</div>'
    })
    return
  }

  blocks.forEach(async (block, i) => {
    // On theme switch an already-rendered block holds the tabs+svg; restore the
    // raw code (saved in dataset.code) before re-rendering.
    let code = block.dataset.code || block.textContent || ''
    if (block.classList.contains('mermaid-rendered')) {
      block.classList.remove('mermaid-rendered')
      block.innerHTML = code
    }
    if (!code.trim()) return
    const id = `mmd-${Date.now()}-${i}`
    // Save original code for the toggle.
    block.dataset.code = code
    try {
      mermaid.initialize({ startOnLoad: false, theme: currentMermaidTheme(), securityLevel: 'loose' })
      const { svg } = await mermaid.render(id, code)
      // Build toggle UI: [预览] [代码]
      block.innerHTML = `
        <div class="mmd-tabs">
          <button class="mmd-tab active" data-view="preview">预览</button>
          <button class="mmd-tab" data-view="code">代码</button>
        </div>
        <div class="mmd-preview">${svg}</div>
        <pre class="mmd-code" style="display:none">${code}</pre>
      `
      block.classList.add('mermaid-rendered')
      // Wire up tab clicks.
      block.querySelectorAll('.mmd-tab').forEach((tab) => {
        tab.addEventListener('click', () => {
          block.querySelectorAll('.mmd-tab').forEach((t) => t.classList.remove('active'))
          tab.classList.add('active')
          const view = (tab as HTMLElement).dataset.view
          block.querySelector('.mmd-preview')!.setAttribute('style', view === 'preview' ? '' : 'display:none')
          block.querySelector('.mmd-code')!.setAttribute('style', view === 'code' ? '' : 'display:none')
        })
      })
    } catch (e) {
      // mermaid.render creates a temp element on failure — clean it up.
      const tempEl = document.getElementById(id)
      if (tempEl?.parentNode) tempEl.parentNode.removeChild(tempEl)
      block.innerHTML = `<div class="mermaid-error">⚠ 图表语法错误，无法渲染</div><pre class="mermaid-code">${code}</pre>`
    }
  })
}
watch(html, renderMermaid)
watch(() => store.theme, renderMermaid)

function onImgClick(e: MouseEvent) {
  const el = e.target as HTMLElement
  if (el.tagName === 'IMG') {
    openLightbox((el as HTMLImageElement).src)
  }
}
</script>

<template>
  <!-- eslint-disable-next-line vue/no-v-html -- output is escaped by markdown-it -->
  <div ref="mdContainer" class="md" v-html="html" @click="onImgClick" />

  <!-- Lightbox overlay: click image to zoom, click backdrop to close -->
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="lightboxImg" class="lightbox-backdrop" @click="closeLightbox">
        <img :src="lightboxImg" class="lightbox-img" @click.stop="closeLightbox" />
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.md :deep(p) {
  margin: 0 0 8px;
}
.md :deep(p:last-child) {
  margin-bottom: 0;
}
.md :deep(pre) {
  background: var(--ah-code-bg);
  color: var(--ah-code-text);
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 10px 12px;
  overflow-x: auto;
  font-size: 12.5px;
  line-height: 1.55;
  margin: 8px 0;
  white-space: pre-wrap;
  word-wrap: break-word;
  overflow-wrap: break-word;
}
/* Syntax highlighting follows the active theme via --ah-code-* tokens. */
.md :deep(.hljs) {
  background: var(--ah-code-bg);
  color: var(--ah-code-text);
}
.md :deep(.hljs-comment),
.md :deep(.hljs-quote) { color: var(--ah-code-comment); font-style: italic; }
.md :deep(.hljs-keyword),
.md :deep(.hljs-selector-tag),
.md :deep(.hljs-literal),
.md :deep(.hljs-section),
.md :deep(.hljs-doctag),
.md :deep(.hljs-name),
.md :deep(.hljs-attribute) { color: var(--ah-code-keyword); }
.md :deep(.hljs-string),
.md :deep(.hljs-regexp),
.md :deep(.hljs-addition),
.md :deep(.hljs-template-string) { color: var(--ah-code-string); }
.md :deep(.hljs-number),
.md :deep(.hljs-symbol),
.md :deep(.hljs-bullet),
.md :deep(.hljs-link),
.md :deep(.hljs-meta),
.md :deep(.hljs-template-variable),
.md :deep(.hljs-variable) { color: var(--ah-code-number); }
.md :deep(.hljs-title),
.md :deep(.hljs-title.function_),
.md :deep(.hljs-type),
.md :deep(.hljs-class .hljs-title),
.md :deep(.hljs-function) { color: var(--ah-code-fn); }
.md :deep(.hljs-tag),
.md :deep(.hljs-selector-id),
.md :deep(.hljs-selector-class),
.md :deep(.hljs-built_in) { color: var(--ah-code-builtin); }
.md :deep(.hljs-attr),
.md :deep(.hljs-selector-attr),
.md :deep(.hljs-selector-pseudo) { color: var(--ah-code-tag); }
.md :deep(.hljs-deletion) { color: var(--ah-danger); }
.md :deep(.hljs-emphasis) { font-style: italic; }
.md :deep(.hljs-strong) { font-weight: 700; }
.md :deep(pre code) {
  font-family: 'Cascadia Code', Consolas, monospace;
  background: transparent;
  padding: 0;
  font-size: inherit;
}
/* inline code (not inside a fenced block) */
.md :deep(:not(pre) > code) {
  font-family: 'Cascadia Code', Consolas, monospace;
  background: var(--ah-code-bg);
  color: var(--ah-code-text);
  border: 1px solid var(--ah-border);
  border-radius: 5px;
  padding: 1px 5px;
  font-size: 0.86em;
}
.md :deep(pre a) {
  color: inherit;
  text-decoration: none;
  pointer-events: none;
}
.md :deep(ul),
.md :deep(ol) {
  margin: 6px 0;
  padding-left: 20px;
}
.md :deep(table) {
  border-collapse: collapse;
  margin: 8px 0;
  display: block;
  overflow-x: auto;
  max-width: 100%;
}
.md :deep(td),
.md :deep(th) {
  border: 1px solid var(--ah-border);
  padding: 4px 9px;
  text-align: left;
}
.md :deep(th) {
  background: var(--ah-bg-sunken);
  font-weight: 600;
}
.md :deep(tr:nth-child(even) td) {
  background: color-mix(in srgb, var(--ah-bg-sunken) 55%, transparent);
}
.md :deep(blockquote) {
  border-left: 3px solid var(--ah-primary);
  margin: 8px 0;
  padding-left: 10px;
  color: var(--ah-text-dim);
}
.md :deep(a) {
  color: var(--ah-primary);
}
.md :deep(img) {
  max-width: 100%;
  max-height: 280px;
  border-radius: 8px;
  margin: 6px 0;
  display: block;
  cursor: zoom-in;
  transition: max-height 0.2s;
}
.md :deep(img:hover) {
  max-height: 320px;
}

/* Mermaid blocks */
.md :deep(.mermaid-block) {
  background: var(--ah-bg-sunken);
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 16px;
  margin: 8px 0;
  overflow-x: auto;
  text-align: center;
}
.md :deep(.mermaid-block svg) {
  max-width: 100%;
  height: auto;
}
.md :deep(.mermaid-error) {
  color: #f56c6c;
  font-size: 13px;
  margin-bottom: 8px;
  padding: 8px 12px;
  background: rgba(245, 108, 108, 0.1);
  border-radius: 6px;
}
.md :deep(.mmd-tabs) {
  display: flex;
  gap: 4px;
  margin-bottom: 8px;
}
.md :deep(.mmd-tab) {
  padding: 3px 10px;
  font-size: 12px;
  border: 1px solid var(--ah-border);
  background: var(--ah-bg-sunken);
  color: var(--ah-text-dim);
  border-radius: 4px;
  cursor: pointer;
}
.md :deep(.mmd-tab.active) {
  background: var(--ah-primary);
  color: #fff;
  border-color: var(--ah-primary);
}
.md :deep(.mmd-code) {
  font-size: 12px;
  color: var(--ah-text-dim);
  background: var(--ah-bg-sunken);
  padding: 8px;
  border-radius: 4px;
  overflow-x: auto;
  margin: 0;
}

/* Lightbox overlay */
.lightbox-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.85);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 9999;
  cursor: zoom-out;
}
.lightbox-img {
  max-width: 90vw;
  max-height: 90vh;
  border-radius: 8px;
  box-shadow: 0 8px 32px rgba(0, 0, 0, 0.5);
}
.fade-enter-active, .fade-leave-active {
  transition: opacity 0.2s;
}
.fade-enter-from, .fade-leave-to {
  opacity: 0;
}
</style>
