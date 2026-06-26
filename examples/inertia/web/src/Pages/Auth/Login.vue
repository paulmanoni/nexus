<script setup lang="ts">
import { useForm } from '@inertiajs/vue3'

// title comes from NewLogin (GET). The form POSTs to the same /login route,
// which the Go handler answers with inertia.Redirect("/users"). useForm reads
// page.props.errors automatically, so adding inertia.Invalid(...) on the server
// would surface here as form.errors without any client change.
defineProps<{ title: string }>()

const form = useForm({ email: '', password: '' })

function submit() {
  form.post('/login')
}
</script>

<template>
  <main>
    <h1>{{ title }}</h1>
    <form @submit.prevent="submit">
      <label>
        Email
        <input v-model="form.email" type="email" />
        <small v-if="form.errors.email">{{ form.errors.email }}</small>
      </label>
      <label>
        Password
        <input v-model="form.password" type="password" />
        <small v-if="form.errors.password">{{ form.errors.password }}</small>
      </label>
      <button type="submit" :disabled="form.processing">Sign in</button>
    </form>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
label { display: block; margin: 0.75rem 0; }
small { color: #c00; display: block; }
</style>
