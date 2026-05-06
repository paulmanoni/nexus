// Nexus client SDK — runtime ESM module served at
// <client-path>/client.js. Single file, no build step, no
// dependencies; works in any modern browser via
// <script type="module"> or import statement.
//
// This file is the placeholder — the full runtime lands in step 4
// of the SDK rollout. It exports a stub NexusClient so consumers
// importing the file today get a clear "not yet implemented"
// signal instead of a 404.

export class NexusClient {
  constructor(opts = {}) {
    this.basePath = opts.basePath ?? ''
    this.manifestPath = opts.manifestPath ?? '/__nexus/client/manifest.json'
  }
  async manifest() {
    const r = await fetch(this.basePath + this.manifestPath)
    if (!r.ok) throw new Error(`nexus: manifest ${r.status}`)
    return r.json()
  }
}

export default NexusClient