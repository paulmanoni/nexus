# nl-island bridge templates

Drop-in reference implementations of the `nl-island` lifecycle contract for
Vue 3, React 18, and vanilla JS. Copy whichever fits, replace `Counter` with
your component name, embed in a `.nlt` template, and you're done.

## The contract every bridge implements

```ts
// Three exported functions, called by /__live/nexus.js
export function mount(el, props, channel)     // create your UI inside el
export function updated(el, newProps, inst)   // server changed :nl-island-props
export function destroyed(el, inst)            // element removed; clean up
```

- **`el`** — the DOM element carrying `nl-island="<name>"`. You own its subtree;
  the live-template engine's morph algorithm skips it on every server-driven
  re-render.
- **`props`** — JSON-parsed value of `:nl-island-props`. Whatever the
  server-side helper returned (`map[string]any`, a struct, anything that
  `json.Marshal` handles).
- **`channel`** — `{ on(event, fn) → unsubscribe }`. Fires when the server
  calls `ctx.PushIsland(name, event, payload)`. Multiple `on` per event are
  fine.
- **`inst`** — whatever `mount` returned. Threaded back into `updated` and
  `destroyed` so you can hold app handles, reactive refs, etc.

## Choosing a bridge

| | Vanilla | Vue | React |
|---|---|---|---|
| Build step required | no | yes (Vite) | yes (Vite) |
| Bundle size baseline | bytes you write | ~40 KB (Vue runtime) | ~45 KB (React + ReactDOM) |
| When it's the right call | tiny widgets, charts, animations, one-off DOM glue | rich state, you already use Vue, you want SFC ergonomics | rich state, your team's already in React |
| Reactivity story | manual | Vue refs + reactive props in place | useState + props via useSyncExternalStore |

The dashboard rewrite pattern (per the audit): vanilla for SVG-heavy widgets
(packet animator, trace waterfall), Vue for the architecture canvas (because
it already wraps VueFlow). React only if there's already a team preference.

## Layout once you have multiple islands

```
your-app/
├── main.go
├── templates/*.nlt
├── islands.src/                  # source — your code lives here
│   ├── Counter.vue
│   ├── Counter.island.ts         # generated bridge
│   ├── Chart.tsx
│   ├── Chart.island.tsx          # generated bridge
│   └── counter.js                # vanilla — no bridge, no build
├── islands/                      # build output (embedded for prod)
│   └── *.js
├── vite.config.ts                # auto-discovers *.island.ts via glob
├── package.json
└── tsconfig.json
```

The convention: any file matching `islands.src/*.island.{ts,tsx,js,jsx}` is
an island entry point and gets bundled into `islands/<name>.js`. Vanilla
files in `islands.src/` that *aren't* `.island.*` either become helpers (Vite
treats them as code-split chunks) or — if you want zero build step — sit
directly in `islands/`.

## Going from source to embed

Dev mode (real Vite HMR for islands):
```bash
vite          # watches islands.src/, serves at :5173
nexus dev     # serves the .nlt + Go side; proxies /islands/* to Vite
```

Production:
```bash
vite build    # bundles to islands/
go build      # embeds islands/*.js via //go:embed
```

See the per-framework subdirectories for working examples. Each is a copy-
paste starting point.
