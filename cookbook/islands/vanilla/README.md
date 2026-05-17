# Vanilla nl-island

No build step. No framework. Drop `counter.js` into your `islands/`
directory (or `WithStatic` from anywhere on an embed.FS), point a
`<nl-island src="…"/>` at it, done.

One file — the file itself IS the bridge, because there's no component
layer underneath.

## When this is the right call

- The widget is small enough that a framework's runtime cost outweighs the
  ergonomics win (charts, SVG animations, intersection observers).
- You want zero dependencies — the module loads as-is, no transpile.
- You're prototyping and don't want to think about tooling yet.
- You're wrapping a third-party library that already does its own DOM
  management (D3, Mapbox, Monaco, Cesium…).

## What you write

```js
export function mount(el, props, channel) {
  // initial render into el
  // listen for channel events
  // return any handle you need in updated/destroyed
}

export function updated(el, newProps, instance) {
  // optional: react to changed :nl-island-props
}

export function destroyed(el, instance) {
  // optional: tear down listeners, intervals, etc.
}
```

`counter.js` in this directory is a working starter — copy + rename, swap
the body for your widget.

## Embedding in your template

```html
<nl-island name="Counter"
           src="/islands/counter.js"
           :props="CounterProps()"/>
```

Server side, identical to the Vue + React examples.
