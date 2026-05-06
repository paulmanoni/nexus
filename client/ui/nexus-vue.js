// Nexus client SDK — Vue 3 composables served at
// <client-path>/vue.js. Composes on top of nexus-client.js. Plain
// ESM, no build step.
//
// Placeholder until step 6 of the SDK rollout. The exported
// surface is a stub so consumers get a clear failure mode rather
// than a 404 on import.

import { NexusClient } from './client.js'

export function useNexus(opts) {
  return new NexusClient(opts)
}