import { createApp } from 'vue'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import '@vue-flow/minimap/dist/style.css'
import './style.css'
import App from './App.vue'

// Inside the console's Architecture tab (the templ shell owns the header and
// theme), the canvas runs embedded: no header of its own.
const el = document.getElementById('app')
createApp(App, { embedded: el.hasAttribute('data-embedded') }).mount(el)