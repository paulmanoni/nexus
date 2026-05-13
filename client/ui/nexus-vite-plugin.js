// nexus-vite-plugin.js — four plugins in one factory:
//
//   1. nexus-auto-select   (default-on, all builds)
//      Auto-injects opts.select into nx.query / nx.mutate calls
//      based on the property accesses the surrounding code makes on
//      the result variable. Without it, the SDK auto-walker fetches
//      every field reachable from the operation's return type up to
//      depth 3 — safe but over-fetches. With it, every typed call
//      rewrites at build time to fetch exactly the fields the
//      consumer reads.
//
//   2. nexus-manifest-filter   (opt-in via options.filter: 'usage')
//      Walks the source tree at buildStart, collects every literal
//      endpoint reference (nx.query/mutate/crud/rest/ws), then
//      intercepts the import of sdk/manifest.json via vite's `load`
//      hook and returns a projected manifest containing only the
//      used endpoints + their reachable ref types + auth flows.
//      Apply: 'build' — inactive in dev, where the runtime fetch
//      keeps the full manifest live for HMR + endpoint discovery.
//      Loose mode (default) tolerates dynamic calls (varName as the
//      first arg) by including everything in that build with a
//      warning; strict mode errors out unless every dynamic call
//      has a `// @nexus-include foo, bar` pragma above it.
//
//   3. nexus-loop-guard   (default-on, build-only)
//      Snapshots auto-imports.d.ts / components.d.ts at buildStart,
//      restores the mtime in closeBundle when contents are
//      unchanged. Breaks the rebuild loop unplugin-auto-import
//      causes by writing identical bytes on every build.
//
//   4. nexus-hmr   (default-on, dev-only)
//      Watches the typed-codegen tree (web/src/__nexus by default;
//      configurable via options.codegenDir) and fires a full-reload
//      on every file write. Pairs with `nexus dev`'s auto-codegen so
//      editing a Go endpoint propagates to the browser without a
//      manual refresh — the user's frontend just has to be running
//      under vite dev with the nexus plugin attached.
//
// Wire it up in vite.config.ts:
//
//     import nexusAutoSelect from './src/sdk/nexus-vite-plugin.js'
//
//     export default defineConfig({
//       plugins: [vue(), nexusAutoSelect()],
//     })
//
// Peer deps the plugin uses (already in any Vue+TS project):
//   - typescript          (AST walking)
//   - magic-string        (source mutation with sourcemaps)
//   - @vue/compiler-sfc   (script-setup extraction; comes with @vue)
//
// v0 scope:
//   ✓ const|let res = await nx.{query|mutate}('opname', vars [, opts])
//   ✓ res.x.y.z accesses (deep, optional chain, non-null) in same fn body
//   ✓ skips the call if opts.select is already provided
//   ✓ .ts / .js / .tsx / .jsx files
//   ✓ <script setup lang="ts"> blocks in .vue
//   ✗ template-only access (defer; document workaround = explicit select)
//   ✗ destructuring (defer; document workaround = direct access)
//   ✗ cross-function flow (defer)

import { readFileSync, existsSync, statSync, utimesSync, readdirSync } from 'node:fs'
import { join, isAbsolute, resolve } from 'node:path'

const DEFAULT_SDK_DIR = 'src/sdk'
const MANIFEST = 'manifest.json'

// LOOP_GUARD_TARGETS are basenames of files that auto-import plugins
// (unplugin-auto-import, unplugin-vue-components — both shipped by
// @nuxt/ui's vite plugin) re-write at the end of every vite build
// with identical bytes. Each unconditional re-write bumps the mtime,
// chokidar fires a "change" event, rollup rebuilds, the plugin
// re-writes, ad infinitum. Rollup's `build.watch.exclude` does NOT
// suppress files plugins add via this.addWatchFile().
//
// We break the cycle by snapshotting these files' bytes + mtime at
// the start of each build and, in `closeBundle`, restoring the
// mtime when the bytes haven't actually changed. chokidar's next
// stat returns the original mtime → no change event → no rebuild.
const LOOP_GUARD_TARGETS = ['auto-imports.d.ts', 'components.d.ts']

