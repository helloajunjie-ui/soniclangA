<script setup lang="ts">
import { ref } from 'vue'

defineProps<{
  /** Tool name shown as the title. */
  name: string
  /** Optional one-line summary shown collapsed. */
  summary?: string
  /** Result or error text. */
  output?: string
  failed?: boolean
}>()

const open = ref(false)
</script>

<template>
  <div class="tool" :class="{ failed }">
    <div class="head" @click="open = !open">
      <el-icon class="caret" :class="{ open }"><ArrowRight /></el-icon>
      <el-icon class="ic"><Tools /></el-icon>
      <span class="name">{{ name }}</span>
      <span v-if="summary && !open" class="summary">{{ summary }}</span>
      <el-tag v-if="failed" size="small" type="danger" effect="plain">失败</el-tag>
    </div>
    <pre v-if="open && output" class="out">{{ output }}</pre>
  </div>
</template>

<style scoped>
.tool {
  border: 1px solid var(--ah-border);
  border-radius: 8px;
  margin: 6px 0;
  background: var(--ah-bg-sunken);
  font-size: 12.5px;
}
.tool.failed {
  border-color: var(--ah-danger);
}
.head {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 6px 10px;
  cursor: pointer;
  user-select: none;
}
.caret {
  transition: transform 0.15s;
  font-size: 11px;
}
.caret.open {
  transform: rotate(90deg);
}
.ic {
  color: var(--ah-primary);
}
.name {
  font-weight: 500;
}
.summary {
  color: var(--ah-text-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 380px;
}
.out {
  margin: 0;
  padding: 8px 12px;
  border-top: 1px solid var(--ah-border);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 340px;
  overflow: auto;
  font-family: 'Cascadia Code', Consolas, monospace;
  color: var(--ah-text-dim);
}
</style>
