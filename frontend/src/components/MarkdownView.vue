<script setup lang="ts">
import { computed } from 'vue'
import MarkdownIt from 'markdown-it'
import hljs from 'highlight.js/lib/common'
import 'highlight.js/styles/github.css'

const props = defineProps<{ content: string }>()

// Markdown rendering is done once per message and cached: re-parsing the whole
// thread on every delta would dominate the frame budget during streaming.
const md: MarkdownIt = new MarkdownIt({
  html: false, // never trust model output with raw HTML
  linkify: true,
  breaks: true,
  highlight(str: string, lang: string): string {
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

const html = computed(() => md.render(props.content || ''))
</script>

<template>
  <!-- eslint-disable-next-line vue/no-v-html -- output is escaped by markdown-it -->
  <div class="md" v-html="html" />
</template>

<style scoped>
.md :deep(p) {
  margin: 0 0 8px;
}
.md :deep(p:last-child) {
  margin-bottom: 0;
}
.md :deep(pre) {
  background: var(--ah-bg-sunken);
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  padding: 10px 12px;
  overflow: auto;
  font-size: 12.5px;
  line-height: 1.55;
  margin: 8px 0;
}
.md :deep(code) {
  font-family: 'Cascadia Code', Consolas, monospace;
}
.md :deep(:not(pre) > code) {
  background: var(--ah-bg-sunken);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 12.5px;
}
.md :deep(ul),
.md :deep(ol) {
  margin: 6px 0;
  padding-left: 20px;
}
.md :deep(table) {
  border-collapse: collapse;
  margin: 8px 0;
}
.md :deep(td),
.md :deep(th) {
  border: 1px solid var(--ah-border);
  padding: 4px 9px;
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
</style>
