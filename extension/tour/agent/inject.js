// Nexus Tour — in-page agent (Phase 2)
//
// This file is the entire client-side runtime for the tour plugin.
// It is intentionally framework-free vanilla JS: it must run on
// ANY frontend (React, Vue, Angular, Svelte, vanilla) without
// caring about the host's render cycle, CSS, or build tooling.
//
// Everything renders inside a closed Shadow DOM rooted at
// document.body. The host frontend can't see in; the agent can't
// leak out. The single observable side-effect on the host's DOM
// is `document.body.appendChild(<nexus-tour-overlay>)` — and even
// that element has `pointer-events: none` except where the agent
// is actively presenting UI.
//
// File layout (all in this one file by design — keeps the embed
// trivial and the build toolchain non-existent):
//
//   1. Boot guard + Shadow DOM mount
//   2. Backend API client (fetch wrappers for /__nexus/tour/*)
//   3. Element picker (hover-highlight + click-pick)
//   4. Screenshot capture (lazy-loads html2canvas-pro from a CDN)
//   5. Recorder (records clicks → builds tour tree)
//   6. Runner (walks a saved tour, draws ring + tooltip per step)
//   7. FAB (the user's only direct entry point)
//   8. Bootstrap: mount FAB, hook route changes, expose window API
//
// Coding conventions:
//   - All UI strings inline (no i18n in Phase 2 — Phase 3 may add).
//   - All measurements in px; never assume rem/em.
//   - All event listeners use { passive: false } when we need to
//     stopPropagation (the picker steals clicks so the host doesn't
//     navigate during recording).

