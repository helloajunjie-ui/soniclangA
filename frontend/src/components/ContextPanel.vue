<script setup lang="ts">
/**
 * Right-hand context panel for a chat turn (DESIGN §2.1): route decision,
 * tool calls, knowledge hits and memory hits for the current exchange.
 */
export interface PanelSection {
  key: string
  title: string
  icon?: string
  items: { label: string; detail?: string; tone?: 'info' | 'warn' | 'danger' }[]
  empty?: string
}

defineProps<{ sections: PanelSection[] }>()
</script>

<template>
  <div class="ctx">
    <template v-for="sec in sections" :key="sec.key">
      <div class="sec-title">
        <el-icon v-if="sec.icon"><component :is="sec.icon" /></el-icon>
        <span>{{ sec.title }}</span>
        <span class="count">{{ sec.items.length }}</span>
      </div>
      <div v-if="!sec.items.length" class="dim-empty">{{ sec.empty || '本轮无' }}</div>
      <div v-for="(it, i) in sec.items" :key="i" class="row" :class="it.tone">
        <div class="lbl">{{ it.label }}</div>
        <div v-if="it.detail" class="det">{{ it.detail }}</div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.ctx {
  padding: 10px 12px;
  overflow: auto;
}
.sec-title {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--ah-text-dim);
  margin: 12px 0 6px;
}
.sec-title:first-child {
  margin-top: 0;
}
.count {
  margin-left: auto;
  background: var(--ah-bg-sunken);
  border-radius: 999px;
  padding: 0 7px;
  font-size: 11px;
}
.dim-empty {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  opacity: 0.7;
  padding: 2px 0 4px;
}
.row {
  border-left: 2px solid var(--ah-border);
  padding: 3px 0 3px 9px;
  margin-bottom: 5px;
}
.row.warn {
  border-color: var(--ah-warn);
}
.row.danger {
  border-color: var(--ah-danger);
}
.row.info {
  border-color: var(--ah-primary);
}
.lbl {
  font-size: 12.5px;
  word-break: break-word;
}
.det {
  font-size: 11.5px;
  color: var(--ah-text-dim);
  word-break: break-word;
}
</style>
