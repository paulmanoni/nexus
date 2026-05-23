# extension/tour

Guided walkthroughs for any HTTP-served frontend — React, Vue, Angular, Svelte, vanilla, anything. Captures click-by-click sequences with auto-screenshots, lets operators edit text inline, then plays them back as numbered-badge highlights with tooltips on the live UI. Exports to PDF or Word for handoff docs.

The in-page agent renders inside a **closed Shadow DOM** rooted at `document.body` — host CSS can't bleed in, the plugin's UI can't bleed out, and the host frontend has no idea anything is on top of it.

## Wire it in one line

```go
import "github.com/paulmanoni/nexus/extension/tour"

nexus.Run(nexus.Config{...},
    tour.Module(
        tour.WithGORM(db.DB()),  // production; omit for in-memory
        tour.AutoInject(true),   // splice <script> into every text/html response
    ),
    // ...
)
```

After restart, every HTML page the app serves carries a floating **● Tour** pill (bottom-right). `AutoMigrate` runs on first use to create `nexus_tours` + `nexus_tour_steps`.

## Record / Play / Manage / Export

| Mode | What happens |
|---|---|
| **Record** | Click the pill → **Record new tour** → modal prompts for **Title** + **Description**. After Start, hover any element (blue rectangle follows the mouse) and click to capture. Each capture pops an inline editor requiring step text before the next pick — placeholders can't slip through. Clicks inside the previous step's bounding box become **substeps** automatically. **Pause/Resume** (button or `P` key) lets you interact with the host without capturing — open a dropdown manually, then resume to capture items inside. **Enter** captures the hovered element without a synthetic click, so menus that close on outside-click stay open. |
| **Play** | Fetches tours for the current `pathname`, walks the tree DFS, draws an orange ring + numbered badge on each target with a tooltip beside it. Back / Next / Skip / Done. `scrollIntoView` before measure so off-screen targets are pulled into view. Missing-target path shows the selector + Skip. |
| **Manage** | Visit `/__nexus/tour` for the authoring dashboard — Vue 3 SPA from esm.sh, no framework rebuild. Edit name/route/description, per-step title/text/placement; **↑ ↓** reorder, **→** demote, **←** promote, **×** delete (children reparent). Drag-and-drop reordering across the whole library; tours group by route when unfiltered. Inline screenshot thumbnails with click-to-zoom. |
| **Preview / Export** | Every tour gets a print-friendly preview at `/__nexus/tour/tours/:id/preview` — one composite screenshot per "scene" with badges + callout cards in the screenshot's margins, connector lines tying each card to its badge. **📄 Save as PDF** uses the browser's print engine; **📋 Copy for Word** writes Arial-12 numbered-list HTML + the composite PNG to the clipboard, ready to paste into Word/Google Docs. The Word export now leads with a **Table of Contents** (Tours / Detailed steps / Figures). **🗑 Delete tour** prunes a single tour from the preview (per-tour buttons on multi-tour previews so you can keep some and drop others). Step text is **click-to-edit inline** on the preview page itself — changes save on blur, no need to bounce to the editor. Route lookup tolerates trailing-slash mismatches. |

## Multi-cover scenes (dropdowns, modals, multi-step flows)

Tours that span page-state changes — open a dropdown, capture an item inside, then close it — store **one cover screenshot per page state**. The recorder takes a fresh cover automatically on every Resume from Pause; the **📸 Capture scene** button on the recorder bar also triggers one on demand for finer control (open the dropdown, click Capture scene, then capture items inside it). Each step pins to the cover that was active when it was captured. The preview renders one composite per cover labelled "Scene N of M", so dropdown items land on the cover that shows the dropdown **open**, not the stale initial screenshot.

**Caveat — pure-CSS `:hover` dropdowns.** html2canvas clones the DOM into a sandboxed iframe to render, and the clone has no real cursor on it — so any element that's only visible via `:hover` (with no JS toggle) is collapsed in the snapshot even if it was open in the live page. Workaround: most production dropdowns toggle a class on click; those work as expected. For pure `:hover` cases the simplest fix is adding a `data-tour-stay-open` attribute (or any persistent class) you can target so the dropdown stays open during the brief capture window.

## Editing tours from the preview

The preview page itself is interactive:

- **+ below** / **+ sub** on any step row inserts a new step (text-only OR capture-from-page). Capture-from-page navigates to the host route and waits for a manual **▶ Start capture** click before recording — operators arrange the page state (open dropdown, expand panel) first, then start.
- **×** removes a step (children reparent to its parent).
- **↳ Merge as substeps of…** on any Scene 2+ header attaches the entire scene's steps under any earlier step (picker shows every candidate).
- **📋 Reuse step** on the recorder bar copies steps from any existing tour into the active one. Useful when several tours share an initial happy-path prefix.
- **Drag the badge + its border** to nudge a callout's anchor on the composite when the auto-placement is awkward. Positions persist per-step in localStorage; the saved `rect_left/rect_top` on the server stays put so ▶ Play still overlays the original control.

## Keyboard shortcuts + controls during recording

| Trigger | What it does |
|---|---|
| `P` | Toggle pause / resume — no outside-click on the recorder bar, so transient UIs (menus, popovers) stay open |
| `Enter` | Capture the currently-hovered element without a synthetic click — works on hosts that close menus on outside-click |
| `Escape` | Cancel the picker (same as pause) |
| **📸 Capture scene** button | Manually take a fresh cover screenshot. Use after arranging the page (open dropdown, expand panel) — subsequent steps land on this new cover so dropdown items render against the dropdown-open state |
| **✎ Edit last step** button | Override the placeholder title / text / placement on the most recent capture inline |
| **📋 Reuse step** button | Open a picker to copy steps from any existing tour into the active one |

Both `P` and `Enter` walk `composedPath()`, `document.activeElement` (with shadow-root descent), and check for role-based widgets (`textbox` / `searchbox` / `combobox`) so they're ignored when typing in any input — including across Shadow DOM boundaries (the agent's own editor UI) and custom autocomplete fields built as styled `<div>`s.

## Why "hover on top of any frontend" works

| Property | Why |
|---|---|
| Mounted on `document.body` (outside host root) | Host render cycle can't unmount or rewrite it |
| Closed Shadow DOM | Host CSS — Tailwind reset, Vuetify defaults — can't bleed in; plugin styles can't bleed out |
| `position: fixed; inset: 0; z-index: 2147483647` | Always on top, even above modal libraries using `z-index: 9999` |
| `pointer-events: none` on root, `auto` on interactive UI | Host clicks pass through where the plugin isn't actively presenting |
| Vanilla JS agent (no Vue/React runtime) | Zero conflicts with the host's framework |
| MutationObserver re-attaches if removed | SPA route swaps, Vue Teleports, Vuetify portals, and `body.innerHTML = …` won't kill the overlay — it self-restores in the next tick |
| `pointerdown` capture + `elementFromPoint` hit-test | Disabled buttons + inputs (which swallow `click`) are still pickable |

## Drive it from the host

The agent exposes `window.nexusTour = { record, play, stop }` so a host's own "Help" button can trigger a tour without using the pill:

```js
<button onClick={() => window.nexusTour.play()}>Show me how</button>
```

Run `nexus docs tour` for the full inline reference (when added) or browse `extension/tour/agent/inject.js` — the entire client runtime is one self-contained file.
