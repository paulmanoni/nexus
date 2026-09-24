// Tests for the nexus-hot half of nexus-vite-plugin.js: the hot file the
// Go side reads (internal/vitehot), the manifest enforcement, the input
// option and the dev origin placeholder. No Vite needed — the hooks are
// driven with the same shapes Vite passes.
//
//   node --test client/ui/

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'

import nexus from './nexus-vite-plugin.js'

const PLUGIN_URL = new URL('./nexus-vite-plugin.js', import.meta.url).href
const PLACEHOLDER = '__nexus_vite_placeholder__'

function hot(options) {
  return nexus(options).find((p) => p.name === 'nexus-hot')
}

function tmpRoot(t) {
  const dir = mkdtempSync(join(tmpdir(), 'nexus-hot-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  return dir
}

function logger() {
  const l = { warns: [], infos: [] }
  l.warn = (m) => l.warns.push(m)
  l.info = (m) => l.infos.push(m)
  return l
}

function resolvedConfig(root, { input, base = '/', outDir = 'dist', lib = false } = {}) {
  return {
    root,
    base,
    build: { outDir, rollupOptions: input === undefined ? {} : { input }, lib },
    logger: logger(),
  }
}

function mockServer({ resolvedUrls = null, address = { address: '127.0.0.1', port: 5173 }, https = false } = {}) {
  const httpServer = new EventEmitter()
  httpServer.address = () => address
  const invalidated = []
  const graph = {
    getModuleById: (id) => ({ id }),
    invalidateModule: (m) => invalidated.push(m.id),
  }
  const server = {
    httpServer,
    resolvedUrls: null,
    config: { server: { https } },
    moduleGraph: graph,
    invalidated,
    // Same order as Vite: the socket's 'listening' fires first, then
    // listen() resolves the URLs.
    async listen() {
      httpServer.emit('listening')
      server.resolvedUrls = resolvedUrls
      return server
    },
  }
  return server
}

// start runs a plugin instance through config → configResolved →
// configureServer → listen, the way `vite` does.
async function start(t, { options = {}, userConfig = {}, cfg = {}, server: serverOpts, root = tmpRoot(t) } = {}) {
  const plugin = hot(options)
  const out = plugin.config(userConfig, { command: 'serve', mode: 'development' })
  const input = cfg.input ?? out?.build?.rollupOptions?.input
  const rc = resolvedConfig(root, { ...cfg, input })
  plugin.configResolved(rc)
  const server = mockServer(serverOpts)
  plugin.configureServer(server)
  await server.listen()
  const file = join(root, rc.build.outDir, '.vite', 'nexus-hot.json')
  return { root, plugin, server, file, rc, out }
}

test('dev writes the hot file with the resolved origin, base and entries', async (t) => {
  const { file, server } = await start(t, {
    options: { input: 'src/main.ts' },
    cfg: { base: '/app/' },
    server: {
      address: { address: '127.0.0.1', port: 5174 },
      resolvedUrls: { local: ['http://localhost:5174/app/'], network: [] },
    },
  })
  const got = JSON.parse(readFileSync(file, 'utf8'))
  assert.deepEqual(got, {
    version: 1,
    origin: 'http://127.0.0.1:5174',
    base: '/app/',
    entries: ['src/main.ts'],
    pid: process.pid,
  })
  assert.deepEqual(Object.keys(got), ['version', 'origin', 'base', 'entries', 'pid'])
  server.httpServer.emit('close')
  assert.equal(existsSync(file), false, 'close removes the hot file')
})

test('default entry is index.html; object inputs become root-relative entries', async (t) => {
  const a = await start(t)
  assert.deepEqual(JSON.parse(readFileSync(a.file, 'utf8')).entries, ['index.html'])
  a.server.httpServer.emit('close')

  const root = tmpRoot(t)
  const b = await start(t, { root, cfg: { input: { app: join(root, 'src', 'app.ts'), admin: 'src/admin.ts' } } })
  assert.deepEqual(JSON.parse(readFileSync(b.file, 'utf8')).entries, ['src/app.ts', 'src/admin.ts'])
  b.server.httpServer.emit('close')
})

test('origin falls back to the socket address, mapping wildcards and bracketing IPv6', async (t) => {
  const cases = [
    [{ address: '0.0.0.0', port: 5175 }, false, 'http://127.0.0.1:5175'],
    [{ address: '::', port: 5176 }, false, 'http://127.0.0.1:5176'],
    [{ address: '::1', port: 5177 }, true, 'https://[::1]:5177'],
  ]
  for (const [address, https, want] of cases) {
    const { file, server } = await start(t, { server: { address, https } })
    assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, want)
    server.httpServer.emit('close')
  }
})

test('an explicit server.origin is honoured and not replaced by the placeholder', async (t) => {
  const { file, out, server } = await start(t, {
    userConfig: { server: { origin: 'http://dev.test:9000/' } },
    server: { resolvedUrls: { local: ['http://localhost:5173/'], network: [] } },
  })
  assert.equal(out.server, undefined)
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://dev.test:9000')
  server.httpServer.emit('close')
})

test('close leaves a hot file another process owns', async (t) => {
  const { file, server } = await start(t)
  const other = { ...JSON.parse(readFileSync(file, 'utf8')), pid: process.pid + 100000 }
  writeFileSync(file, JSON.stringify(other))
  server.httpServer.emit('close')
  assert.equal(existsSync(file), true)
})

test('placeholder: set for dev only, swapped for the real origin in transform', async (t) => {
  const p = hot()
  assert.equal(p.config({}, { command: 'serve' }).server.origin, PLACEHOLDER)
  assert.equal(hot().config({}, { command: 'build' }).server, undefined)
  assert.equal(hot().config({}, { command: 'serve', isPreview: true }).server, undefined)
  assert.equal(hot().config({ server: { middlewareMode: true } }, { command: 'serve' }).server, undefined)

  // A module transformed before listen (server.warmup) keeps the
  // placeholder for now and is invalidated once the port is known.
  const root = tmpRoot(t)
  const plugin = hot()
  plugin.config({}, { command: 'serve' })
  plugin.configResolved(resolvedConfig(root))
  const server = mockServer({
    address: { address: '127.0.0.1', port: 5180 },
    resolvedUrls: { local: ['http://localhost:5180/'], network: [] },
  })
  plugin.configureServer(server)
  const css = `body{background:url('${PLACEHOLDER}/src/a.png')}`
  assert.equal(plugin.transform(css, '/src/early.css'), null)
  await server.listen()
  assert.deepEqual(server.invalidated, ['/src/early.css'])
  assert.equal(plugin.transform(css, '/src/a.css').code, `body{background:url('http://127.0.0.1:5180/src/a.png')}`)
  assert.equal(plugin.transform('no marker', '/src/b.ts'), null)
  server.httpServer.emit('close')
})

test('build forces the manifest and warns when it had to', () => {
  const build = (userConfig) => {
    const p = hot()
    const out = p.config(userConfig, { command: 'build', mode: 'production' })
    const l = logger()
    p.configResolved({ ...resolvedConfig('/r'), logger: l })
    return { manifest: out.build?.manifest, warns: l.warns }
  }
  assert.deepEqual(build({}), { manifest: true, warns: [] })
  const off = build({ build: { manifest: false } })
  assert.equal(off.manifest, true)
  assert.match(off.warns[0], /manifest: false overridden/)
  const custom = build({ build: { manifest: 'm.json' } })
  assert.equal(custom.manifest, undefined)
  assert.match(custom.warns[0], /reads \.vite\/manifest\.json/)
  assert.deepEqual(build({ build: { manifest: '.vite/manifest.json' } }), { manifest: undefined, warns: [] })
  assert.equal(hot().config({ build: { ssr: true } }, { command: 'build' }).build, undefined)
  assert.equal(hot().config({}, { command: 'serve' }).build, undefined)
})

test('input sets rollupOptions.input unless one is set; a conflict warns once', () => {
  assert.deepEqual(hot({ input: ['src/main.ts'] }).config({}, { command: 'build' }).build.rollupOptions, { input: ['src/main.ts'] })

  const same = hot({ input: 'src/main.ts' })
  assert.equal(same.config({ build: { rollupOptions: { input: 'src/main.ts' } } }, { command: 'serve' }).build, undefined)

  const p = hot({ input: 'src/main.ts' })
  const out = p.config({ build: { rollupOptions: { input: 'src/app.ts' } } }, { command: 'serve' })
  assert.equal(out.build, undefined)
  const l = logger()
  p.configResolved({ ...resolvedConfig('/r'), logger: l })
  assert.equal(l.warns.length, 1)
  assert.match(l.warns[0], /differs from/)
})

test('build start removes a stale hot file and keeps a live one', (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  const run = (pid) => {
    mkdirSync(join(root, 'dist', '.vite'), { recursive: true })
    writeFileSync(file, JSON.stringify({ version: 1, origin: 'http://localhost:5173', pid }))
    const p = hot()
    p.config({}, { command: 'build' })
    p.configResolved(resolvedConfig(root))
    const warns = []
    p.buildStart.call({ warn: (m) => warns.push(m) })
    return warns
  }
  // A pid that cannot exist.
  assert.deepEqual(run(2 ** 30), [])
  assert.equal(existsSync(file), false)
  // The test runner's parent is alive: its file belongs to a running server.
  assert.equal(run(process.ppid).length, 1)
  assert.equal(existsSync(file), true)
})

// The signal paths run in a child so a real signal can be delivered.
function child(t, body) {
  const root = tmpRoot(t)
  const script = `
    import nexus from ${JSON.stringify(PLUGIN_URL)}
    import { EventEmitter } from 'node:events'
    import { existsSync } from 'node:fs'
    const root = ${JSON.stringify(root)}
    const p = nexus().find((x) => x.name === 'nexus-hot')
    p.config({}, { command: 'serve' })
    p.configResolved({ root, base: '/', build: { outDir: 'dist', rollupOptions: {} }, logger: { warn() {}, info() {} } })
    const httpServer = new EventEmitter()
    httpServer.address = () => ({ address: '127.0.0.1', port: 5173 })
    const server = { httpServer, resolvedUrls: null, config: { server: {} }, moduleGraph: null,
      async listen() { httpServer.emit('listening'); return server } }
    p.configureServer(server)
    await server.listen()
    const file = root + '/dist/.vite/nexus-hot.json'
    if (!existsSync(file)) { console.log('NOT WRITTEN'); process.exit(3) }
    setInterval(() => {}, 1000)
    ${body}
  `
  const res = spawnSync(process.execPath, ['--input-type=module', '-e', script], { encoding: 'utf8', timeout: 10000 })
  return { res, file: join(root, 'dist', '.vite', 'nexus-hot.json') }
}

for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  test(`${sig} removes the hot file and still kills the process`, { skip: process.platform === 'win32' }, (t) => {
    const { res, file } = child(t, `process.kill(process.pid, '${sig}')`)
    assert.equal(res.signal, sig, res.stdout + res.stderr)
    assert.equal(existsSync(file), false)
  })
}

