# arch-canvas — VueFlow as an nl-island

The dashboard architecture-canvas pattern, distilled into ~300 lines of Go
+ 80 lines of Vue + 50 lines of bridge TypeScript. Vue-side authored per the
[islands cookbook](../../cookbook/islands/vue/) — `.vue` SFC + a tiny
generated `.island.ts` bridge file.

## Just running it

```bash
go run ./examples/arch-canvas
# → http://localhost:8082
```

`islands/arch-canvas.js` is committed (1.5 KB), so `go run` works without
`npm install`. The bundle externalizes Vue + VueFlow to `esm.sh` URLs; the
browser fetches them once on first load and caches.

## Editing the Vue side

```bash
cd examples/arch-canvas
npm install
npm run build       # regenerates islands/arch-canvas.js
# OR:
npm run dev         # vite dev server on :5173 (no nexus integration yet)
```

`islands.src/arch-canvas.vue` is the actual component — edit there.
`islands.src/arch-canvas.island.ts` is the bridge — copy of the cookbook's
Vue template, swap only the `import Component from './…'` line if you
rename.

## Why externalized deps?

Vue and `@vue-flow/core` together are ~250 KB unminified. With them
externalized to esm.sh and only the SFC + bridge bundled locally, the
committed `arch-canvas.js` stays at ~1.5 KB. For a production app you'd
flip externals OFF in `vite.config.ts` and ship a self-contained bundle —
that's a 5-line change.

## Files

```
.
├── main.go                              live-template + Graph model
├── templates/
│   └── Architecture.nlt                 page chrome + <nl-island/> tag
├── islands.src/                         source (edit these)
│   ├── arch-canvas.vue                  Vue 3 SFC
│   └── arch-canvas.island.ts            bridge (cookbook copy)
├── islands/                             build output (committed)
│   └── arch-canvas.js                   served via WithStatic
├── vite.config.ts                       multi-entry glob + externals
├── tsconfig.json
└── package.json
```
