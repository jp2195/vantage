import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { PiniaColada } from '@pinia/colada'
import App from './App.vue'
import { router } from './router'
import './styles/tokens.css'

const app = createApp(App)
app.use(createPinia())
// Query caching for the generated SDK calls every later screen makes.
// Installed here, once, for the same reason auth.ts and client.ts are
// singletons: one cache, not one per component that happens to fetch data.
app.use(PiniaColada)
app.use(router)
app.mount('#app')
