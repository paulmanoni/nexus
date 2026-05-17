// vueflow-canvas.js — VueFlow island for the live-template
// engine. Demonstrates wrapping a rich-client Vue component
// behind the nl-island lifecycle so the surrounding page can
// stay in live-template land while the canvas stays Vue.
//
// Loads Vue 3 + VueFlow from esm.sh — zero build step for the
// example. A production deploy would bundle locally for offline
// + caching + version pinning.
//
// Contract (per nl-island v1):
//   - mount(el, props, channel)   → creates Vue app inside el
//   - updated(el, newProps, inst) → reactive refs pick up new
//                                    nodes/edges; VueFlow does
//                                    the incremental DOM update,
//                                    no remount
//   - destroyed(el, inst)         → unmounts the Vue app
//
// channel.on("focus-node", { id }) → pan the view to a node.
// Server-side counterpart is ctx.PushIsland("ArchCanvas",
// "focus-node", { id: "n3" }).

// Vue's ESM-bundler build needs __VUE_PROD_DEVTOOLS__ and the
// related compile-time flags defined as globals BEFORE the
// module import resolves. ES module imports hoist above any
// top-of-file statements, so the flags can't be set from
// inside this file — they're injected by an inline <script>
// in Architecture.nlt that runs at SSR parse time, before the
// browser dynamic-imports this module.
import { createApp, h, ref } from "https://esm.sh/vue@3.4.0";
import { VueFlow, Position } from "https://esm.sh/@vue-flow/core@1.41.0?deps=vue@3.4.0";

// Inject the VueFlow stylesheet once per page — multiple
// islands sharing the same canvas package shouldn't duplicate
// the <link> tag. The marker attribute lets us idempotently
// detect prior insertion across hot-reloads.
function ensureVueFlowStyles() {
    const marker = "data-nl-vueflow-css";
    if (document.querySelector(`link[${marker}]`)) return;
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.href = "https://esm.sh/@vue-flow/core@1.41.0/dist/style.css";
    link.setAttribute(marker, "");
    document.head.appendChild(link);
}

export function mount(el, props, channel) {
    ensureVueFlowStyles();

    // Reactive refs Vue + VueFlow consume. updated() mutates
    // these in place, which keeps Vue's reconciler happy and
    // avoids re-creating node/edge components on every server
    // re-render.
    const nodes = ref(toVueFlowNodes(props));
    const edges = ref(toVueFlowEdges(props));

    // useVueFlow is per-instance via the id prop — without
    // it, multiple ArchCanvas islands on one page would share
    // the same global flow state.
    const flowId = "nl-arch-" + Math.random().toString(36).slice(2, 8);

    // The Vue app body is minimal — just a host component
    // that hands VueFlow the reactive refs. Custom node /
    // edge types would slot in via the named slots of
    // VueFlow; the demo uses defaults.
    const app = createApp({
        setup() {
            return () => h(
                "div",
                { style: "height: 460px; border: 1px solid #e1e4e8; border-radius: 6px; background: #fafbfc; overflow: hidden;" },
                [
                    h(VueFlow, {
                        id: flowId,
                        modelValueNodes: nodes.value,
                        modelValueEdges: edges.value,
                        nodes: nodes.value,
                        edges: edges.value,
                        fitViewOnInit: true,
                        defaultViewport: { x: 0, y: 0, zoom: 0.9 },
                    }),
                ]
            );
        },
    });
    const instance = app.mount(el);

    // Server-pushed focus: pan to a specific node. Showcases
    // the channel API; not strictly needed for the canvas to
    // function.
    const offFocus = channel.on("focus-node", ({ id }) => {
        // Defer to next tick so VueFlow has the node mounted.
        setTimeout(() => {
            const node = nodes.value.find((n) => n.id === String(id));
            if (!node) return;
            // Minimal "pan-to" — full impl would call vue-flow's
            // setCenter() via the useVueFlow hook. Kept simple
            // for the demo.
            console.info("[arch-canvas] focus-node:", id, node);
        }, 0);
    });

    return { app, nodes, edges, offFocus };
}

export function updated(el, newProps, inst) {
    if (!inst) return;
    // Mutating the refs in place fires Vue's reactivity →
    // VueFlow does an incremental update. No remount, internal
    // state (zoom, pan, selection) is preserved across server-
    // driven re-renders.
    inst.nodes.value = toVueFlowNodes(newProps);
    inst.edges.value = toVueFlowEdges(newProps);
}

export function destroyed(el, inst) {
    if (!inst) return;
    if (inst.offFocus) inst.offFocus();
    if (inst.app) inst.app.unmount();
}

// --- shape mapping --------------------------------------------
//
// Go-side struct uses lowercase JSON tags (id, label, x, y);
// these helpers translate to VueFlow's expected
// { id, position, data } shape. Defensive against missing
// arrays so a fresh page with no graph state doesn't blow up.

function toVueFlowNodes(props) {
    if (!props || !Array.isArray(props.nodes)) return [];
    return props.nodes.map((n) => ({
        id: String(n.id),
        position: { x: Number(n.x) || 0, y: Number(n.y) || 0 },
        data: { label: n.label || n.id },
        label: n.label || n.id,
        type: "default",
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
    }));
}

function toVueFlowEdges(props) {
    if (!props || !Array.isArray(props.edges)) return [];
    return props.edges.map((e) => ({
        id: String(e.id),
        source: String(e.source),
        target: String(e.target),
        animated: !!e.animated,
    }));
}
