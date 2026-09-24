// Tests for the nexus-hot half of nexus-vite-plugin.js: the hot file the
// Go side reads (internal/vitehot), the manifest enforcement, the input
// option and the dev origin placeholder. No Vite needed — the hooks are
// driven with the same shapes Vite passes.
//
//   node --test client/ui/

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, existsSync, rmSync, readdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { spawnSync, spawn } from 'node:child_process'

import nexus from './nexus-vite-plugin.js'

const PLUGIN_URL = new URL('./nexus-vite-plugin.js', import.meta.url).href
const PLACEHOLDER = '__nexus_vite_placeholder__'
const IFACES = Symbol.for('nexus-vite-plugin.networkInterfaces')

// withInterfaces runs the rest of test t with os.networkInterfaces replaced.
function withInterfaces(t, ifaces) {
  globalThis[IFACES] = () => ifaces
  t.after(() => { delete globalThis[IFACES] })
}

const LAN = {
  lo0: [
    { address: '127.0.0.1', family: 'IPv4', internal: true },
    { address: '::1', family: 'IPv6', internal: true },
  ],
  en0: [
    { address: 'fe80::1c2b:3aff:fe4d:5e6f', family: 'IPv6', internal: false, scopeid: 4 },
    { address: '192.168.1.5', family: 'IPv4', internal: false },
  ],
}

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
  withInterfaces(t, { lo0: LAN.lo0 })
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
  assert.equal(out.server.origin, undefined)
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
  assert.equal(hot().config({ server: { middlewareMode: true } }, { command: 'serve' }).server.origin, undefined)

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

// ── M5: wildcard binds and cross-origin module loading ─────────────────

// `vite --host` binds every interface. 127.0.0.1 would send a phone on the
// LAN to itself; the machine's network address works for the phone and the
// machine alike.
test('a wildcard bind is written as the network address', async (t) => {
  withInterfaces(t, LAN)
  for (const address of ['0.0.0.0', '::']) {
    const { file, server } = await start(t, {
      server: {
        address: { address, port: 5181 },
        resolvedUrls: { local: ['http://localhost:5181/'], network: ['http://10.0.0.7:5181/'] },
      },
    })
    assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://10.0.0.7:5181', 'Vite\'s Network URL first')
    server.httpServer.emit('close')
  }
  // No resolvedUrls (bound outside server.listen): the interfaces decide,
  // skipping loopback, IPv6 and link-local.
  const { file, server, plugin } = await start(t, {
    server: { address: { address: '::', port: 5182 }, resolvedUrls: null },
  })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://192.168.1.5:5182')
  const css = `a{b:url('${PLACEHOLDER}/src/x.png')}`
  assert.equal(plugin.transform(css, '/src/x.css').code, `a{b:url('http://192.168.1.5:5182/src/x.png')}`)
  server.httpServer.emit('close')
})

test('a wildcard bind with no network address falls back to 127.0.0.1', async (t) => {
  withInterfaces(t, {
    lo0: LAN.lo0,
    en1: [{ address: '169.254.10.2', family: 'IPv4', internal: false }],
  })
  const { file, server } = await start(t, { server: { address: { address: '0.0.0.0', port: 5183 } } })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://127.0.0.1:5183')
  server.httpServer.emit('close')
})

test('a specific bind keeps its literal address, even when a network address exists', async (t) => {
  withInterfaces(t, LAN)
  const { file, server } = await start(t, {
    server: {
      address: { address: '::1', port: 5173 },
      resolvedUrls: { local: ['http://localhost:5173/'], network: ['http://192.168.1.5:5173/'] },
    },
  })
  assert.equal(JSON.parse(readFileSync(file, 'utf8')).origin, 'http://[::1]:5173')
  server.httpServer.emit('close')
})

function corsCheck(plugin, userConfig = {}) {
  const out = plugin.config(userConfig, { command: 'serve', mode: 'development' })
  const origin = out.server && out.server.cors && out.server.cors.origin
  return {
    out,
    allows: (o) => {
      let got
      origin(o, (err, v) => { assert.equal(err, null); got = v })
      return got
    },
  }
}

test('dev CORS allows this machine and local names, on any port, and nothing else', (t) => {
  withInterfaces(t, LAN)
  const { allows } = corsCheck(hot({ appOrigin: ['https://staging.example.com/', 'http://10.1.2.3:9000'] }))
  for (const o of [
    'http://localhost:8080', 'https://localhost', 'http://app.localhost:3000',
    'http://127.0.0.1:8080', 'http://127.1.2.3:8080', 'http://[::1]:8080',
    'http://myapp.test:8080', 'https://admin.myapp.test',
    'http://192.168.1.5:8080', 'http://[fe80::1c2b:3aff:fe4d:5e6f]:8080',
    'https://staging.example.com', 'http://10.1.2.3:9000',
  ]) assert.equal(allows(o), true, o)
  for (const o of [
    'https://evil.example', 'http://192.168.1.6:8080', 'http://localhost.evil.example',
    'http://test', 'http://mytest:8080', 'http://10.1.2.3:9001', 'https://staging.example.com:8443',
    'null', '', undefined, 'file:///etc/passwd', 'chrome-extension://abc',
  ]) assert.equal(allows(o), false, String(o))
})

