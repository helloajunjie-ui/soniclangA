import { createRouter, createWebHashHistory } from 'vue-router'

// Hash history: the backend serves the SPA from a static handler and a deep
// link must work after a hard refresh without any server rewrite rules.
const routes = [
  { path: '/', redirect: '/chat' },
  { path: '/chat/:id?', name: 'chat', component: () => import('@/views/ChatView.vue'), meta: { title: '聊天' } },
  { path: '/roles', name: 'roles', component: () => import('@/views/RolesView.vue'), meta: { title: '角色库' } },
  { path: '/skills', name: 'skills', component: () => import('@/views/SkillsView.vue'), meta: { title: '技能库' } },
  { path: '/methodology', name: 'methodology', component: () => import('@/views/MethodologyView.vue'), meta: { title: '方法论' } },
  { path: '/knowledge', name: 'knowledge', component: () => import('@/views/KnowledgeView.vue'), meta: { title: '资料库' } },
  { path: '/memory', name: 'memory', component: () => import('@/views/MemoryView.vue'), meta: { title: '记忆' } },
  { path: '/providers', name: 'providers', component: () => import('@/views/ProvidersView.vue'), meta: { title: '服务商' } },
  { path: '/tools', name: 'tools', component: () => import('@/views/ToolsView.vue'), meta: { title: '工具' } },
  { path: '/control', name: 'control', component: () => import('@/views/ControlView.vue'), meta: { title: '软件控制' } },
  // 「能力」页已删除：它只是把资料库和记忆的功能各抄了一份，开关回到各自的页面。
  // 留一条重定向，免得旧链接打开是空白页。
  { path: '/modules', redirect: '/knowledge' },
  { path: '/settings', name: 'settings', component: () => import('@/views/SettingsView.vue'), meta: { title: '设置' } },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