(function () {
  'use strict';

  // ---------------------------------------------------------------
  // 1. Boot guard + Shadow DOM mount
  // ---------------------------------------------------------------

  // Boot guard — multiple <script> tags must not double-init.
  // The mount observer below handles re-attachment if the
  // overlay gets stripped from body; we don't re-run the whole
  // setup, just the appendChild.
  if (window.__nexusTourMounted) {
    if (window.__nexusTourRemount) window.__nexusTourRemount();
    return;
  }
  window.__nexusTourMounted = true;

  // The single overlay element — appended to document.body once.
  // pointer-events on the root is `none` so host clicks pass
  // through everywhere the agent isn't actively presenting UI;
  // individual interactive elements toggle their own to `auto`.
  const overlayHost = document.createElement('nexus-tour-overlay');
  overlayHost.style.cssText = [
    'all: initial',
    'position: fixed',
    'inset: 0',
    'z-index: 2147483647',
    'pointer-events: none',
    'font: 14px/1.4 system-ui, -apple-system, sans-serif',
    'color: #111',
  ].join('; ');
  const shadow = overlayHost.attachShadow({ mode: 'closed' });

  // Top-level container inside the shadow tree. Three children:
  //   .fab        — always-on launcher
  //   .surface    — record/play UI mounted here on demand
  //   .pickerHi   — element-picker highlight rectangle (when active)
  const root = document.createElement('div');
  root.style.cssText = 'position: absolute; inset: 0; pointer-events: none;';
  shadow.appendChild(root);

  // Shared stylesheet for everything inside the shadow root.
  // Inline CSS rather than <style> tag so it's harder to break by
  // accidental host-side stylesheet edits.
  const style = document.createElement('style');
  style.textContent = `
    :host { all: initial; }
    * { box-sizing: border-box; font-family: inherit; }
    .fab {
      position: fixed; right: 16px; bottom: 16px;
      background: #111; color: #fff;
      padding: 10px 14px; border-radius: 999px;
      box-shadow: 0 4px 12px rgba(0,0,0,.25);
      cursor: pointer; pointer-events: auto;
      display: flex; align-items: center; gap: 6px;
      font: 14px/1 system-ui, sans-serif; user-select: none;
    }
    .fab:hover { background: #000; }
    .fab .dot { width: 6px; height: 6px; border-radius: 50%; background: #6f6; }
    .fab.recording .dot { background: #f55; animation: pulse 1s infinite; }
    @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.4} }

    .menu {
      position: fixed; right: 16px; bottom: 64px;
      background: #fff; border-radius: 8px;
      box-shadow: 0 8px 24px rgba(0,0,0,.18);
      padding: 8px;
      min-width: 320px;       /* enough for "▶ Play: Tour for /some/path" + 📄 */
      max-width: 420px;       /* don't grow absurdly when tour names are long */
      pointer-events: auto;
    }
    .menu button {
      display: block; width: 100%; text-align: left;
      background: transparent; border: 0; padding: 9px 12px;
      border-radius: 6px; cursor: pointer; font: inherit; color: #111;
      white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
    }
    .menu button:hover { background: #f3f4f6; }
    .menu .hint { padding: 6px 12px; color: #666; font-size: 12px; }

    .recbar {
      position: fixed; top: 12px; left: 50%; transform: translateX(-50%);
      background: #111; color: #fff; padding: 10px 14px;
      border-radius: 999px; box-shadow: 0 4px 12px rgba(0,0,0,.25);
      display: flex; align-items: center; gap: 12px; pointer-events: auto;
      font: 13px/1 system-ui, sans-serif;
    }
    .recbar button {
      background: #fff; color: #111; border: 0;
      padding: 6px 10px; border-radius: 6px; cursor: pointer; font: inherit;
    }
    .recbar button.danger { background: #ef4444; color: #fff; }
    .recbar .count { background: #6f6; color: #111; padding: 2px 8px; border-radius: 999px; font-weight: 600; }

    /* Picker highlight: outline (not border) so it doesn't grow
       the box and shift hit-testing. box-sizing reset guards
       against host CSS that sets box-sizing: content-box on *. */
    .picker-hi {
      position: fixed; pointer-events: none;
      outline: 2px solid #2563eb;
      outline-offset: 1px;
      background: rgba(37,99,235,.10);
      border-radius: 4px;
      box-sizing: border-box;
      transition: all 80ms;
    }
    .picker-label {
      position: fixed; pointer-events: none;
      background: #2563eb; color: #fff;
      padding: 3px 8px; border-radius: 4px;
      font: 12px/1.3 system-ui, sans-serif;
      max-width: 320px;
      overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
      box-sizing: border-box;
    }

    .play-ring {
      position: fixed; pointer-events: none;
      border: 3px solid #f59e0b;
      border-radius: 6px;
      box-shadow: 0 0 0 9999px rgba(0,0,0,.35);
      transition: all 250ms;
    }
    /* Badge: line-height matches container height so the digit
       centres reliably (flex + line-height:1 left a visible
       optical offset because most sans-serif fonts have
       asymmetric ascent/descent — the glyph appeared top-heavy).
       text-align handles horizontal; line-height handles
       vertical. min-width keeps single digits as a circle and
       lets multi-digit numbers (10, 11, …) stretch into a pill
       instead of overflowing. */
    .play-badge {
      position: fixed; pointer-events: none;
      background: #f59e0b; color: #111;
      font-family: system-ui, -apple-system, sans-serif;
      font-weight: 700; font-size: 14px;
      line-height: 28px;
      min-width: 28px; height: 28px; padding: 0 8px;
      border-radius: 999px;
      text-align: center;
      box-shadow: 0 2px 6px rgba(0,0,0,.3);
      box-sizing: border-box;
      white-space: nowrap;
      display: inline-block;
      font-variant-numeric: tabular-nums;
    }
    .play-tip {
      position: fixed; pointer-events: auto;
      background: #fff; color: #111;
      padding: 12px 14px; border-radius: 8px;
      box-shadow: 0 8px 24px rgba(0,0,0,.25);
      max-width: 320px; min-width: 220px;
    }
    .play-tip h4 { margin: 0 0 6px; font-size: 14px; }
    .play-tip p  { margin: 0 0 10px; font-size: 13px; color: #444; line-height: 1.4; }
    .play-tip .row { display: flex; justify-content: space-between; gap: 8px; }
    .play-tip button {
      background: #111; color: #fff; border: 0;
      padding: 6px 12px; border-radius: 6px; cursor: pointer; font: inherit;
    }
    .play-tip button.ghost { background: transparent; color: #666; }

    .editor {
      position: fixed; right: 16px; top: 60px; width: 320px;
      background: #fff; border-radius: 10px;
      box-shadow: 0 12px 32px rgba(0,0,0,.18);
      pointer-events: auto; padding: 14px;
    }
    .editor label { display: block; font-size: 12px; color: #555; margin: 8px 0 4px; }
    .editor input, .editor textarea {
      width: 100%; padding: 6px 8px; font: inherit;
      border: 1px solid #d4d4d8; border-radius: 6px;
    }
    .editor textarea { min-height: 64px; resize: vertical; }
    .editor .actions { display: flex; gap: 8px; margin-top: 12px; justify-content: flex-end; }
    .editor button { padding: 6px 12px; border-radius: 6px; border: 0; cursor: pointer; font: inherit; }
    .editor button.primary { background: #111; color: #fff; }
    .editor button.ghost { background: transparent; color: #555; }
  `;
  shadow.appendChild(style);

  // mountOverlay attaches <nexus-tour-overlay> to document.body
  // AND keeps it there. A bare appendChild isn't enough — host
  // frontends routinely remove or relocate our element via:
  //
  //   - SPA route changes that swap body.innerHTML wholesale
  //     (some Inertia + manual-DOM patterns do this)
  //   - Vue 3 Teleports + Vuetify portals briefly detaching or
  //     reparenting siblings during transitions
  //   - Any library doing document.body.replaceChildren(...)
  //   - document.body being replaced wholesale (rare, but
  //     a few Vue 3 + custom-element combos do this on mount)
  //
  // Symptom: the FAB disappears mid-session, and even a soft
  // refresh doesn't bring it back because the stale browser-
  // cached script never expected to need to recover.
  //
  // Defense layers, in order of cost:
  //   1. Observe documentElement.childList so a body swap is
  //      caught (re-observe the new body when it appears).
  //   2. Observe the live body's childList so direct-child
  //      removals trigger a re-append.
  //   3. ensureMounted() asserts both "is connected" AND
  //      "parent is document.body" — Vue Teleports can move
  //      us under a non-body node, which counts as broken.
  //   4. A 30-second post-load setInterval safety net catches
  //      any race the observers missed during early init when
  //      body might not be live yet.
  let mountObserver = null;
  let docObserver = null;

  function ensureMounted() {
    if (!document.body) return;
    if (overlayHost.parentNode !== document.body) {
      document.body.appendChild(overlayHost);
    }
  }

  function observeBody() {
    if (mountObserver) mountObserver.disconnect();
    mountObserver = new MutationObserver(ensureMounted);
    mountObserver.observe(document.body, { childList: true });
  }

  function mountOverlay() {
    if (!document.body) {
      document.addEventListener('DOMContentLoaded', mountOverlay, { once: true });
      return;
    }
    ensureMounted();
    observeBody();

    if (!docObserver) {
      // The body element itself can be replaced (rare). Watch
      // documentElement's direct children and re-bind the body
      // observer whenever a new body shows up.
      docObserver = new MutationObserver(() => {
        observeBody();
        ensureMounted();
      });
      docObserver.observe(document.documentElement, { childList: true });
    }

    // Belt-and-suspenders: poll for 30s after mount in case the
    // observers were set up while the host was still tearing
    // the body around during early init.
    let ticks = 0;
    const safety = setInterval(() => {
      ensureMounted();
      if (++ticks >= 30) clearInterval(safety);
    }, 1000);
  }

  // ---------------------------------------------------------------
  // 2. Backend API client
  // ---------------------------------------------------------------
  //
  // All endpoints live under /__nexus/tour/. The dashboard CRUD
  // routes (POST/DELETE) live under /__nexus/tour/tours/... — the
  // in-page agent uses the same paths.

  const API = {
    listActive(route) {
      return fetch(`/__nexus/tour/active?route=${encodeURIComponent(route)}`)
        .then(r => r.ok ? r.json() : Promise.reject(new Error('list tours failed')))
        .then(j => j.tours || []);
    },
    upsertTour(tour) {
      return fetch('/__nexus/tour/tours', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(tour),
      }).then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t))));
    },
    deleteTour(id) {
      return fetch(`/__nexus/tour/tours/${encodeURIComponent(id)}`, { method: 'DELETE' })
        .then(r => r.ok ? r.json() : Promise.reject(new Error('delete failed')));
    },
  };

  // ---------------------------------------------------------------
  // 3. Element picker
  // ---------------------------------------------------------------
  //
  // Listens at the document level (capture phase) so the host's
  // own click handlers don't fire during a pick. On move, draws a
  // highlight rect over whichever element the mouse is under. On
  // click, resolves with that element. Escape cancels.

  const PICKER_EXCLUDE = 'nexus-tour-overlay';

  function describeElement(el) {
    const tag = el.tagName.toLowerCase();
    const txt = (el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 40);
    return txt ? `${tag} "${txt}"` : tag;
  }

  // buildSelector returns a "reasonably stable" CSS selector for
  // the element. Order of preference:
  //   1. data-tour-id attribute (operator-curated)
  //   2. unique id
  //   3. structural path with tag + 1-3 stable classes +
  //      :nth-of-type to disambiguate siblings
  // The third path bottoms out at the tag name if nothing else
  // narrows it — better than failing the recording.
  const AUTO_CLASS_RE = /^(v-|el-)?[a-z0-9]+(-[a-z0-9]+)*--[a-z0-9]{5,}$/i;
  function pickClasses(el) {
    return Array.from(el.classList || [])
      .filter(c => !AUTO_CLASS_RE.test(c))
      .filter(c => !c.startsWith('v-enter') && !c.startsWith('v-leave'))
      .slice(0, 3);
  }
  function buildSelector(el) {
    const tourId = el.getAttribute && el.getAttribute('data-tour-id');
    if (tourId) return `[data-tour-id="${CSS.escape(tourId)}"]`;
    if (el.id && !/^\d/.test(el.id) && document.querySelectorAll(`#${CSS.escape(el.id)}`).length === 1) {
      return `#${CSS.escape(el.id)}`;
    }
    const parts = [];
    let node = el;
    while (node && node.nodeType === 1 && node !== document.body && node !== document.documentElement) {
      let part = node.tagName.toLowerCase();
      if (node.id && !/^\d/.test(node.id)) {
        parts.unshift(`#${CSS.escape(node.id)}`);
        break;
      }
      const classes = pickClasses(node);
      if (classes.length) part += '.' + classes.map(c => CSS.escape(c)).join('.');
      const parent = node.parentElement;
      if (parent) {
        const same = Array.from(parent.children).filter(s => s.tagName === node.tagName);
        if (same.length > 1) part += `:nth-of-type(${same.indexOf(node) + 1})`;
      }
      parts.unshift(part);
      const probe = parts.join(' > ');
      try { if (document.querySelectorAll(probe).length === 1) return probe; } catch { /* malformed intermediate */ }
      node = node.parentElement;
    }
    return parts.join(' > ') || el.tagName.toLowerCase();
  }

  function createPicker() {
    const hi = document.createElement('div');
    hi.className = 'picker-hi';
    hi.style.display = 'none';
    const label = document.createElement('div');
    label.className = 'picker-label';
    label.style.display = 'none';
    root.appendChild(hi);
    root.appendChild(label);

    let active = false;
    let onPick = null;
    let onCancel = null;

    function isOurs(el) {
      return el && (el === overlayHost || (el.closest && el.closest(PICKER_EXCLUDE)));
    }

    function moveTo(rect, text) {
      hi.style.display = 'block';
      hi.style.left = rect.left + 'px';
      hi.style.top = rect.top + 'px';
      hi.style.width = rect.width + 'px';
      hi.style.height = rect.height + 'px';
      label.style.display = 'block';
      label.style.left = rect.left + 'px';
      label.style.top = Math.max(0, rect.top - 24) + 'px';
      label.textContent = text;
    }

    // hitTest finds the element under the cursor. We prefer
    // document.elementFromPoint(x, y) over e.target because
    // disabled <button>/<input>/etc. don't fire click events
    // and may even let pointer events drop down to their
    // parent — elementFromPoint gives us the true topmost
    // element regardless of disabled state. We hide our own
    // overlay's pointer-events at the host attribute level
    // already, so it won't show up as the topmost hit.
    function hitTest(e) {
      // Try elementFromPoint first; fall back to e.target if
      // for some reason the coords aren't usable yet.
      let t = document.elementFromPoint(e.clientX, e.clientY);
      if (!t) t = e.target;
      // Skip text nodes and similar — picker only targets
      // elements.
      while (t && t.nodeType !== 1) t = t.parentNode;
      return t;
    }

    function onMove(e) {
      const t = hitTest(e);
      if (!t || isOurs(t)) { hi.style.display = 'none'; label.style.display = 'none'; return; }
      moveTo(t.getBoundingClientRect(), describeElement(t));
    }
    // onDown handles selection. pointerdown is used instead of
    // click because:
    //   1. Disabled form controls (button, input, select,
    //      textarea, option) silently swallow click events but
    //      still fire pointerdown — operators frequently want
    //      to point at a disabled "Submit" button to explain
    //      why it's disabled, and without this fix that's
    //      impossible.
    //   2. Capture phase means we run before the host's own
    //      onClick handlers, so the host doesn't navigate
    //      during a recording pick.
    function onDown(e) {
      if (e.button !== undefined && e.button !== 0) return; // left click only
      const t = hitTest(e);
      if (!t || isOurs(t)) return;
      e.preventDefault();
      e.stopPropagation();
      e.stopImmediatePropagation();
      // Also suppress the trailing click + mouseup so the host
      // can't react to either. Belt + suspenders.
      ['click', 'mouseup'].forEach(ev => {
        document.addEventListener(ev, suppressOnce, true);
      });
      const rect = t.getBoundingClientRect();
      const out = {
        selector: buildSelector(t),
        label: describeElement(t),
        rect: { left: rect.left, top: rect.top, width: rect.width, height: rect.height },
      };
      if (onPick) onPick(out);
    }
    // suppressOnce eats the next click/mouseup that lands after
    // a pointerdown pick — otherwise the host's onClick can fire
    // on the trailing click and navigate the page mid-recording.
    function suppressOnce(e) {
      e.preventDefault();
      e.stopPropagation();
      e.stopImmediatePropagation();
      ['click', 'mouseup'].forEach(ev => {
        document.removeEventListener(ev, suppressOnce, true);
      });
    }
    function onKey(e) { if (e.key === 'Escape' && onCancel) onCancel(); }

    return {
      start(onPickCb, onCancelCb) {
        if (active) return;
        active = true;
        onPick = onPickCb; onCancel = onCancelCb;
        document.addEventListener('mousemove', onMove, true);
        document.addEventListener('pointerdown', onDown, true);
        document.addEventListener('keydown', onKey, true);
      },
      stop() {
        if (!active) return;
        active = false;
        hi.style.display = 'none';
        label.style.display = 'none';
        document.removeEventListener('mousemove', onMove, true);
        document.removeEventListener('pointerdown', onDown, true);
        document.removeEventListener('keydown', onKey, true);
        onPick = null; onCancel = null;
      },
    };
  }

  const picker = createPicker();

  // ---------------------------------------------------------------
  // 4. Screenshot capture — lazy CDN-loaded html2canvas-pro
  // ---------------------------------------------------------------
  //
  // html2canvas is the only heavyweight dependency. We pull it on
  // demand the first time the user starts recording so the static
  // inject.js stays small on pages that don't capture. esm.sh
  // serves the modern fork that handles oklch() + CSS layers.

  let _h2c = null;
  async function loadHtml2Canvas() {
    if (_h2c) return _h2c;
    const mod = await import('https://esm.sh/html2canvas-pro@1.5.8');
    _h2c = mod.default || mod;
    return _h2c;
  }

  // captureClean takes a screenshot of the FULL document body
  // with no overlays. We capture the whole document (not just
  // the viewport) and pair it with step rects stored in
  // DOCUMENT coordinates — that way badges land on the right
  // spot regardless of where the operator was scrolled when
  // they clicked. Earlier viewport-only path was fragile:
  // html2canvas's x/y options aren't reliable, so the image
  // and rects ended up using different origins and badges
  // clustered in the wrong corner.
  async function captureClean() {
    const h2c = await loadHtml2Canvas();
    overlayHost.style.visibility = 'hidden';
    let canvas;
    try {
      canvas = await h2c(document.body, {
        backgroundColor: '#ffffff',
        scale: Math.min(window.devicePixelRatio || 1, 2),
        logging: false,
        useCORS: true,
      });
    } finally {
      overlayHost.style.visibility = '';
    }
    return canvas.toDataURL('image/png');
  }

  // captureWithBadge takes a screenshot of the WHOLE page and
  // overlays a coloured ring + numbered badge centred on the
  // target's bounding rect. Returns a data: URL PNG.
  //
  // We render the host body, then composite the highlight onto a
  // copy of the canvas — drawing on the live DOM before snapshot
  // would force the agent's chrome (FAB, picker) into the
  // recording, which is exactly what we don't want.
  async function captureWithBadge(targetRect, badgeNum) {
    const h2c = await loadHtml2Canvas();
    // Hide our own overlay so it doesn't end up in the screenshot.
    overlayHost.style.visibility = 'hidden';
    let canvas;
    try {
      canvas = await h2c(document.body, {
        backgroundColor: '#ffffff',
        scale: Math.min(window.devicePixelRatio || 1, 2),
        logging: false,
        useCORS: true,
      });
    } finally {
      overlayHost.style.visibility = '';
    }
    const ctx = canvas.getContext('2d');
    const s = canvas.width / document.documentElement.clientWidth;
    const x = targetRect.left * s;
    const y = targetRect.top * s;
    const w = targetRect.width * s;
    const h = targetRect.height * s;
    // Ring
    ctx.lineWidth = Math.max(3, 3 * s);
    ctx.strokeStyle = '#f59e0b';
    ctx.beginPath();
    ctx.roundRect ? ctx.roundRect(x - 4, y - 4, w + 8, h + 8, 6 * s) : ctx.rect(x - 4, y - 4, w + 8, h + 8);
    ctx.stroke();
    // Numbered badge — circle just above the target's top-left corner.
    const bx = x - 14 * s;
    const by = y - 14 * s;
    const br = 18 * s;
    ctx.fillStyle = '#f59e0b';
    ctx.beginPath();
    ctx.arc(bx + br, by + br, br, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = '#111';
    ctx.font = `bold ${22 * s}px system-ui, sans-serif`;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(String(badgeNum), bx + br, by + br + 1);
    return canvas.toDataURL('image/png');
  }

  // ---------------------------------------------------------------
  // 5. Recorder
  // ---------------------------------------------------------------
  //
  // Record mode:
  //   - Toolbar at top with step count + Stop / Cancel.
  //   - Each click via the picker appends a step.
  //   - Auto-substep: if the new click's rect is INSIDE the
  //     previous step's rect, it becomes a child rather than a
  //     sibling. Operators can flatten manually in the dashboard.
  //   - Stop: save the whole tour via API.upsertTour.

  function createRecorder() {
    let active = false;
    let paused = false;     // set by togglePause(); picker is off while true
    let tour = null;        // { id?, name, route, description, steps: [] }
    let lastStep = null;    // for substep-rect heuristic
    let badge = 0;          // running badge number for screenshots
    let bar = null;         // .recbar element

    function rectContains(outer, inner) {
      if (!outer || !inner) return false;
      return (
        inner.left >= outer.left - 2 &&
        inner.top >= outer.top - 2 &&
        inner.left + inner.width <= outer.left + outer.width + 2 &&
        inner.top + inner.height <= outer.top + outer.height + 2
      );
    }

    function buildBar() {
      const b = document.createElement('div');
      b.className = 'recbar';
      b.innerHTML = `
        <span class="status">● Recording</span>
        <span class="count">0</span>
        <button class="pause">⏸ Pause</button>
        <button class="edit-last" disabled>✎ Edit last step</button>
        <button class="stop">Stop &amp; Save</button>
        <button class="danger cancel">Cancel</button>
      `;
      root.appendChild(b);
      b.querySelector('.stop').addEventListener('click', () => stop(true));
      b.querySelector('.cancel').addEventListener('click', () => stop(false));
      b.querySelector('.edit-last').addEventListener('click', () => editLastStep());
      b.querySelector('.pause').addEventListener('click', () => togglePause());
      return b;
    }

    // togglePause flips picker capture on/off without ending the
    // recording. While paused the operator can navigate the host
    // freely — log in, fill a form, open a drawer — without those
    // clicks becoming phantom steps. Hitting Resume re-arms the
    // picker for the next intentional click.
    function togglePause() {
      if (!active) return;
      paused = !paused;
      const btn = bar.querySelector('.pause');
      const status = bar.querySelector('.status');
      if (paused) {
        picker.stop();
        btn.textContent = '▶ Resume';
        status.textContent = '⏸ Paused';
        fab.classList.remove('recording');
        // Visual cue inside the bar so the dim host page reads
        // as "we're waiting on you" instead of "recording".
        bar.style.background = '#f59e0b';
        bar.style.color = '#111';
      } else {
        btn.textContent = '⏸ Pause';
        status.textContent = '● Recording';
        fab.classList.add('recording');
        bar.style.background = '';
        bar.style.color = '';
        picker.start(onPicked, () => stop(false));
      }
    }

    // editLastStep delegates to the shared openStepEditor —
    // same form, same positioning rules, just with
    // required=false so Cancel doesn't remove the step.
    function editLastStep() {
      if (!lastStep) return;
      picker.stop();
      openStepEditor(lastStep, lastStep._rect || null, /* required = */ false);
    }

    function updateCount() {
      if (!bar) return;
      // Count all nodes in the tree (parents + children).
      function countTree(steps) {
        let n = steps.length;
        for (const s of steps) if (s.children) n += countTree(s.children);
        return n;
      }
      const total = countTree(tour.steps);
      bar.querySelector('.count').textContent = String(total);
      // "Edit last step" is meaningless before the first capture
      // — toggle the disabled state along with the counter so
      // the affordance only lights up when there's something
      // to edit.
      const editBtn = bar.querySelector('.edit-last');
      if (editBtn) editBtn.disabled = total === 0;
    }

    function onPicked(p) {
      // Stop picker — auto-editor will re-arm after the operator
      // writes the step's text.
      picker.stop();
      // Build step — rect fields persist in DOCUMENT coordinates
      // (viewport rect + current scroll offset) so badges land
      // on the right spot in the cover image regardless of where
      // the operator was scrolled when they clicked.
      const docLeft = p.rect.left + window.scrollX;
      const docTop  = p.rect.top  + window.scrollY;
      const step = {
        selector: p.selector,
        label: p.label,
        title: `Step ${++badge}`,
        text: '', // empty by default — editor requires the operator to fill it
        placement: 'bottom',
        children: [],
        rect_left:   Math.round(docLeft),
        rect_top:    Math.round(docTop),
        rect_width:  Math.round(p.rect.width),
        rect_height: Math.round(p.rect.height),
        // _rect kept for the in-recorder substep heuristic only.
        _rect: p.rect,
      };
      // Capture screenshot with badge (async — non-blocking).
      captureWithBadge(p.rect, badge)
        .then(url => { step.media_url = url; })
        .catch(err => console.warn('tour: capture failed', err));

      // Substep heuristic: click inside previous step's rect → child.
      if (lastStep && rectContains(lastStep._rect, p.rect)) {
        lastStep.children.push(step);
      } else {
        tour.steps.push(step);
      }
      lastStep = step;
      updateCount();
      // Force the operator to label this step before the next
      // click. openStepEditor blocks the picker until they Save
      // (with non-empty text) or Cancel (removes the step).
      openStepEditor(step, p.rect, /* required = */ true);
    }

    // openStepEditor renders the small in-page form for a step.
    // When required=true, the Save button is disabled until the
    // text field is non-empty, and Cancel removes the just-
    // captured step (so the operator doesn't accumulate empty
    // entries). When required=false (used by the "Edit last
    // step" toolbar button), Cancel just closes without removing.
    //
    // Positioning: the card avoids overlapping the picked
    // element's rect when one is supplied. If the rect occupies
    // the right half of the viewport, the card opens on the
    // left, and vice-versa. Vertical position is also clamped
    // so the card never escapes the viewport.
    function openStepEditor(step, anchorRect, required) {
      const editor = document.createElement('div');
      editor.className = 'editor';
      editor.innerHTML = `
        <div style="font-weight:600;margin-bottom:6px">${required ? '✏ Describe step ' + step.title : 'Edit step'}</div>
        <label>Title</label>
        <input class="ed-title" value="${escapeHtml(step.title || '')}" />
        <label>Text shown to the user <span style="color:#ef4444">*</span></label>
        <textarea class="ed-text" placeholder="What does this step do? (required)">${escapeHtml(step.text || '')}</textarea>
        <label>Placement</label>
        <select class="ed-place">
          <option value="bottom">bottom</option>
          <option value="top">top</option>
          <option value="left">left</option>
          <option value="right">right</option>
        </select>
        <div class="actions">
          <button class="ghost ed-cancel">${required ? 'Discard step' : 'Cancel'}</button>
          <button class="primary ed-save" disabled>Save</button>
        </div>
      `;
      root.appendChild(editor);
      editor.querySelector('.ed-place').value = step.placement || 'bottom';

      // Position the editor away from the picked element.
      positionEditor(editor, anchorRect);

      const titleEl = editor.querySelector('.ed-title');
      const textEl  = editor.querySelector('.ed-text');
      const placeEl = editor.querySelector('.ed-place');
      const saveBtn = editor.querySelector('.ed-save');
      // Live-validate text presence so the Save button enables
      // the moment the operator types anything (no empty saves).
      function updateSaveEnabled() {
        saveBtn.disabled = textEl.value.trim().length === 0;
      }
      updateSaveEnabled();
      textEl.addEventListener('input', updateSaveEnabled);
      // Focus the text area so the operator can start typing
      // immediately — that's the only required field.
      setTimeout(() => textEl.focus(), 0);

      function close(rearm) {
        editor.remove();
        if (active && !paused && rearm) {
          picker.start(onPicked, () => stop(false));
        }
      }
      editor.querySelector('.ed-cancel').addEventListener('click', () => {
        if (required) {
          // Discarding a freshly-captured step — remove it from
          // the tour so we don't accumulate empties.
          removeStep(step);
          badge = Math.max(0, badge - 1);
          updateCount();
        }
        close(true);
      });
      saveBtn.addEventListener('click', () => {
        step.title = titleEl.value || step.title;
        step.text  = textEl.value.trim();
        step.placement = placeEl.value;
        close(true);
      });
    }

    // positionEditor anchors the editor opposite to the picked
    // element. Same as Shepherd's flip-on-overlap heuristic but
    // simpler — three regions (left half / right half / no
    // anchor) and clamp to viewport.
    function positionEditor(editor, anchorRect) {
      const margin = 16;
      const cardW = 320; // matches .editor width
      // Read offsetHeight after a tick so the browser has laid
      // out the textarea + buttons.
      requestAnimationFrame(() => {
        const cardH = editor.offsetHeight || 280;
        let left, top;
        if (anchorRect && anchorRect.width && anchorRect.height) {
          const midX = anchorRect.left + anchorRect.width / 2;
          // If the picked element is in the LEFT half of the
          // viewport, open the editor on the RIGHT, and vice-
          // versa. Avoids covering the thing the operator just
          // pointed at.
          if (midX < window.innerWidth / 2) {
            left = window.innerWidth - cardW - margin;
          } else {
            left = margin;
          }
          // Try to vertically centre on the anchor; clamp to
          // viewport so the bottom doesn't escape.
          top = anchorRect.top + anchorRect.height / 2 - cardH / 2;
        } else {
          // No anchor (Edit last step from toolbar) — top-right.
          left = window.innerWidth - cardW - margin;
          top  = 60;
        }
        top  = Math.max(margin, Math.min(window.innerHeight - cardH - margin, top));
        left = Math.max(margin, Math.min(window.innerWidth - cardW - margin, left));
        editor.style.left = left + 'px';
        editor.style.top  = top + 'px';
        editor.style.right = 'auto'; // override the default right:16px from CSS
      });
    }

    // removeStep deletes a step from the tree (root or substep).
    // Used when the operator discards a freshly-captured step.
    function removeStep(step) {
      function walk(arr) {
        const i = arr.indexOf(step);
        if (i >= 0) { arr.splice(i, 1); return true; }
        for (const s of arr) {
          if (s.children && walk(s.children)) return true;
        }
        return false;
      }
      walk(tour.steps);
      // lastStep ↔ the popped item — if that's what we removed,
      // bump back to the previous tail.
      if (lastStep === step) {
        const flat = [];
        function collect(arr) {
          for (const s of arr) {
            flat.push(s);
            if (s.children) collect(s.children);
          }
        }
        collect(tour.steps);
        lastStep = flat[flat.length - 1] || null;
      }
    }

    async function start(routePath) {
      if (active) return;
      active = true;
      paused = false;
      lastStep = null;
      badge = 0;
      tour = {
        name: `Tour for ${routePath}`,
        route: routePath,
        description: '',
        // base_* match the FULL document we capture (not just
        // the viewport) — scrollWidth × scrollHeight. Step
        // rects are stored in document coords too, so badges
        // align with the cover image regardless of where the
        // operator was scrolled when they clicked.
        base_width:  document.documentElement.scrollWidth  || window.innerWidth,
        base_height: document.documentElement.scrollHeight || window.innerHeight,
        steps: [],
      };
      bar = buildBar();
      fab.classList.add('recording');
      // Capture the clean cover image first (no overlays) — this
      // is what the single-tour PDF preview lays badges over.
      // Best-effort: if html2canvas fails, the preview falls
      // back to per-step screenshots.
      captureClean()
        .then(url => { tour.cover_image_url = url; })
        .catch(err => console.warn('tour: cover capture failed', err));
      // First click — kick off the picker; from now on each click
      // re-arms it inside onPicked.
      picker.start(onPicked, () => stop(false));
    }

    // stripInternals removes recorder-only scratch fields before
    // serialising the tour for save. The persisted rect_* ints
    // stay; _rect (the live DOMRect) goes.
    function stripRects(steps) {
      for (const s of steps) {
        delete s._rect;
        if (s.children && s.children.length) stripRects(s.children);
        else delete s.children;
      }
    }

    async function stop(save) {
      if (!active) return;
      active = false;
      picker.stop();
      if (bar) { bar.remove(); bar = null; }
      fab.classList.remove('recording');
      if (!save || !tour || tour.steps.length === 0) return;
      // Allow any in-flight captureWithBadge() promises to land
      // before we serialize — html2canvas is async and the badge
      // number is already correct, just give it a tick.
      await new Promise(r => setTimeout(r, 250));
      stripRects(tour.steps);
      try {
        await API.upsertTour(tour);
        toast('Tour saved.');
      } catch (e) {
        toast('Save failed: ' + (e.message || e), 'error');
      }
    }

    return { start, stop, isActive: () => active };
  }

  // ---------------------------------------------------------------
  // 6. Runner — walk a saved tour at play time
  // ---------------------------------------------------------------
  //
  // Tree walk:
  //   - DFS: parent → first child → its children → next sibling.
  //   - Each step: find element by selector, draw ring + badge,
  //     position tooltip beside it, wait for Next/Back/Close.
  //   - If the element is missing (the host UI changed since
  //     recording), show a "missing target" tooltip with the
  //     selector + Skip / Stop options.

  function createRunner() {
    let active = false;
    let ring = null, badgeEl = null, tip = null;
    let order = [];          // flattened DFS list of steps
    let idx = 0;

    function flatten(steps, out) {
      for (const s of steps) {
        out.push(s);
        if (s.children && s.children.length) flatten(s.children, out);
      }
    }

    function ensureChrome() {
      if (!ring) { ring = document.createElement('div'); ring.className = 'play-ring'; root.appendChild(ring); }
      if (!badgeEl) { badgeEl = document.createElement('div'); badgeEl.className = 'play-badge'; root.appendChild(badgeEl); }
      if (!tip)  { tip  = document.createElement('div'); tip.className  = 'play-tip';  root.appendChild(tip); }
      ring.style.display = 'block';
      badgeEl.style.display = 'block';
      tip.style.display  = 'block';
    }
    function hideChrome() {
      if (ring) ring.style.display = 'none';
      if (badgeEl) badgeEl.style.display = 'none';
      if (tip)  tip.style.display  = 'none';
    }

    // placeRing draws the orange outline + page-dim around the
    // target. We pad by 4px on every side so the ring is clearly
    // outside the target's border (not overlapping a button's
    // own border in a confusing way).
    function placeRing(rect) {
      ring.style.left = (rect.left - 4) + 'px';
      ring.style.top  = (rect.top - 4) + 'px';
      ring.style.width  = (rect.width + 8) + 'px';
      ring.style.height = (rect.height + 8) + 'px';
    }
    // placeBadge anchors the numbered pill at the target's
    // top-left corner. Clamped to viewport so a target at (0,0)
    // doesn't render the badge half-offscreen — the number
    // disappearing was the most common report. Width may exceed
    // 28px now (min-width pill), so we measure after assignment.
    function placeBadge(rect, n) {
      badgeEl.textContent = String(n);
      // Force layout to read the actual rendered width — the
      // pill min-widths to 28 but can grow for "10"+.
      const bw = badgeEl.offsetWidth || 28;
      const bh = badgeEl.offsetHeight || 28;
      let left = rect.left - Math.floor(bw / 2);
      let top  = rect.top - Math.floor(bh / 2);
      // Clamp so the entire pill stays inside the viewport
      // (8px margin on each edge so it doesn't kiss the edge).
      left = Math.max(8, Math.min(window.innerWidth - bw - 8, left));
      top  = Math.max(8, Math.min(window.innerHeight - bh - 8, top));
      badgeEl.style.left = left + 'px';
      badgeEl.style.top  = top + 'px';
    }
    function placeTip(rect, placement) {
      // Default: under the target, left-aligned. Tip width is
      // bounded by .play-tip CSS; we just pick coordinates.
      const margin = 12;
      let left = rect.left;
      let top  = rect.top + rect.height + margin;
      if (placement === 'top')   { top  = rect.top - margin - tip.offsetHeight; }
      if (placement === 'right') { left = rect.left + rect.width + margin; top = rect.top; }
      if (placement === 'left')  { left = rect.left - margin - tip.offsetWidth; top = rect.top; }
      // Clamp to viewport.
      left = Math.max(8, Math.min(window.innerWidth - 8 - 220, left));
      top  = Math.max(8, Math.min(window.innerHeight - 8 - 100, top));
      tip.style.left = left + 'px';
      tip.style.top  = top + 'px';
    }

    function renderTip(step, n, total, missing) {
      const titleText = step.title || `Step ${n}`;
      const bodyText  = step.text  || '';
      const navHTML = `
        <div class="row">
          <button class="ghost back" ${idx === 0 ? 'disabled' : ''}>Back</button>
          <span style="color:#888;font-size:12px;align-self:center">${n} / ${total}</span>
          ${missing
            ? `<button class="skip">Skip</button>`
            : `<button class="next">${idx === total - 1 ? 'Done' : 'Next'}</button>`}
        </div>
      `;
      const head = missing
        ? `<h4>⚠ Target missing</h4><p>Selector: <code>${escapeHtml(step.selector)}</code></p>`
        : `<h4>${escapeHtml(titleText)}</h4><p>${escapeHtml(bodyText)}</p>`;
      tip.innerHTML = head + navHTML;
      const back = tip.querySelector('.back');
      if (back) back.addEventListener('click', () => move(-1));
      const next = tip.querySelector('.next');
      if (next) next.addEventListener('click', () => move(+1));
      const skip = tip.querySelector('.skip');
      if (skip) skip.addEventListener('click', () => move(+1));
    }

    function show(i) {
      idx = i;
      if (i < 0 || i >= order.length) { stop(); return; }
      const s = order[i];
      const target = s.selector ? document.querySelector(s.selector) : null;
      ensureChrome();
      if (!target) {
        // Hide ring + badge for missing targets; keep tip with
        // selector + Skip.
        ring.style.display = 'none'; badgeEl.style.display = 'none';
        // Park tip in the centre of the screen.
        const fakeRect = { left: window.innerWidth/2 - 110, top: window.innerHeight/2 - 60, width: 220, height: 120 };
        tip.style.left = fakeRect.left + 'px';
        tip.style.top  = fakeRect.top + 'px';
        renderTip(s, i + 1, order.length, true);
        return;
      }
      // Scroll into view if needed — the user shouldn't have to
      // hunt for the highlighted control off-screen.
      target.scrollIntoView({ behavior: 'smooth', block: 'center', inline: 'nearest' });
      // The rect after scroll lands lazily, so re-measure on the
      // next animation frame.
      requestAnimationFrame(() => {
        const rect = target.getBoundingClientRect();
        placeRing(rect);
        placeBadge(rect, s.badge_number || (i + 1));
        renderTip(s, i + 1, order.length, false);
        placeTip(rect, s.placement || 'bottom');
      });
    }

    function move(delta) { show(idx + delta); }

    function start(tour) {
      if (active) return;
      active = true;
      order = [];
      flatten(tour.steps || [], order);
      if (order.length === 0) { active = false; toast('Tour has no steps.'); return; }
      show(0);
    }
    function stop() {
      active = false;
      hideChrome();
    }

    return { start, stop, isActive: () => active };
  }

  // ---------------------------------------------------------------
  // 7. FAB + menu
  // ---------------------------------------------------------------

  const fab = document.createElement('div');
  fab.className = 'fab';
  fab.innerHTML = `<span class="dot"></span><span>Tour</span>`;
  root.appendChild(fab);

  let menu = null;
  function closeMenu() { if (menu) { menu.remove(); menu = null; } }
  function openMenu(tours) {
    closeMenu();
    menu = document.createElement('div');
    menu.className = 'menu';
    const recordBtn  = `<button data-act="record">▶ Record new tour</button>`;
    const playBlock  = tours.length
      ? tours.map(t => `
          <div style="display:flex;gap:2px">
            <button data-act="play" data-id="${t.id}" style="flex:1">▶ Play: ${escapeHtml(t.name)}</button>
            <button data-act="preview" data-id="${t.id}" title="Open printable preview" style="padding:8px 10px">📄</button>
          </div>`).join('')
      : `<div class="hint">No tour saved for this page.</div>`;
    // Multi-preview entry — only meaningful with 2+ tours on the
    // current route. Opens every tour for this route stacked in
    // play order.
    const previewAllBtn = tours.length > 1
      ? `<button data-act="preview-all">📄 Preview all (${tours.length}) as PDF</button>`
      : '';
    menu.innerHTML = recordBtn + playBlock + previewAllBtn;
    root.appendChild(menu);
    menu.addEventListener('click', e => {
      const t = e.target.closest('button');
      if (!t) return;
      const act = t.getAttribute('data-act');
      const id  = t.getAttribute('data-id');
      closeMenu();
      if (act === 'record') return recorder.start(currentRoute());
      if (act === 'play') {
        const tour = tours.find(x => x.id === id);
        if (tour) runner.start(tour);
      }
      if (act === 'preview' && id) {
        window.open(`/__nexus/tour/tours/${encodeURIComponent(id)}/preview`, '_blank');
      }
      if (act === 'preview-all') {
        window.open(`/__nexus/tour/preview?route=${encodeURIComponent(currentRoute())}`, '_blank');
      }
    });
  }

  fab.addEventListener('click', async () => {
    if (recorder.isActive()) return; // FAB is a status indicator during recording
    if (runner.isActive()) { runner.stop(); return; }
    if (menu) { closeMenu(); return; }
    try {
      const tours = await API.listActive(currentRoute());
      openMenu(tours);
    } catch (e) {
      console.warn('tour: list failed', e);
      openMenu([]);
    }
  });

  // ---------------------------------------------------------------
  // 8. Bootstrap
  // ---------------------------------------------------------------

  function currentRoute() {
    // Phase 2 uses pathname only — query strings + hashes are
    // ignored so a tour for /admin/users plays on
    // /admin/users?sort=desc too. Phase 3 may add explicit
    // glob/regex pinning.
    return window.location.pathname || '/';
  }

  function toast(msg, kind) {
    const t = document.createElement('div');
    t.style.cssText = [
      'position:fixed','left:50%','bottom:80px','transform:translateX(-50%)',
      'background:' + (kind === 'error' ? '#ef4444' : '#111'),
      'color:#fff','padding:10px 14px','border-radius:8px',
      'pointer-events:none','font:13px/1 system-ui,sans-serif',
      'box-shadow:0 4px 12px rgba(0,0,0,.25)',
    ].join(';');
    t.textContent = msg;
    root.appendChild(t);
    setTimeout(() => t.remove(), 3000);
  }

  function escapeHtml(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  const recorder = createRecorder();
  const runner = createRunner();

  // Close menu on outside click (clicks land in the host, not
  // inside the shadow root).
  document.addEventListener('click', () => {
    // Note: shadow-DOM clicks don't bubble up to document with
    // composed=false targets, so the FAB's own click won't
    // trigger this. Only true outside-clicks fall through.
    if (menu) closeMenu();
  }, true);

  // Expose a minimal window API so an operator could trigger
  // record/play from the host's own UI (e.g. an in-app "Help"
  // button). Optional — the FAB is the default UX.
  window.nexusTour = {
    record: () => recorder.start(currentRoute()),
    play:   id => API.listActive(currentRoute()).then(ts => {
      const t = id ? ts.find(x => x.id === id) : ts[0];
      if (t) runner.start(t);
    }),
    stop:   () => { recorder.stop && recorder.stop(false); runner.stop(); },
  };

  // Allow a re-loaded script tag (Vite HMR in dev, manual
  // re-include, etc.) to nudge us back into body without going
  // through full init. The boot-guard branch at the top of this
  // IIFE calls __nexusTourRemount() and returns.
  window.__nexusTourRemount = mountOverlay;

  mountOverlay();
})();