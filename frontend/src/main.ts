import { createApp } from 'vue'
import { ElLoading } from 'element-plus'
import * as Icons from '@element-plus/icons-vue'

// Element Plus 的组件不再全量注册（见 vite.config.ts 里的 Components 插件）：
// 模板里出现的 <el-xxx> 会被自动按需 import，样式也一并只带用到的那些。
// 但下面几样是插件解析不到、必须手写的：
//
//   1. 服务式 API（ElMessage / ElMessageBox）不出现在模板里，样式要自己引；
//   2. v-loading 是指令，同样不在模板的组件位置，得显式注册。
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import 'element-plus/es/components/loading/style/css'

import App from './App.vue'
import router from './router'
import { applyTheme } from './stores/app'
import './styles/theme.css'

applyTheme()

const app = createApp(App)
app.use(ElLoading)
app.use(router)
// 图标保持全量注册：模板里有 <component :is="'CircleCheck'"> 这类按名字
// 动态取用的情况（ChatView/MessageItem/ContextPanel/ProvidersView），
// 按需引入一旦漏掉某个名字就是一块空白图标，而且只在特定状态才出现。
for (const [name, comp] of Object.entries(Icons)) {
  app.component(name, comp as any)
}
app.mount('#app')
