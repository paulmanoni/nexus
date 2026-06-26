<script setup lang="ts">
import { Link } from '@inertiajs/vue3'

// Props come straight from NewUsers in ../pages/pages.go — no client API call.
defineProps<{ users: string[] }>()
</script>

<template>
  <main>
    <h1>Users</h1>
    <ul>
      <li v-for="u in users" :key="u">{{ u }}</li>
    </ul>
    <p><Link href="/login">Sign in</Link></p>
    <p>Server-rendered via Inertia — edit <code>web/src/Pages/Users/Index.vue</code>;
    props come from <code>NewUsers</code>. <code>nexus dev</code> hot-reloads on save.</p>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
</style>
