// counter.js — vanilla nl-island. ~30 LoC, no build step, no
// framework. The whole module IS the bridge.
//
// Demonstrates the three lifecycle hooks (mount / updated /
// destroyed) plus the channel API. Server can fire
// ctx.PushIsland("Counter", "ping", payload) to deliver
// scoped events without re-rendering the surrounding template.

export function mount(el, props, channel) {
    let count = (props && props.initial) || 0
    let pings = 0

    el.innerHTML = `
        <div class="island">
            <button data-act="inc">+1 (client)</button>
            <p>client clicks: <strong data-c></strong></p>
            <p>server pings:  <strong data-p></strong></p>
        </div>
    `
    const cEl = el.querySelector('[data-c]')
    const pEl = el.querySelector('[data-p]')
    const render = () => {
        cEl.textContent = String(count)
        pEl.textContent = String(pings)
    }
    render()

    el.querySelector('[data-act="inc"]').addEventListener('click', () => {
        count += 1
        render()
    })

    const offPing = channel.on('ping', () => {
        pings += 1
        render()
    })

    return { offPing }
}

export function destroyed(_el, instance) {
    instance?.offPing?.()
}