// Directories the manifest-filter usage walker skips when crawling
// the project root. Hoisted to module scope (instead of declared
// inside the plugin factory) because the recursive walker is invoked
// from a function-declaration that runs after the factory's return
// — `const` inside the factory would hit the TDZ at call time.
const FILTER_SKIP_DIRS = new Set([
  'node_modules', '.git', 'dist', 'build', '.vite', '.cache', '.next', '.nuxt', 'coverage',
])

export default function nexusAutoSelect(options = {}) {
  let ts, MagicString, parseSFC
  let manifest = null
  let manifestPath = ''
  let projectRoot = ''
  // Tracks every module we successfully rewrote. On manifest change
  // we invalidate these so they re-run their transform with the
  // fresh op list — without it, brand-new ops added on the Go side
  // wouldn't get auto-selected until vite dev is restarted.
  const transformedIds = new Set()

  function loadManifest(logger) {
    try {
      const raw = JSON.parse(readFileSync(manifestPath, 'utf8'))
      const ops = new Set()
      for (const e of raw.endpoints || []) {
        if (e.transport === 'graphql') ops.add(e.name)
      }
      manifest = { ops }
      return true
    } catch (e) {
      logger?.warn(`[nexus-auto-select] manifest reload failed: ${e?.message || e}`)
      return false
    }
  }

  // Loop-guard state: per-target { bytes: Buffer, mtime: Date }.
  // Captured at buildStart, consulted at closeBundle to decide
  // whether to restore mtime. Lives on the post-enforce plugin so
  // closeBundle runs AFTER unplugin-auto-import writes its d.ts.
  const loopGuardSnapshots = new Map()
  const loopGuardPaths = () => projectRoot
    ? LOOP_GUARD_TARGETS.map((t) => join(projectRoot, t))
    : []

  const authoringPlugin = {
    name: 'nexus-auto-select',
    enforce: 'pre',

    async configResolved(cfg) {
      projectRoot = cfg.root || process.cwd()
      const sdkDir = options.sdkDir
        ? (isAbsolute(options.sdkDir) ? options.sdkDir : join(projectRoot, options.sdkDir))
        : join(projectRoot, DEFAULT_SDK_DIR)
      manifestPath = join(sdkDir, MANIFEST)
      if (!existsSync(manifestPath)) {
        cfg.logger.warn(`[nexus-auto-select] manifest not found at ${manifestPath} — plugin disabled`)
        return
      }
      try {
        ts = (await import('typescript')).default || (await import('typescript'))
      } catch {
        cfg.logger.warn(`[nexus-auto-select] 'typescript' is not installed — plugin disabled`)
        return
      }
      try {
        MagicString = (await import('magic-string')).default || (await import('magic-string'))
      } catch {
        cfg.logger.warn(`[nexus-auto-select] 'magic-string' is not installed — plugin disabled`)
        return
      }
      try {
        parseSFC = (await import('@vue/compiler-sfc')).parse
      } catch {
        // Optional — the plugin still works on .ts/.js files.
        parseSFC = null
      }
      loadManifest(cfg.logger)
    },

    // Wire the manifest watcher into the dev server. When the Go
    // side re-dumps manifest.json (a struct change, a new op, etc.)
    // we re-read the op list and invalidate every module we've
    // already transformed so HMR picks up the new selection rules.
    configureServer(server) {
      if (!manifestPath) return
      // Vite's chokidar watcher already covers files inside the
      // project root; manifest.json under OutDir typically qualifies,
      // but adding it explicitly is cheap and safe outside the root.
      server.watcher.add(manifestPath)
      const target = resolve(manifestPath)
      const onChange = (file) => {
        if (resolve(file) !== target) return
        const ok = loadManifest(server.config.logger)
        if (!ok) return
        const graph = server.moduleGraph
        let invalidated = 0
        for (const id of transformedIds) {
          const mod = graph.getModuleById(id)
          if (mod) {
            graph.invalidateModule(mod)
            invalidated++
          }
        }
        // A full reload is the cheapest correct thing here — a new
        // op list might affect any number of modules, and partial
        // HMR on rewritten code is fragile.
        server.ws.send({ type: 'full-reload' })
        server.config.logger.info(
          `[nexus-auto-select] manifest reloaded, ${invalidated} module(s) invalidated`,
        )
      }
      server.watcher.on('change', onChange)
      server.watcher.on('add', onChange)
    },

    transform(code, id) {
      if (!manifest || !ts || !MagicString) return null
      // Skip the SDK directory itself + node_modules.
      if (id.includes('/node_modules/')) return null
      if (id.includes('/sdk/')) return null

      let result = null
      if (/\.(t|j)sx?$/.test(id)) {
        result = transformScript(code, id, /*isVueScript=*/false)
      } else if (id.endsWith('.vue') && parseSFC) {
        result = transformVue(code, id)
      }
      if (result) transformedIds.add(id)
      return result
    },
  }

  // Loop-guard plugin runs at enforce: 'post' so its closeBundle
  // hook fires AFTER unplugin-auto-import / unplugin-vue-components
  // have written their d.ts files. We snapshot bytes+mtime at
  // buildStart (still post-enforced, but every plugin's buildStart
  // runs before any plugin's writeBundle/closeBundle) and revert
  // mtime when content is unchanged. chokidar's next stat sees the
  // pre-build mtime → no event → no rebuild loop.
  const loopGuardPlugin = {
    name: 'nexus-loop-guard',
    enforce: 'post',
    apply: 'build', // skip during `vite` (dev server); only matters in build --watch
    configResolved(cfg) {
      // Independent of the authoring plugin: the loop guard is
      // useful even when the auto-select half is disabled (no
      // manifest, no typescript, etc.).
      if (!projectRoot) projectRoot = cfg.root || process.cwd()
    },
    buildStart() {
      loopGuardSnapshots.clear()
      for (const p of loopGuardPaths()) {
        try {
          if (!existsSync(p)) continue
          const bytes = readFileSync(p)
          const { mtime, atime } = statSync(p)
          loopGuardSnapshots.set(p, { bytes, mtime, atime })
        } catch {
          /* file may not yet exist — first build creates it */
        }
      }
    },
    closeBundle() {
      for (const p of loopGuardPaths()) {
        const snap = loopGuardSnapshots.get(p)
        if (!snap) continue
        try {
          if (!existsSync(p)) continue
          const after = readFileSync(p)
          if (after.length === snap.bytes.length && after.equals(snap.bytes)) {
            utimesSync(p, snap.atime, snap.mtime)
          }
        } catch {
          /* best effort — never fail the build */
        }
      }
    },
  }

  // ── manifest filter (mode 1: usage-driven projection) ────────────
  //
  // When options.filter === 'usage', this plugin walks every source
  // file at buildStart, collecting literal endpoint references from
  // nx.query/mutate/crud/rest/ws calls. At `load` time it intercepts
  // the import of sdk/manifest.json and returns a filtered subset:
  // only the endpoints actually referenced + the ref types reachable
  // from their args/return TypeRefs + the auth flows.
  //
  // Goal: shrink the schema slice that ends up in the production
  // bundle to just what the app uses. Threat model is "schema visible
  // inside the JS bundle": this is the lever that reduces it without
  // going to full compile-time inlining.
  //
  // Inactive in dev (`apply: 'build'`) — dev keeps the full manifest
  // for HMR + autocomplete + endpoint discoverability. Inactive when
  // typescript isn't installed (the AST walker can't run) — same
  // graceful degradation as the auto-select plugin.
  //
  // Loose mode (default): a dynamic call (`nx.query(varName, ...)`)
  // disables the filter for that build and warns. Strict mode
  // (`filterMode: 'strict'`) errors out unless every dynamic call
  // has a `// @nexus-include foo, bar` pragma immediately above it.
  const filterMode = options.filter || 'off'
  const filterStrict = options.filterMode === 'strict'
  const scanInclude = options.scanInclude || ['src']
  const usedGqlNames = new Set()       // 'listUsers' (query + mutation)
  const usedRestRoutes = new Set()     // 'GET /users/:id'
  const usedCrudEntities = new Set()   // 'pets' → keeps every REST route under /pets
  const usedWsPaths = new Set()        // '/events'
  let filterDynamicSeen = false

  const manifestFilterPlugin = {
    name: 'nexus-manifest-filter',
    enforce: 'pre',
    apply: 'build',

    async buildStart() {
      if (filterMode !== 'usage') return
      if (!ts) return
      if (!projectRoot) return

      const start = Date.now()
      let scanned = 0
      for (const root of scanInclude) {
        const dir = isAbsolute(root) ? root : join(projectRoot, root)
        walkDir(dir, (file) => {
          if (!isScanFile(file)) return
          scanned++
          scanFileForUsage(file)
        })
      }
      const elapsed = Date.now() - start
      const dynNote = filterDynamicSeen ? ' — dynamic call(s) detected' : ''
      this.warn?.(
        `[nexus-manifest-filter] scanned ${scanned} files in ${elapsed}ms · ` +
        `${usedGqlNames.size} gql, ${usedRestRoutes.size} rest, ` +
        `${usedCrudEntities.size} crud, ${usedWsPaths.size} ws${dynNote}`,
      )
      if (filterDynamicSeen && filterStrict) {
        this.error?.(
          `[nexus-manifest-filter] strict mode: dynamic call without // @nexus-include pragma. ` +
          `Inline the literal name, add a pragma above the call, or switch to filterMode: 'loose'.`,
        )
      }
    },

    load(id) {
      if (filterMode !== 'usage') return null
      if (!isManifestImport(id)) return null

      let raw
      try {
        raw = JSON.parse(readFileSync(stripQuery(id), 'utf8'))
      } catch {
        return null
      }

      // Loose-mode escape hatch: any dynamic call disables the
      // filter for this build. We still inline via a virtual module
      // (so on-disk file isn't bundled as-is) but with the full
      // contents — same security as today, no broken calls.
      const projected = filterDynamicSeen ? raw : projectManifest(raw)

      const before = (raw.endpoints || []).length
      const after = (projected.endpoints || []).length
      const beforeRefs = Object.keys(raw.refs || {}).length
      const afterRefs = Object.keys(projected.refs || {}).length
      this.warn?.(
        `[nexus-manifest-filter] manifest projected: ` +
        `${before} → ${after} endpoints, ${beforeRefs} → ${afterRefs} refs`,
      )

      return `export default ${JSON.stringify(projected)}`
    },
  }

  // ── nexus-hmr (mode 4: codegen → frontend full-reload) ──────────
  //
  // Watches the typed-codegen tree (web/src/__nexus by default) and
  // fires a full-reload over vite's WebSocket whenever the renderer
  // rewrites a file. Pairs with `nexus dev`'s auto-codegen step so
  // editing a Go endpoint propagates to the browser without a manual
  // refresh: Go restarts → codegen writes new TS into the tree →
  // this watcher fires full-reload → browser fetches the fresh
  // bundle. Apply: 'serve' so prod builds skip it.
  //
  // The codegen dir is chosen by:
  //   options.codegenDir → explicit absolute or projectRoot-relative
  //   else 'src/__nexus' under the project root (matches
  //   frontend.Config's default Generate path)
  //
  // The watcher is registered against the directory even when empty
  // / non-existent at boot — vite's chokidar emits add events as
  // files appear, so the first codegen run is caught without a
  // restart of vite dev.
  const hmrPlugin = {
    name: 'nexus-hmr',
    enforce: 'pre',
    apply: 'serve',
    configureServer(server) {
      const dir = options.codegenDir
        ? (isAbsolute(options.codegenDir) ? options.codegenDir : join(projectRoot, options.codegenDir))
        : join(projectRoot, 'src/__nexus')
      server.watcher.add(dir)
      const dirResolved = resolve(dir)
      const onChange = (file) => {
        if (!resolve(file).startsWith(dirResolved)) return
        const rel = file.replace(projectRoot + '/', '')
        server.config.logger.info(`[nexus-hmr] codegen change → full-reload (${rel})`)
        server.ws.send({ type: 'full-reload' })
      }
      server.watcher.on('change', onChange)
      server.watcher.on('add', onChange)
    },
  }

  return [authoringPlugin, manifestFilterPlugin, loopGuardPlugin, hmrPlugin]

  // ---- script transform (TS / JS / TSX / JSX) -----------------------

  function transformScript(code, id, isVueScript) {
    const isTSX = id.endsWith('.tsx') || id.endsWith('.jsx')
    const sf = ts.createSourceFile(
      id,
      code,
      ts.ScriptTarget.Latest,
      /*setParentNodes=*/true,
      isTSX ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
    )
    const ms = new MagicString(code)
    let edited = false

    walk(sf, sf, ms, () => { edited = true })

    if (!edited) return null
    return {
      code: ms.toString(),
      map: ms.generateMap({ source: id, hires: true, includeContent: !isVueScript }),
    }
  }

  // ---- Vue SFC transform --------------------------------------------

  function transformVue(code, id) {
    const { descriptor } = parseSFC(code)
    const setup = descriptor.scriptSetup
    if (!setup || !setup.content) return null

    // Run the script transform on just the <script setup> body, then
    // splice it back into the original source so Vue's @vitejs plugin
    // sees a syntactically-identical SFC except for the rewritten ops.
    const scriptCode = setup.content
    const scriptId = id + '?vue&type=script&setup=true&lang=' + (setup.lang || 'ts')
    const result = transformScript(scriptCode, scriptId, /*isVueScript=*/true)
    if (!result) return null

    const ms = new MagicString(code)
    const start = setup.loc.start.offset
    const end = setup.loc.end.offset
    ms.overwrite(start, end, result.code)
    return {
      code: ms.toString(),
      map: ms.generateMap({ source: id, hires: true, includeContent: true }),
    }
  }

  // ---- AST walk + scope-local accumulation --------------------------

  function walk(node, sf, ms, onEdit) {
    // Each function-like body gets scanned as a unit so the
    // result-variable's lexical scope is bounded.
    ts.forEachChild(node, child => {
      if (isFunctionLike(child) && child.body && ts.isBlock(child.body)) {
        scanBlock(child.body, sf, ms, onEdit)
      } else if (ts.isBlock(child) && (!child.parent || !isFunctionLike(child.parent))) {
        // Top-level block (rare in TS but covers <script setup>).
        scanBlock(child, sf, ms, onEdit)
      }
      walk(child, sf, ms, onEdit)
    })
  }

  function isFunctionLike(n) {
    return ts.isFunctionDeclaration(n)
      || ts.isFunctionExpression(n)
      || ts.isArrowFunction(n)
      || ts.isMethodDeclaration(n)
      || ts.isGetAccessorDeclaration(n)
      || ts.isSetAccessorDeclaration(n)
      || ts.isConstructorDeclaration(n)
  }

  function scanBlock(block, sf, ms, onEdit) {
    // A <script setup> body is a SourceFile, not a Block. Statements
    // live on .statements either way.
    const stmts = block.statements || []
    for (let i = 0; i < stmts.length; i++) {
      const candidate = findNexusCall(stmts[i])
      if (!candidate) continue

      const accesses = []
      for (let j = i; j < stmts.length; j++) {
        collectAccesses(stmts[j], candidate.resultName, accesses)
      }
      if (accesses.length === 0) continue

      const tree = buildTree(accesses)
      if (!tree) continue
      const selectExpr = renderTree(tree)
      if (!selectExpr) continue

      injectSelect(candidate.callExpr, selectExpr, ms, sf)
      onEdit()
    }
  }

  // Match: const|let X = await Y.{query|mutate}('opname', ...)
  function findNexusCall(stmt) {
    if (!ts.isVariableStatement(stmt)) return null
    const decls = stmt.declarationList.declarations
    if (!decls || decls.length !== 1) return null
    const decl = decls[0]
    if (!decl || !ts.isIdentifier(decl.name)) return null
    if (!decl.initializer) return null

    let init = decl.initializer
    if (ts.isAwaitExpression(init)) init = init.expression
    if (!ts.isCallExpression(init)) return null

    const callee = init.expression
    if (!ts.isPropertyAccessExpression(callee)) return null
    const method = callee.name.text
    if (method !== 'query' && method !== 'mutate') return null

    const arg0 = init.arguments[0]
    if (!arg0 || !ts.isStringLiteral(arg0)) return null
    if (!manifest.ops.has(arg0.text)) return null

    // Skip if the caller already passed an explicit select.
    const arg2 = init.arguments[2]
    if (arg2 && hasSelectKey(arg2)) return null

    return {
      resultName: decl.name.text,
      callExpr: init,
    }
  }

  function hasSelectKey(arg) {
    if (!ts.isObjectLiteralExpression(arg)) return false
    return arg.properties.some(p =>
      ts.isPropertyAssignment(p) &&
      p.name && ts.isIdentifier(p.name) &&
      p.name.text === 'select'
    )
  }

  // Walk a statement, recording every res.X.Y... path rooted at varName.
  // Optional-chain (?.), non-null (!), and parenthesised forms unwrap.
  function collectAccesses(node, varName, out) {
    function unwrap(n) {
      while (n && (ts.isParenthesizedExpression(n) || ts.isNonNullExpression(n))) {
        n = n.expression
      }
      return n
    }
    function visit(n) {
      if (!n) return
      if (ts.isPropertyAccessExpression(n)) {
        // Walk the chain leftward, collecting field names.
        const path = []
        let cur = n
        while (cur && ts.isPropertyAccessExpression(cur)) {
          path.unshift(cur.name.text)
          cur = unwrap(cur.expression)
        }
        if (cur && ts.isIdentifier(cur) && cur.text === varName) {
          out.push(path)
          // Don't descend into the chain itself; we've captured it.
          return
        }
      }
      ts.forEachChild(n, visit)
    }
    visit(node)
  }

  function buildTree(accesses) {
    const tree = {}
    for (const path of accesses) {
      let cur = tree
      for (let i = 0; i < path.length; i++) {
        const k = path[i]
        if (i === path.length - 1) {
          if (cur[k] === undefined) cur[k] = true
          // If a deeper access already promoted it to {}, keep it.
        } else {
          if (cur[k] === undefined || cur[k] === true) cur[k] = {}
          cur = cur[k]
        }
      }
    }
    return tree
  }

  function renderTree(tree) {
    const parts = []
    for (const [k, v] of Object.entries(tree)) {
      const key = isSafeKey(k) ? k : JSON.stringify(k)
      if (v === true) parts.push(`${key}: true`)
      else parts.push(`${key}: ${renderTree(v)}`)
    }
    if (parts.length === 0) return ''
    return `{ ${parts.join(', ')} }`
  }

  function isSafeKey(s) {
    return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(s)
  }

  function injectSelect(callExpr, selectExpr, ms, sf) {
    const args = callExpr.arguments
    const closeParenPos = callExpr.getEnd() - 1

    if (args.length >= 3) {
      const arg2 = args[2]
      if (ts.isObjectLiteralExpression(arg2)) {
        // Insert select as the FIRST property in the existing opts.
        const insertPos = arg2.getStart(sf) + 1
        ms.appendRight(insertPos, ` select: ${selectExpr},`)
      }
      // If arg2 is something else (a variable / spread), bail — too risky.
      return
    }
    if (args.length === 2) {
      ms.appendLeft(closeParenPos, `, { select: ${selectExpr} }`)
      return
    }
    if (args.length === 1) {
      ms.appendLeft(closeParenPos, `, undefined, { select: ${selectExpr} }`)
      return
    }
    // 0 args is invalid for query/mutate but defend anyway.
    ms.appendLeft(closeParenPos, `undefined, undefined, { select: ${selectExpr} }`)
  }

  // ── manifest-filter helpers ──────────────────────────────────────

  // walkDir is a deps-free recursive directory walker. Skips a few
  // well-known build-output / dependency directories (FILTER_SKIP_DIRS,
  // hoisted to module scope so it survives the TDZ when this fn is
  // invoked from a closure that captured the post-return scope).
  // Hand-rolled instead of pulling in fast-glob — the plugin ships
  // embedded in the Go binary, so every dep adds to the consumer's
  // install footprint.
  function walkDir(dir, fn) {
    if (!existsSync(dir)) return
    let entries
    try {
      entries = readdirSync(dir, { withFileTypes: true })
    } catch {
      return
    }
    for (const e of entries) {
      if (FILTER_SKIP_DIRS.has(e.name)) continue
      const full = join(dir, e.name)
      if (e.isDirectory()) walkDir(full, fn)
      else fn(full)
    }
  }

  function isScanFile(p) {
    return /\.(ts|tsx|js|jsx|vue)$/.test(p)
  }

  // isManifestImport recognises the on-disk SDK manifest. We accept
  // either the configured manifestPath exactly, or any path ending
  // in /sdk/manifest.json (covers monorepo setups where multiple
  // sub-packages might import their own SDK bundle). vite passes
  // resolved ids that may carry a `?something` suffix on rare paths;
  // strip it before comparing.
  function isManifestImport(id) {
    if (!id) return false
    const clean = stripQuery(id)
    if (manifestPath && resolve(clean) === resolve(manifestPath)) return true
    return clean.endsWith('/sdk/manifest.json')
  }

  function stripQuery(id) {
    const q = id.indexOf('?')
    return q >= 0 ? id.slice(0, q) : id
  }

  function scanFileForUsage(file) {
    let code
    try { code = readFileSync(file, 'utf8') } catch { return }
    let scriptCode = code
    let scriptKindHint = file
    if (file.endsWith('.vue')) {
      if (!parseSFC) return
      let descriptor
      try { ({ descriptor } = parseSFC(code)) } catch { return }
      scriptCode = descriptor.scriptSetup?.content || descriptor.script?.content || ''
      if (!scriptCode) return
      // Use the SFC's script lang to pick the AST mode below.
      const lang = descriptor.scriptSetup?.lang || descriptor.script?.lang || 'ts'
      scriptKindHint = file + '.' + lang
    }
    let sf
    try {
      const isTSX = /\.(t|j)sx$/.test(scriptKindHint)
      sf = ts.createSourceFile(
        scriptKindHint,
        scriptCode,
        ts.ScriptTarget.Latest,
        /*setParentNodes=*/true,
        isTSX ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
      )
    } catch {
      return
    }
    walkUsage(sf, sf, scriptCode)
  }

  function walkUsage(node, sf, srcCode) {
    if (ts.isCallExpression(node)) {
      collectUsageFromCall(node, sf, srcCode)
    }
    ts.forEachChild(node, (child) => walkUsage(child, sf, srcCode))
  }

  // collectUsageFromCall identifies a usage signal and adds it to
  // the appropriate set. A usage signal is ANY call that looks like
  // `<expr>.<query|mutate|crud|rest|ws>(<literal>, ...)`. We don't
  // try to verify the receiver is a NexusClient — false positives
  // (overshoots: pulls a real endpoint into the bundle that wasn't
  // meant) are safer than false negatives (undershoots: drops an
  // endpoint the app actually calls). The set is a *floor*, not a
  // ceiling.
  function collectUsageFromCall(call, sf, srcCode) {
    const callee = call.expression
    if (!ts.isPropertyAccessExpression(callee)) return
    const method = callee.name && callee.name.text
    if (method !== 'query' && method !== 'mutate'
      && method !== 'crud' && method !== 'rest' && method !== 'ws') return

    const arg0 = call.arguments[0]
    if (!arg0) return

    if (method === 'rest') {
      // nx.rest('METHOD', '/path', args, opts) — both literals required.
      const arg1 = call.arguments[1]
      if (ts.isStringLiteral(arg0) && arg1 && ts.isStringLiteral(arg1)) {
        usedRestRoutes.add(arg0.text.toUpperCase() + ' ' + arg1.text)
        return
      }
      handleDynamicCall(call, sf, srcCode, 'rest')
      return
    }

    if (ts.isStringLiteral(arg0)) {
      if (method === 'query' || method === 'mutate') usedGqlNames.add(arg0.text)
      else if (method === 'crud') usedCrudEntities.add(arg0.text)
      else if (method === 'ws') usedWsPaths.add(arg0.text)
      return
    }

    handleDynamicCall(call, sf, srcCode, method)
  }

  // handleDynamicCall reads any `// @nexus-include foo, bar` pragma
  // sitting immediately above the call's line and folds the listed
  // names into every usage set (we don't know what kind the dynamic
  // first-arg resolves to — projecting against the manifest filters
  // out non-existent kinds naturally). When no pragma is present the
  // build-wide dynamic-seen flag is set; loose mode treats that as
  // "include everything", strict mode treats it as a build error.
  function handleDynamicCall(call, sf, srcCode, methodKind) {
    const names = readIncludePragma(call, sf, srcCode)
    if (names.length === 0) {
      filterDynamicSeen = true
      return
    }
    for (const n of names) {
      if (methodKind === 'rest') {
        usedRestRoutes.add(n)
        continue
      }
      // For non-rest, the pragma name is interpreted as the GraphQL
      // op name OR the WS path OR the crud entity. We add it to all
      // three; the manifest projection filters out the ones that
      // don't exist as real endpoints.
      usedGqlNames.add(n)
      usedCrudEntities.add(n)
      if (n.startsWith('/')) usedWsPaths.add(n)
    }
  }

  function readIncludePragma(call, sf, srcCode) {
    // Walk backward through preceding lines, tolerating any number
    // of blank lines between the pragma and the call so a developer
    // can write a multi-line annotation block.
    let cursor = call.getStart(sf)
    while (cursor > 0) {
      const lineStart = srcCode.lastIndexOf('\n', cursor - 1) + 1
      const prevLineEnd = lineStart - 1
      if (prevLineEnd <= 0) break
      const prevLineStartIdx = srcCode.lastIndexOf('\n', prevLineEnd - 1) + 1
      const prevLine = srcCode.slice(prevLineStartIdx, prevLineEnd).trim()
      if (prevLine === '') {
        cursor = prevLineStartIdx
        continue
      }
      const m = prevLine.match(/^\/\/\s*@nexus-include\s+(.+)$/)
      if (m) return m[1].split(',').map(s => s.trim()).filter(Boolean)
      // First non-blank, non-pragma line above the call → no pragma.
      return []
    }
    return []
  }

  // projectManifest filters raw to only the endpoints flagged in the
  // usage sets, plus auth flows (always preserved), plus the ref
  // types reachable from the surviving endpoints' Args/Return
  // TypeRefs. Mirrors the Go-side collectAuthFlowRefs closure walk
  // — same algorithm, JS-side at build time.
  function projectManifest(raw) {
    const out = {
      version: raw.version,
      basePath: raw.basePath,
      auth: raw.auth,
      endpoints: [],
      refs: {},
    }
    for (const e of (raw.endpoints || [])) {
      if (shouldKeepEndpoint(e)) out.endpoints.push(e)
    }
    const need = new Set()
    for (const e of out.endpoints) {
      collectRefsFromTypeRef(e.args, need)
      collectRefsFromTypeRef(e.return, need)
    }
    let changed = true
    while (changed) {
      changed = false
      for (const name of [...need]) {
        if (out.refs[name]) continue
        const nt = (raw.refs || {})[name]
        if (!nt) continue
        out.refs[name] = nt
        for (const f of (nt.fields || [])) collectRefsFromTypeRef(f.type, need)
        changed = true
      }
    }
    return out
  }

  function shouldKeepEndpoint(e) {
    if (!e) return false
    // Auth flows (login/logout/me) are always preserved so the SDK's
    // auth namespace keeps working regardless of usage scan results.
    if (e.authFlow) return true
    // Name match works across all transports: pragma authors write
    // `// @nexus-include myEndpoint` without knowing whether the
    // target is GraphQL, REST, or WS — and any endpoint registered
    // with a `name` field can be referenced by it. Cheap to check
    // first, common case for GraphQL.
    if (e.name && usedGqlNames.has(e.name)) return true

    if (e.transport === 'graphql') {
      return false  // GraphQL is name-only; falling through means no match
    }
    if (e.transport === 'rest') {
      const route = (e.method || 'GET').toUpperCase() + ' ' + (e.path || '')
      if (usedRestRoutes.has(route)) return true
      // CRUD entities expand to a 5-route suite under /<entity>.
      // Match any rest endpoint whose path starts with /<entity> or
      // /<entity>/. This may over-keep when an unrelated route lives
      // under that prefix; the filter is a floor, not a ceiling.
      for (const ent of usedCrudEntities) {
        const prefix = '/' + ent
        if (e.path === prefix || (e.path || '').startsWith(prefix + '/')) return true
      }
      return false
    }
    if (e.transport === 'ws') {
      return usedWsPaths.has(e.path)
    }
    return false
  }

  function collectRefsFromTypeRef(t, into) {
    if (!t) return
    if (t.kind === 'ref' && t.ref) into.add(t.ref)
    if (t.of) collectRefsFromTypeRef(t.of, into)
    if (t.keyOf) collectRefsFromTypeRef(t.keyOf, into)
    if (t.object && t.object.fields) {
      for (const f of t.object.fields) collectRefsFromTypeRef(f.type, into)
    }
  }
}