test('a signal someone else handles is left to them', { skip: process.platform === 'win32' }, (t) => {
  const { res, file } = child(t, `
    process.on('SIGINT', () => { console.log('app handler ran'); process.exit(7) })
    process.kill(process.pid, 'SIGINT')
  `)
  assert.equal(res.status, 7, res.stdout + res.stderr)
  assert.match(res.stdout, /app handler ran/)
  assert.equal(existsSync(file), false)
})

test('a normal exit removes the hot file', (t) => {
  const { res, file } = child(t, `process.exit(0)`)
  assert.equal(res.status, 0, res.stdout + res.stderr)
  assert.equal(existsSync(file), false)
})

// Reproduces a collision seen on a real machine: another project's Vite held
// 127.0.0.1:5173, so this one bound [::1]:5173 and Vite reported
// "localhost:5173" — a name that reaches either server.
test('origin names the bound address, not the ambiguous localhost', async (t) => {
  const { file, server } = await start(t, {
    server: {
      address: { address: '::1', port: 5173 },
      resolvedUrls: { local: ['http://localhost:5173/'], network: [] },
    },
  })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://[::1]:5173')
  server.httpServer.emit('close')
})

test('an IPv4-mapped bind is written as plain IPv4', async (t) => {
  const { file, server } = await start(t, {
    server: { address: { address: '::ffff:127.0.0.1', port: 5174 } },
  })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://127.0.0.1:5174')
  server.httpServer.emit('close')
})

test('resolvedUrls are used only when the socket gives no address', async (t) => {
  const { file, server } = await start(t, {
    server: { address: null, resolvedUrls: { local: ['http://localhost:5190/'], network: [] } },
  })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://localhost:5190')
  server.httpServer.emit('close')
})
