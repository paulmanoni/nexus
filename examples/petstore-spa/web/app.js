// Petstore SPA — Vue setup-script demo of the nexus client SDK.
// Plain ESM; resolved by the browser via the importmap in
// index.html. No build step required.
//
// What this exercises:
//   - useAuth(): reactive auth state + login/logout/me actions
//   - useCrud('pets'): list + create + delete with optimistic
//     updates, fetched once on mount
//   - The SDK's typed Bearer-token attachment — set once on login,
//     auto-applied to every CRUD call
//
// Live updates via WS are turned off for this demo (the demo's
// Pet store is in-memory and doesn't broadcast). Apps that emit
// pets.created / pets.updated / pets.deleted via AsWS get reactive
// list updates automatically by removing { subscribe: false } below.

import { createApp, ref } from 'vue'
import { useAuth, useCrud } from '/__nexus/client/vue.js'

const App = {
  setup() {
    const auth = useAuth()
    const pets = useCrud('pets', { subscribe: false })
    const newName = ref('')
    const credentials = ref({ username: 'alice', password: 'hunter2' })

    async function add() {
      const name = newName.value.trim()
      if (!name) return
      try {
        await pets.create({ name })
        newName.value = ''
      } catch (e) {
        // useCrud doesn't reach into errors — the underlying SDK
        // call throws NexusError; surface it on screen.
        alert(`create failed: ${e.message}`)
      }
    }

    async function login() {
      try {
        await auth.login(credentials.value)
        // After login, the CRUD list needs a fresh fetch with the
        // new auth header attached. (useCrud's initial fetch fired
        // before login; refresh now that we have a token.)
        await pets.refresh()
      } catch (e) {
        // auth.error.value is also populated; we surface via the
        // template below. Empty catch keeps focus on auth state.
      }
    }

    async function logout() {
      await auth.logout()
      pets.items.value = []
    }

    return { auth, pets, newName, credentials, add, login, logout }
  },

  template: `
    <div v-if="auth.loading.value" class="loading">Loading…</div>

    <form v-else-if="!auth.isAuthenticated.value" @submit.prevent="login">
      <h1>Petstore</h1>
      <p class="muted">Sign in with <code>alice</code> / <code>hunter2</code>.</p>
      <input v-model="credentials.username" placeholder="Username" autofocus />
      <input v-model="credentials.password" type="password" placeholder="Password" />
      <button type="submit">Sign in</button>
      <p v-if="auth.error.value" class="error">
        {{ auth.error.value.payload?.error || auth.error.value.message }}
      </p>
    </form>

    <div v-else>
      <header>
        <h1>Pets <small class="muted">({{ pets.items.value.length }})</small></h1>
        <button class="ghost" @click="logout">
          Sign out {{ auth.identity.value?.name || auth.identity.value?.id || '' }}
        </button>
      </header>

      <p v-if="pets.error.value" class="error">{{ pets.error.value.message }}</p>
      <p v-if="pets.loading.value && !pets.items.value.length" class="loading">Loading pets…</p>

      <ul v-if="pets.items.value.length">
        <li v-for="p in pets.items.value" :key="p.id">
          <span>{{ p.name }} <small class="muted" v-if="p.age">({{ p.age }})</small></span>
          <button class="icon" @click="pets.remove(p.id)" title="Delete">×</button>
        </li>
      </ul>
      <p v-else-if="!pets.loading.value" class="muted">No pets yet.</p>

      <div class="row">
        <input v-model="newName" @keyup.enter="add" placeholder="Add a pet…" />
        <button @click="add">Add</button>
      </div>
    </div>
  `,
}

createApp(App).mount('#app')
