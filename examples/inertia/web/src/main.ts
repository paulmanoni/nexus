import { createInertiaApp } from '@inertiajs/vue3'
import { createApp, h } from 'vue'

// Page components under src/Pages are resolved by the name the Go handler passes
// to inertia.Page — e.g. "Users/Index" → src/Pages/Users/Index.vue,
// "Auth/Login" → src/Pages/Auth/Login.vue (see ../pages/pages.go).
createInertiaApp({
  resolve: (name) => {
    const pages = import.meta.glob('./Pages/**/*.vue', { eager: true })
    return pages['./Pages/' + name + '.vue']
  },
  setup({ el, App, props, plugin }) {
    createApp({ render: () => h(App, props) }).use(plugin).mount(el)
  },
})
