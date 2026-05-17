// counter.js — vanilla-JS island demonstrating the nl-island
// v1 contract end-to-end. No framework dependency.
//
// Lifecycle: mount(el, props, channel) → updated(el, props,
// instance) → destroyed(el, instance). Channel.on(event, fn)
// fires when the server calls ctx.PushIsland(name, event,
// payload).
//
// The element's subtree is the island's playground — the
// live-template engine won't morph it on subsequent server
// re-renders. State that lives only on the client (the count
// here) survives notifier-triggered re-renders of the
// surrounding template.

export function mount(el, props, channel) {
    let count = (props && props.initial) || 0;
    el.innerHTML = `
        <div class="island-card">
            <p class="lead">Vanilla-JS island.</p>
            <p class="count">Clicks: <strong></strong></p>
            <button data-act="inc">+1 (client-side)</button>
            <button data-act="reset">reset</button>
            <p class="hint">
                Click + to bump the counter purely on the client —
                the surrounding live template never re-renders.
                The "Server reset" button on the page below
                fires <code>ctx.PushIsland("Counter", "reset",
                nil)</code>, which the channel listener receives
                and applies to this island only.
            </p>
        </div>
    `;
    const strong = el.querySelector("strong");
    const render = () => { strong.textContent = String(count); };
    render();

    el.querySelector('[data-act="inc"]').addEventListener("click", () => {
        count += 1;
        render();
    });
    el.querySelector('[data-act="reset"]').addEventListener("click", () => {
        count = 0;
        render();
    });

    // Listen for server pushes. The channel.on() handle is
    // scoped to this island instance — only PushIsland calls
    // targeting "Counter" fire here.
    const offReset = channel.on("reset", () => {
        count = 0;
        render();
    });

    return { offReset, render };
}

export function updated(el, newProps, instance) {
    // Called when :nl-island-props changes server-side. Demo
    // behavior: if the server sends a new initial value, snap
    // to it. Real islands would diff props against the
    // running state more carefully.
    if (newProps && typeof newProps.initial === "number") {
        const strong = el.querySelector("strong");
        if (strong) strong.textContent = String(newProps.initial);
    }
}

export function destroyed(el, instance) {
    if (instance && instance.offReset) instance.offReset();
}
