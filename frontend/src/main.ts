import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import * as Icons from '@element-plus/icons-vue'

import App from './App.vue'
import router from './router'
import { applyTheme } from './stores/app'
import './styles/theme.css'

applyTheme()

const app = createApp(App)
app.use(ElementPlus)
app.use(router)
for (const [name, comp] of Object.entries(Icons)) {
  app.component(name, comp as any)
}
app.mount('#app')
