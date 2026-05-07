// nexus-vite-plugin.js — auto-injects opts.select into nx.query /
// nx.mutate calls based on the property accesses the surrounding
// code makes on the result variable.
//
// Without this plugin, the SDK auto-walker fetches every field
// reachable from the operation's return type up to depth 3 — safe
// but over-fetches. With the plugin, every typed call rewrites at
// build time to fetch exactly the fields the consumer reads.
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

import { readFileSync, existsSync, statSync, utimesSync } from 'node:fs'
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

  return [authoringPlugin, loopGuardPlugin]

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
}