test('dev CORS is left alone when the app configures it, and never set for build or preview', () => {
  const p = hot({ appOrigin: 'http://x.example' })
  const out = p.config({ server: { cors: false } }, { command: 'serve' })
  assert.equal(out.server.cors, undefined)
  const l = logger()
  p.configResolved({ ...resolvedConfig('/r'), logger: l })
  assert.match(l.warns[0], /appOrigin.*ignored because server\.cors is set/)

  assert.equal(hot().config({ server: { cors: { origin: '*' } } }, { command: 'serve' }).server.cors, undefined)
  assert.equal(hot().config({}, { command: 'build' }).server, undefined)
  assert.equal(hot().config({}, { command: 'serve', isPreview: true }).server, undefined)
})

// ── M1: a build into a running dev server's outDir ──────────────────────

// buildInto drives one plugin instance through the build hooks. The caller
// performs Vite's emptyOutDir between them.
function buildInto(root) {
  const p = hot()
  p.config({}, { command: 'build', mode: 'production' })
  const rc = resolvedConfig(root)
  p.configResolved(rc)
  const warns = []
  const ctx = { warn: (m) => warns.push(m) }
  return {
    warns,
    logger: rc.logger,
    buildStart: () => p.buildStart.call(ctx),
    watchChange: () => p.watchChange.call(ctx, 'src/a.ts', { event: 'update' }),
    renderStart: () => p.renderStart.handler.call(ctx),
    writeBundle: () => p.writeBundle.handler.call(ctx),
    closeBundle: () => p.closeBundle.call(ctx),
  }
}

function writeDevHot(root, pid, port = 5173) {
  const dir = join(root, 'dist', '.vite')
  mkdirSync(dir, { recursive: true })
  const raw = JSON.stringify({ version: 1, origin: `http://127.0.0.1:${port}`, base: '/', entries: ['src/main.ts'], pid }, null, 2) + '\n'
  writeFileSync(join(dir, 'nexus-hot.json'), raw)
  return raw
}

// Vite's emptyOutDir: everything under outDir goes, .vite included.
const emptyOutDir = (root) => rmSync(join(root, 'dist'), { recursive: true, force: true })

test('a build puts back the hot file of a running dev server after emptyOutDir', (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  const raw = writeDevHot(root, process.ppid)
  const b = buildInto(root)
  b.buildStart()
  assert.equal(b.warns.length, 1)
  assert.match(b.warns[0], /is restored after the build empties the outDir/)
  emptyOutDir(root)                       // Vite: prepareOutDir, just before writing
  b.renderStart()
  assert.equal(readFileSync(file, 'utf8'), raw, 'restored byte for byte')
  assert.deepEqual(readdirNames(join(root, 'dist', '.vite')), ['nexus-hot.json'], 'no temp file left')
  b.writeBundle()
  b.closeBundle()
  assert.equal(readFileSync(file, 'utf8'), raw)
})

test('a later hook restores when the wipe came after renderStart', (t) => {
  const root = tmpRoot(t)
  const raw = writeDevHot(root, process.ppid)
  const b = buildInto(root)
  b.buildStart()
  b.renderStart()
  emptyOutDir(root)
  b.closeBundle()
  assert.equal(readFileSync(join(root, 'dist', '.vite', 'nexus-hot.json'), 'utf8'), raw)
})

test('watch mode: a rebuild wipes before buildStart, which restores; warns once', (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  const raw = writeDevHot(root, process.ppid)
  const b = buildInto(root)
  b.buildStart(); emptyOutDir(root); b.renderStart(); b.closeBundle()
  // The dev server restarted on a new port between builds.
  const raw2 = writeDevHot(root, process.ppid, 5199)
  b.watchChange()
  emptyOutDir(root)                       // watch mode: BUNDLE_START wipes first
  b.buildStart()
  assert.equal(readFileSync(file, 'utf8'), raw2, 'the newer file, captured at watchChange')
  assert.notEqual(raw, raw2)
  assert.equal(b.warns.length, 1)
})

test('a hot file the dev server rewrote after the wipe is not overwritten', (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  writeDevHot(root, process.ppid)
  const b = buildInto(root)
  b.buildStart()
  emptyOutDir(root)
  const fresh = writeDevHot(root, process.ppid, 5200)
  b.renderStart(); b.closeBundle()
  assert.equal(readFileSync(file, 'utf8'), fresh)
})

test('no restore once the dev server has exited', async (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  const dev = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' })
  await new Promise((r) => dev.once('spawn', r))
  writeDevHot(root, dev.pid)
  const b = buildInto(root)
  b.buildStart()
  assert.equal(b.warns.length, 1)
  dev.kill('SIGKILL')
  await new Promise((r) => dev.once('exit', r))
  emptyOutDir(root)
  b.renderStart(); b.writeBundle(); b.closeBundle()
  assert.equal(existsSync(file), false)
})

test('a build removes a stale or malformed hot file and restores nothing', (t) => {
  const root = tmpRoot(t)
  const file = join(root, 'dist', '.vite', 'nexus-hot.json')
  for (const content of [JSON.stringify({ version: 1, pid: 2 ** 30 }), '{not json']) {
    mkdirSync(dirname(file), { recursive: true })
    writeFileSync(file, content)
    const b = buildInto(root)
    b.buildStart()
    assert.equal(existsSync(file), false)
    assert.deepEqual(b.warns, [])
    b.renderStart(); b.closeBundle()
    assert.equal(existsSync(file), false)
  }
})

test('a build in the dev server\'s own process keeps that server\'s hot file', async (t) => {
  const { root, file, server } = await start(t)
  const raw = readFileSync(file, 'utf8')
  const b = buildInto(root)
  b.buildStart()
  emptyOutDir(root)
  b.renderStart()
  assert.equal(readFileSync(file, 'utf8'), raw)
  server.httpServer.emit('close')
  assert.equal(existsSync(file), false)
})

function readdirNames(dir) {
  return readdirSync(dir).sort()
}
