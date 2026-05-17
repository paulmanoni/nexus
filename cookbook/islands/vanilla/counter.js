// counter.js — vanilla nl-island starter.
//
// The whole module is the bridge — there's no separate
// component layer because nothing's framework-mediating
// the DOM. You own `el`'s subtree start to finish.
//
// Edit freely; the live-template engine only needs the
// three exports below.

export function mount(el, props, channel) {
  // Render initial DOM. Use any approach — innerHTML, manual
  // createElement, your own micro-lib. The engine doesn't care.
  const initial = (props && props.initial) || 0
  let count = initial

  el.innerHTML = `
    <div class="counter-island">
      <button data-act="inc">Count: <span></span></button>
      <p class="hint">
        Click to bump on the client only.
        Server can fire <code>PushIsland("Counter", "reset", nil)</code>
        to zero this back out.
      </p>
    </div>
  `

  const span = el.querySelector('span')
  const render = () => { span.textContent = String(count) }
  render()

  el.querySelector('[data-act="inc"]').addEventListener('click', () => {
    count += 1
    render()
  })

  // Subscribe to server-pushed events scoped to this island.
  // channel.on returns an unsubscribe fn — keep a reference
  // so destroyed() can release it.
  const offReset = channel.on('reset', () => {
    count = 0
    render()
  })

  // Whatever you return lands as `instance` in updated() and
  // destroyed(). Stash anything you'll need to clean up
  // (event listeners, intervals, requestAnimationFrame ids,
  // third-party widget handles).
  return { offReset }
}

export function updated(el, newProps, instance) {
  // Called when the server changes :nl-island-props. The
  // attribute on `el` is already up to date; you decide what
  // to do with the new values. Common patterns:
  //   - snap a controlled input to the new value
  //   - re-render a chart with the new dataset
  //   - tween between old and new positions
  //
  // For this counter, props.initial is read-once at mount;
  // we ignore subsequent changes.
}

export function destroyed(el, instance) {
  // Element is being removed from the page. Release any
  // resources you allocated in mount().
  instance?.offReset?.()
}
