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
    // getTour fetches the hydrated tour by id (steps tree +
    // covers + metadata). Used by the FAB menu's ✎ button to
    // load an existing tour into the recorder for "add more
    // scenes" mode.
    async getTour(id) {
      const r = await fetch(`/__nexus/tour/tours/${encodeURIComponent(id)}`);
      if (!r.ok) throw new Error(`get tour: HTTP ${r.status}`);
      return r.json();
    },
    // listActive returns a {matched, others} pair so the FAB
    // menu can render route-matched tours prominently and ALSO
    // surface every other tour as a fallback. Operators kept
    // hitting "No tour saved for this page" on dynamic routes
    // (e.g. /admin/users/42 vs the /admin/users/9 they recorded
    // on) and had no path forward without bouncing to the
    // dashboard.
    //
    // Two queries per call: the exact path (with trailing-slash
    // variant), then everything else. The unmatched fallback
    // only triggers when the route-specific query is empty, so
    // most calls stay one network request.
    async listActive(route) {
      const tried = [];
      const variants = [route];
      if (route.endsWith('/') && route !== '/') variants.push(route.replace(/\/+$/, ''));
      else if (!route.endsWith('/')) variants.push(route + '/');
      let matched = [];
      for (const v of variants) {
        tried.push(v);
        const r = await fetch(`/__nexus/tour/active?route=${encodeURIComponent(v)}`);
        if (!r.ok) continue;
        const j = await r.json();
        if ((j.tours || []).length > 0) { matched = j.tours; break; }
      }
      if (matched.length > 0) return { matched, others: [] };
      // No exact match — pull the whole catalogue so the
      // operator can still play / preview any tour from the
      // FAB instead of having to open the dashboard.
      console.log('tour: no tours for', tried, '— showing full catalogue');
      try {
        const r = await fetch('/__nexus/tour/tours');
        if (r.ok) {
          const j = await r.json();
          return { matched: [], others: j.tours || [] };
        }
      } catch (e) {
        console.warn('tour: catalogue fetch failed', e);
      }
      return { matched: [], others: [] };
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

    // lastHover tracks the most recently hovered target so the
    // Enter-key path can capture it without a click. Clicks
    // close transient UIs (dropdowns, menus, popovers) before
    // the picker's preventDefault even fires in some hosts;
    // keyboard capture sidesteps that entire class of bug.
    let lastHover = null;

    function onMove(e) {
      const t = hitTest(e);
      if (!t || isOurs(t)) { lastHover = null; hi.style.display = 'none'; label.style.display = 'none'; return; }
      lastHover = t;
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
    function onKey(e) {
      if (e.key === 'Escape' && onCancel) onCancel();
      // Enter captures the currently hovered element WITHOUT
      // a click — for transient UIs that close on outside
      // click (Vuetify v-menu, MUI Menu, Bootstrap dropdown,
      // etc.) this is the only way to reach items inside.
      // Skip when the focus is in an editable field so Enter
      // still inserts a newline / submits a form.
      if (e.key === 'Enter' && lastHover && !isInEditable(e)) {
        e.preventDefault();
        e.stopPropagation();
        e.stopImmediatePropagation();
        const rect = lastHover.getBoundingClientRect();
        const out = {
          selector: buildSelector(lastHover),
          label: describeElement(lastHover),
          rect: { left: rect.left, top: rect.top, width: rect.width, height: rect.height },
        };
        if (onPick) onPick(out);
      }
    }
    // isInEditable lets the picker leave Enter alone when the
    // operator is typing in a host input — important because
    // the host may have a search box inside a dropdown that
    // submits on Enter. composedPath sees through Shadow DOM
    // boundaries, so typing into our own editor UI is caught
    // too.
    function isInEditable(e) {
      const path = (e.composedPath && e.composedPath()) || [e.target];
      for (const el of path) {
        if (!el || !el.tagName) continue;
        if (el.isContentEditable) return true;
        if (/^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName)) return true;
      }
      return false;
    }

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

  // forceAnimationsComplete fires every CSS animation +
  // transition to its end state immediately. We inject a
  // !important stylesheet that zeroes durations + delays;
  // the browser snaps elements to their final keyframe in
  // one frame instead of playing them.
  //
  // Why not just waitForAnimations(): Vue/React SPAs use a
  // mix of CSS animations, JS-driven transitions, and IntersectionObserver-triggered
  // "fade-up" libraries (AOS, motion-one, GSAP, custom Vue
  // composables). document.getAnimations() catches the pure-
  // CSS ones but misses anything driven by JS that hasn't
  // started yet — so a staggered "card 5 fades in at +0.4s"
  // pattern can still grab the card pre-fade.
  //
  // Forcing duration:0 on every element guarantees: every
  // CSS-driven animation finishes; any JS-driven animation
  // that runs after our injection inherits a 0-duration
  // transition so it lands immediately too.
  //
  // Caller wraps capture work in returned `release()` callback
  // — restores stylesheet after.
  function forceAnimationsComplete() {
    const style = document.createElement('style');
    style.setAttribute('data-nexus-tour-freeze', '1');
    // !important + the universal selector + ::before / ::after
    // covers every animation surface CSS can produce.
    style.textContent = `
      *, *::before, *::after {
        animation-duration: 0.001s !important;
        animation-delay: 0s !important;
        animation-iteration-count: 1 !important;
        animation-fill-mode: forwards !important;
        transition-duration: 0s !important;
        transition-delay: 0s !important;
      }
    `;
    document.head.appendChild(style);
    // Force a reflow so the new rules take effect immediately.
    void document.body.offsetHeight;
    return () => style.remove();
  }

  // Convenience wrapper: settle animations, then yield two
  // animation frames so the browser has a chance to paint the
  // final state before html2canvas reads the DOM.
  async function settleForCapture() {
    const release = forceAnimationsComplete();
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
    return release;
  }

  // captureClean takes a screenshot of the FULL document body
  // with no overlays. We capture the whole document (not just
  // the viewport) and pair it with step rects stored in
  // DOCUMENT coordinates — that way badges land on the right
  // spot regardless of where the operator was scrolled when
  // they clicked.
  async function captureClean() {
    // Freeze + fast-forward all animations so we don't snapshot
    // a half-faded-in page (common on Vue/React SPAs with
    // staggered entry animations).
    const releaseAnims = await settleForCapture();
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
      releaseAnims();
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
    // Same animation freeze as captureClean — per-step
    // screenshots fired right after a click on a freshly-
    // rendered route otherwise grab mid-fade-in content.
    const releaseAnims = await settleForCapture();
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
      releaseAnims();
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
    let coverIndex = 0;     // current Tour.CoverImages[] index. Bumps on each
                            // successful resume so subsequent steps align with
                            // a freshly-captured screenshot showing whatever
                            // dropdown / modal / page state is now visible.

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
        <span class="status">● Recording <span style="opacity:.6;font-size:11px">(P pause · Enter capture hovered)</span></span>
        <span class="count">0</span>
        <button class="capture-scene" title="Take a fresh screenshot of the current page state — subsequent steps land on this new screenshot. Use after opening a dropdown / modal that wasn't visible at start.">📸 Capture scene</button>
        <button class="pause" title="Pause (P) — interact with the page (e.g. open a dropdown) then Resume to capture inside">⏸ Pause</button>
        <button class="reuse-step" title="Copy steps from an existing tour into this one">📋 Reuse step</button>
        <button class="edit-last" disabled>✎ Edit last step</button>
        <button class="stop">Stop &amp; Save</button>
        <button class="danger cancel">Cancel</button>
      `;
      root.appendChild(b);
      b.querySelector('.stop').addEventListener('click', () => stop(true));
      b.querySelector('.cancel').addEventListener('click', () => stop(false));
      b.querySelector('.edit-last').addEventListener('click', () => editLastStep());
      b.querySelector('.pause').addEventListener('click', () => togglePause());
      b.querySelector('.capture-scene').addEventListener('click', () => captureScene());
      b.querySelector('.reuse-step').addEventListener('click', () => openReusePicker());
      return b;
    }

    // captureScene grabs a fresh cover screenshot on demand and
    // bumps coverIndex so subsequent steps land on it. Used when
    // the operator has manually arranged the page (open
    // dropdown, expanded panel, scrolled section) and the
    // auto-capture-on-resume timing didn't catch it. Brief
    // toast confirms the new scene number.
    async function captureScene() {
      if (!active) return;
      const btn = bar && bar.querySelector('.capture-scene');
      if (btn) { btn.disabled = true; btn.textContent = '📸 Capturing…'; }
      try {
        const url = await captureClean();
        if (url && tour) {
          tour.cover_images = tour.cover_images || [];
          tour.cover_images.push(url);
          coverIndex = tour.cover_images.length - 1;
          toast(`Scene ${coverIndex + 1} captured`);
        }
      } catch (e) {
        console.warn('tour: captureScene failed', e);
        toast('Capture failed', 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = '📸 Capture scene'; }
      }
    }

    // openReusePicker is the "copy from existing tour" flow.
    // Operators starting a new tour often re-walk the same
    // initial path as an existing one (login → dashboard →
    // …). Re-recording the same clicks is tedious; this
    // picker lets them grab the prefix from any saved tour
    // and continue with fresh captures from there.
    //
    // Picker shape: modal in the agent's shadow DOM, lists
    // every saved tour, each row expandable to its step
    // tree. Multi-select via checkboxes; "Add N steps"
    // clones the selection into the active tour with fresh
    // ids and properly remapped cover_index references.
    async function openReusePicker() {
      if (!active) return;
      picker.stop();
      let catalogue;
      try {
        const r = await fetch('/__nexus/tour/tours');
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        catalogue = (await r.json()).tours || [];
      } catch (e) {
        toast('Could not load tours: ' + (e.message || e), 'error');
        if (!paused) picker.start(onPicked, () => stop(false));
        return;
      }
      // Exclude the active tour from the picker — reusing
      // steps from yourself just produces duplicates.
      const otherTours = catalogue.filter(t => t.id && t.id !== tour.id);
      if (!otherTours.length) {
        toast('No other tours to reuse from.', 'error');
        if (!paused) picker.start(onPicked, () => stop(false));
        return;
      }
      buildReusePicker(otherTours);
    }

    function buildReusePicker(otherTours) {
      const modal = document.createElement('div');
      modal.className = 'editor';
      modal.style.cssText = [
        'left: 50%', 'top: 50%',
        'transform: translate(-50%, -50%)',
        'right: auto', 'width: 520px', 'max-height: 76vh',
        'overflow: hidden', 'display: flex', 'flex-direction: column',
      ].join('; ') + ';';
      modal.innerHTML = `
        <div style="font-weight:600;font-size:14px;margin-bottom:6px">📋 Reuse steps from an existing tour</div>
        <div style="font-size:11px;color:#666;margin-bottom:8px">
          Steps you tick get COPIED into the current recording with fresh ids.
          Their screenshots come along automatically.
        </div>
        <div class="reuse-list" style="flex:1;overflow:auto;border:1px solid #e5e7eb;border-radius:6px;padding:6px;min-height:200px"></div>
        <div class="actions">
          <button class="ghost cancel">Cancel</button>
          <button class="primary submit" disabled>Add 0 steps</button>
        </div>
      `;
      root.appendChild(modal);

      const list = modal.querySelector('.reuse-list');
      const submitBtn = modal.querySelector('.submit');
      // selected: stepId → { tourId, srcStep } for resolving
      // on submit. We hydrate tours lazily on first expansion;
      // catalogue.tours[*] lacks the .steps tree.
      const selected = new Map();
      const tourCache = new Map(); // tourId → hydrated tour

      function updateSubmit() {
        submitBtn.disabled = selected.size === 0;
        submitBtn.textContent = `Add ${selected.size} step${selected.size === 1 ? '' : 's'}`;
      }

      otherTours.forEach(t => {
        const row = document.createElement('div');
        row.style.cssText = 'border-bottom:1px solid #f3f4f6;padding:6px 4px';
        const safeName = escapeHtml(t.name || `Tour ${t.id.slice(0, 6)}`);
        row.innerHTML = `
          <button class="tour-toggle" style="background:transparent;border:0;width:100%;text-align:left;padding:4px 6px;cursor:pointer;font:inherit;display:flex;align-items:center;gap:6px">
            <span class="caret" style="display:inline-block;width:10px;transition:transform 120ms">▶</span>
            <strong style="flex:1">${safeName}</strong>
            <span style="font-size:10px;color:#888">${escapeHtml(t.route || '')}</span>
          </button>
          <div class="tour-steps" style="display:none;padding:4px 0 4px 20px"></div>
        `;
        const toggle = row.querySelector('.tour-toggle');
        const stepsBox = row.querySelector('.tour-steps');
        const caret = row.querySelector('.caret');
        toggle.addEventListener('click', async () => {
          if (stepsBox.style.display === 'none') {
            stepsBox.style.display = 'block';
            caret.style.transform = 'rotate(90deg)';
            if (!stepsBox.dataset.loaded) {
              stepsBox.innerHTML = '<div style="color:#888;font-size:11px">Loading…</div>';
              try {
                const r = await fetch(`/__nexus/tour/tours/${encodeURIComponent(t.id)}`);
                if (!r.ok) throw new Error(`HTTP ${r.status}`);
                const full = await r.json();
                tourCache.set(t.id, full);
                renderStepCheckboxes(stepsBox, full);
                stepsBox.dataset.loaded = '1';
              } catch (e) {
                stepsBox.innerHTML = `<div style="color:#ef4444;font-size:11px">Load failed: ${escapeHtml(e.message || String(e))}</div>`;
              }
            }
          } else {
            stepsBox.style.display = 'none';
            caret.style.transform = '';
          }
        });
        list.appendChild(row);
      });

      function renderStepCheckboxes(container, full) {
        container.innerHTML = '';
        const flat = [];
        function walk(arr, depth, prefix) {
          arr.forEach((s, i) => {
            const label = prefix ? `${prefix}.${i + 1}` : `${i + 1}`;
            flat.push({ step: s, depth, label });
            if (s.children && s.children.length) walk(s.children, depth + 1, label);
          });
        }
        walk(full.steps || [], 0, '');
        if (!flat.length) {
          container.innerHTML = '<div style="color:#888;font-size:11px">No steps in this tour.</div>';
          return;
        }
        flat.forEach(({ step, depth, label }) => {
          const item = document.createElement('label');
          item.style.cssText = `display:flex;gap:6px;align-items:flex-start;padding:3px 0 3px ${depth * 14}px;cursor:pointer;font-size:12px`;
          item.innerHTML = `
            <input type="checkbox" data-step-id="${escapeHtml(step.id)}" style="margin-top:2px;flex-shrink:0">
            <span style="background:#f59e0b;color:#111;font-weight:700;padding:0 6px;border-radius:999px;font-size:10px;line-height:18px;height:18px;display:inline-block">${escapeHtml(label)}</span>
            <span style="flex:1">${escapeHtml(step.title || step.label || 'Step ' + label)}</span>
          `;
          const cb = item.querySelector('input');
          cb.addEventListener('change', () => {
            if (cb.checked) selected.set(step.id, { tourId: full.id, srcStep: step });
            else selected.delete(step.id);
            updateSubmit();
          });
          container.appendChild(item);
        });
      }

      function close() { modal.remove(); }
      modal.querySelector('.cancel').addEventListener('click', () => {
        close();
        if (active && !paused) picker.start(onPicked, () => stop(false));
      });
      submitBtn.addEventListener('click', () => {
        const added = applyReuse(selected, tourCache);
        close();
        if (added > 0) toast(`Added ${added} step${added === 1 ? '' : 's'}.`);
        if (active && !paused) picker.start(onPicked, () => stop(false));
      });
    }

    // applyReuse takes the selected steps + the cache of
    // hydrated source tours, then deep-clones each selected
    // step (with all its descendants), remapping cover_index
    // references so the source's covers come into the active
    // tour's cover_images[]. Same source cover used by
    // multiple cloned steps is deduped to a single entry.
    // Returns the count of steps added (top-level + nested).
    function applyReuse(selected, tourCache) {
      // De-dup: if the operator ticked both a parent and one
      // of its descendants, only the parent's branch clones
      // (the descendant comes along inside the parent).
      const finalSelections = [];
      const allIds = new Set(Array.from(selected.values()).map(v => v.srcStep.id));
      for (const { tourId, srcStep } of selected.values()) {
        let parentTicked = false;
        function hasTickedAncestor(s, srcTour) {
          // Walk srcTour to find the path; if ANY ancestor's
          // id is in allIds (and it's not s itself), skip.
          function find(arr, ancestors) {
            for (const node of arr) {
              if (node.id === s.id) {
                return ancestors.some(a => allIds.has(a.id) && a.id !== s.id);
              }
              if (node.children && node.children.length) {
                const got = find(node.children, ancestors.concat(node));
                if (got !== null) return got;
              }
            }
            return null;
          }
          const srcTourFull = tourCache.get(srcTour);
          if (!srcTourFull) return false;
          return find(srcTourFull.steps || [], []) === true;
        }
        if (hasTickedAncestor(srcStep, tourId)) {
          parentTicked = true;
        }
        if (!parentTicked) finalSelections.push({ tourId, srcStep });
      }

      let count = 0;
      function cloneStep(srcStep, srcTour) {
        const srcCovers = (srcTour.cover_images && srcTour.cover_images.length)
          ? srcTour.cover_images
          : (srcTour.cover_image_url ? [srcTour.cover_image_url] : []);
        const srcCi = Number.isInteger(srcStep.cover_index) ? srcStep.cover_index : 0;
        const srcImg = srcCovers[srcCi];
        // Dedupe target cover by image data — same URL across
        // cloned steps maps to the same slot in our cover_images.
        let dstCi;
        if (srcImg) {
          let found = tour.cover_images.indexOf(srcImg);
          if (found < 0) {
            tour.cover_images.push(srcImg);
            found = tour.cover_images.length - 1;
          }
          dstCi = found;
        } else {
          dstCi = coverIndex; // fallback to current scene
        }
        count++;
        return {
          id: (crypto && crypto.randomUUID)
            ? crypto.randomUUID().replace(/-/g, '')
            : Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2),
          selector: srcStep.selector,
          label: srcStep.label || '',
          title: srcStep.title || '',
          text: srcStep.text || '',
          placement: srcStep.placement || 'bottom',
          rect_left:   srcStep.rect_left   || 0,
          rect_top:    srcStep.rect_top    || 0,
          rect_width:  srcStep.rect_width  || 0,
          rect_height: srcStep.rect_height || 0,
          media_url: srcStep.media_url || '',
          cover_index: dstCi,
          children: (srcStep.children || []).map(c => cloneStep(c, srcTour)),
        };
      }

      for (const { tourId, srcStep } of finalSelections) {
        const srcTour = tourCache.get(tourId);
        if (!srcTour) continue;
        const cloned = cloneStep(srcStep, srcTour);
        tour.steps.push(cloned);
      }
      updateCount();
      return count;
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
        // Paused-state hint coaches the dropdown-capture flow:
        // operators were getting stuck trying to point at items
        // inside menus that close on the picker's preventDefault.
        status.innerHTML = '⏸ Paused — interact, then press <b>P</b> to resume (keeps dropdowns open). Hover + <b>Enter</b> to capture without clicking.';
        fab.classList.remove('recording');
        bar.style.background = '#f59e0b';
        bar.style.color = '#111';
      } else {
        btn.textContent = '⏸ Pause';
        status.textContent = '● Recording';
        fab.classList.add('recording');
        bar.style.background = '';
        bar.style.color = '';
        // Resuming = potentially new page state (open dropdown,
        // revealed modal, scrolled-into-view section). Capture
        // a fresh cover so subsequent steps render against the
        // current visual. Best-effort; if html2canvas fails the
        // new steps just stay on the previous cover (degrades
        // to the old single-cover behaviour, not to a broken
        // preview).
        captureClean().then(url => {
          if (!url || !tour) return;
          tour.cover_images = tour.cover_images || [];
          tour.cover_images.push(url);
          coverIndex = tour.cover_images.length - 1;
        }).catch(err => console.warn('tour: resume cover failed', err));
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
        // cover_index pins the step to whichever cover was active
        // at click time. Preview groups by this value so a step
        // captured inside a dropdown renders on the cover that
        // shows the dropdown OPEN.
        cover_index: coverIndex,
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

    // promptStartMeta shows a modal asking the operator for the
    // tour's title + description BEFORE the picker arms. The
    // metadata typed here ends up as the H1 + intro paragraph
    // in the printed preview / Word export, so it's worth
    // gathering up-front instead of leaving "Tour for /login"
    // placeholder names to clutter the list view.
    function promptStartMeta(routePath) {
      return new Promise(resolve => {
        const modal = document.createElement('div');
        modal.className = 'editor';
        // Centre instead of top-right — it's a blocking prompt
        // so it deserves the screen's attention.
        modal.style.cssText = [
          'left: 50%', 'top: 50%',
          'transform: translate(-50%, -50%)',
          'right: auto', 'width: 360px',
        ].join('; ') + ';';
        modal.innerHTML = `
          <div style="font-weight:600; font-size:15px; margin-bottom:8px">New tour</div>
          <label>Title <span style="color:#ef4444">*</span></label>
          <input class="meta-title" placeholder="e.g. Steps to Open User Profile" />
          <label>Description / figure caption</label>
          <textarea class="meta-desc" placeholder="What does this walkthrough show? Appears as the intro paragraph in the printed preview."></textarea>
          <div class="actions">
            <button class="ghost meta-cancel">Cancel</button>
            <button class="primary meta-start" disabled>Start recording</button>
          </div>
        `;
        root.appendChild(modal);
        const titleEl = modal.querySelector('.meta-title');
        const descEl  = modal.querySelector('.meta-desc');
        const startEl = modal.querySelector('.meta-start');
        // Start button enables once Title is non-empty.
        const sync = () => { startEl.disabled = titleEl.value.trim().length === 0; };
        titleEl.addEventListener('input', sync);
        setTimeout(() => titleEl.focus(), 0);

        function finish(meta) {
          modal.remove();
          resolve(meta);
        }
        modal.querySelector('.meta-cancel').addEventListener('click', () => finish(null));
        startEl.addEventListener('click', () => finish({
          name: titleEl.value.trim(),
          description: descEl.value.trim(),
        }));
        // Enter in the title field jumps to description; Enter
        // in description submits when title is filled.
        titleEl.addEventListener('keydown', e => {
          if (e.key === 'Enter') { e.preventDefault(); descEl.focus(); }
        });
        descEl.addEventListener('keydown', e => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !startEl.disabled) {
            e.preventDefault(); startEl.click();
          }
        });
      });
    }

    // recorderHotkey runs at the document level while recording
    // is active. `P` toggles pause/resume without an outside-
    // click on the recorder bar (which would close a hover-
    // opened dropdown). Skipped while focused inside an
    // editable element so typing the letter still works.
    function recorderHotkey(e) {
      if (!active) return;
      if (e.key !== 'p' && e.key !== 'P') return;
      // Use composedPath so we see INTO shadow roots — events
      // crossing a shadow boundary get retargeted to the host
      // element, which means a plain e.target check misses
      // inputs inside our own step-editor UI (the operator
      // typing "p" in the description field would otherwise
      // toggle pause).
      if (isPathEditable(e)) return;
      e.preventDefault();
      e.stopPropagation();
      togglePause();
    }
    // isPathEditable returns true when the user is currently
    // typing into a form control or contenteditable region.
    // Checks both signals because each one alone has blind
    // spots:
    //   * composedPath() walks across shadow-DOM boundaries
    //     and catches inputs inside our own step-editor UI,
    //     but is empty for events synthesised programmatically.
    //   * document.activeElement is authoritative for the
    //     currently-focused element (the OS-level "you are
    //     typing here") and walks open shadow roots via
    //     shadowRoot.activeElement, which catches inputs in
    //     web-component-based host apps where composedPath()
    //     may stop at the host boundary.
    //   * role-based widgets (role="textbox", "searchbox",
    //     "combobox") behave like inputs to the user even
    //     when their underlying element is a <div> — the P
    //     hotkey must respect those too or apps built with
    //     custom autocomplete fields silently toggle pause
    //     mid-search.
    function isEditableEl(el) {
      if (!el) return false;
      if (el.isContentEditable) return true;
      if (el.tagName && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName)) return true;
      const role = el.getAttribute && el.getAttribute('role');
      return !!(role && /^(textbox|searchbox|combobox|spinbutton)$/.test(role));
    }
    function isPathEditable(e) {
      // 1. The currently-focused element + its shadow chain.
      let ae = document.activeElement;
      while (ae) {
        if (isEditableEl(ae)) return true;
        if (ae.shadowRoot && ae.shadowRoot.activeElement) {
          ae = ae.shadowRoot.activeElement;
        } else {
          break;
        }
      }
      // 2. The event's composed path (catches keydowns that
      //    arrive before focus has settled on the target).
      const path = (e.composedPath && e.composedPath()) || [e.target];
      for (const el of path) {
        if (isEditableEl(el)) return true;
      }
      return false;
    }

    // countTreeSteps gives a fast total of root + nested
    // steps, used to seed the default-title badge counter
    // when continuing an existing tour ("Step 5" if 4
    // already exist).
    function countTreeSteps(arr) {
      let n = 0;
      for (const s of arr) {
        n++;
        if (s.children && s.children.length) n += countTreeSteps(s.children);
      }
      return n;
    }

    // start(routePath, existingTour?) — when existingTour is
    // supplied, the recorder loads it in "continue" mode and
    // appends new captures. Next click pushes a step into
    // tour.steps; first captureClean takes a fresh cover that
    // becomes a NEW scene at the end of tour.cover_images.
    // Save uses the existing tour.id so the server treats it
    // as an update (POST /tours is upsert).
    async function start(routePath, existingTour) {
      if (active) return;
      let meta;
      if (existingTour) {
        // Continue mode: reuse the saved tour's metadata
        // without prompting. Rename happens in the dashboard
        // editor, not here.
        meta = {
          name: existingTour.name || '',
          description: existingTour.description || '',
        };
      } else {
        meta = await promptStartMeta(routePath);
        if (!meta) return; // operator cancelled
      }
      active = true;
      paused = false;
      document.addEventListener('keydown', recorderHotkey, true);
      lastStep = null;

      if (existingTour) {
        tour = existingTour;
        if (!tour.cover_images) tour.cover_images = [];
        if (!tour.steps) tour.steps = [];
        // base_* default if the existing tour predates that schema.
        if (!tour.base_width)  tour.base_width  = document.documentElement.scrollWidth  || window.innerWidth;
        if (!tour.base_height) tour.base_height = document.documentElement.scrollHeight || window.innerHeight;
        badge = countTreeSteps(tour.steps);
      } else {
        badge = 0;
        tour = {
          name: meta.name,
          route: routePath, // kept internally for route-matching; not surfaced in previews
          description: meta.description,
          // base_* match the FULL document we capture (not just
          // the viewport) — scrollWidth × scrollHeight. Step
          // rects are stored in document coords too, so badges
          // align with the cover image regardless of where the
          // operator was scrolled when they clicked.
          base_width:  document.documentElement.scrollWidth  || window.innerWidth,
          base_height: document.documentElement.scrollHeight || window.innerHeight,
          cover_images: [],
          steps: [],
        };
      }
      coverIndex = tour.cover_images.length;
      bar = buildBar();
      // Continue mode signals what's happening so the
      // operator doesn't think they're starting fresh.
      if (existingTour && bar) {
        const status = bar.querySelector('.status');
        if (status) status.innerHTML = `● Adding to: <strong>${escapeHtml(tour.name || 'tour')}</strong> <span style="opacity:.6;font-size:11px">(P pause · Enter capture hovered)</span>`;
      }
      fab.classList.add('recording');
      // Capture the clean cover image. For fresh tours this is
      // cover_image_url + cover_images[0]; for continue mode
      // it becomes a NEW scene at the end of cover_images.
      captureClean()
        .then(url => {
          if (!url) return;
          if (!existingTour) tour.cover_image_url = url;
          tour.cover_images.push(url);
          coverIndex = tour.cover_images.length - 1;
        })
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
      document.removeEventListener('keydown', recorderHotkey, true);
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
  function openMenu(result) {
    closeMenu();
    menu = document.createElement('div');
    menu.className = 'menu';
    // Backward-compat: callers used to pass a flat array. Wrap
    // those into the {matched, others} shape we use internally.
    if (Array.isArray(result)) result = { matched: result, others: [] };
    const matched = result.matched || [];
    const others  = result.others  || [];
    const allTours = [...matched, ...others];
    const recordBtn  = `<button data-act="record">▶ Record new tour</button>`;
    function tourRow(t) {
      const label = (t.name && t.name.trim())
        || t.label
        || (t.route ? `Tour for ${t.route}` : '')
        || (t.id ? `Untitled (${t.id.slice(0, 6)})` : 'Untitled tour');
      if (!t.name || !t.name.trim()) console.warn('tour: missing name', t);
      const routeBadge = t.route && t.route !== currentRoute()
        ? `<span style="font-size:10px;color:#999;margin-left:4px">${escapeHtml(t.route)}</span>`
        : '';
      return `
        <div style="display:flex;gap:2px">
          <button data-act="play" data-id="${t.id}" style="flex:1">▶ ${escapeHtml(label)}${routeBadge}</button>
          <button data-act="continue" data-id="${t.id}" title="Add more scenes / steps to this tour" style="padding:8px 10px">✎</button>
          <button data-act="preview" data-id="${t.id}" title="Open printable preview" style="padding:8px 10px;display:flex;align-items:center;gap:4px">
            📄 <span style="font-size:11px;color:#666;max-width:80px;overflow:hidden;text-overflow:ellipsis">${escapeHtml(label)}</span>
          </button>
        </div>`;
    }
    const matchedBlock = matched.length
      ? `<div style="font-size:11px;color:#888;padding:8px 12px 4px;text-transform:uppercase;letter-spacing:.05em">For this page</div>` +
        matched.map(tourRow).join('') +
        (matched.length > 1
          ? `<button data-act="preview-all">📄 Preview all ${matched.length} as PDF</button>`
          : '')
      : '';
    const othersBlock = others.length
      ? `<div style="font-size:11px;color:#888;padding:8px 12px 4px;text-transform:uppercase;letter-spacing:.05em">${matched.length ? 'Other tours' : 'No tour for this page · Other tours'}</div>` +
        others.map(tourRow).join('')
      : '';
    const emptyHint = allTours.length === 0
      ? `<div class="hint">No tours saved yet — click ▶ Record new tour to start.</div>`
      : '';
    const dashboardLink = `<div style="border-top:1px solid #eee;margin-top:6px;padding-top:4px"><button data-act="dashboard">⚙ Manage in dashboard</button></div>`;
    menu.innerHTML = recordBtn + matchedBlock + othersBlock + emptyHint + dashboardLink;
    root.appendChild(menu);
    menu.addEventListener('click', e => {
      const btn = e.target.closest('button');
      if (!btn) return;
      const act = btn.getAttribute('data-act');
      const id  = btn.getAttribute('data-id');
      closeMenu();
      if (act === 'record') return recorder.start(currentRoute());
      if (act === 'play') {
        const tour = allTours.find(x => x.id === id);
        if (tour) runner.start(tour);
      }
      if (act === 'continue' && id) {
        // Fetch full hydrated tour (the catalogue list omits
        // steps for size) + hand to the recorder in continue
        // mode. The first captureClean lands as a NEW scene
        // on top of the saved covers.
        API.getTour(id)
          .then(t => recorder.start(currentRoute(), t))
          .catch(e => { console.warn('tour: continue fetch failed', e); alert('Could not load tour: ' + (e.message || e)); });
      }
      if (act === 'preview' && id) {
        window.open(`/__nexus/tour/tours/${encodeURIComponent(id)}/preview`, '_blank');
      }
      if (act === 'preview-all') {
        window.open(`/__nexus/tour/preview?route=${encodeURIComponent(currentRoute())}`, '_blank');
      }
      if (act === 'dashboard') {
        window.open('/__nexus/tour', '_blank');
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

  // Check for a pending insertion intent left by the preview
  // page's "Capture from page" choice. If found, the operator
  // wants to record ONE step and have it inserted at a specific
  // slot in an existing tour — not the full tour-recording
  // flow. We fetch the tour, run the picker, splice the new
  // step in, save, then navigate back to where the operator
  // came from. Intent expires after 15 minutes so a stale
  // localStorage entry doesn't ambush a later session.
  function checkPendingInsert() {
    let intent;
    try {
      const raw = localStorage.getItem('__nexus_tour_insert');
      if (!raw) return;
      intent = JSON.parse(raw);
    } catch { return; }
    if (!intent || !intent.tourId || !intent.anchorStepId) {
      localStorage.removeItem('__nexus_tour_insert');
      return;
    }
    const ageMs = Date.now() - (intent.ts || 0);
    if (ageMs > 15 * 60 * 1000) {
      localStorage.removeItem('__nexus_tour_insert');
      return;
    }
    // Wait one paint so the host's frontend has had a chance
    // to render — otherwise the picker might activate before
    // the page's interactive elements are in the DOM.
    setTimeout(() => runInsertCapture(intent), 200);
  }

  async function runInsertCapture(intent) {
    let tour;
    try {
      const r = await fetch(`/__nexus/tour/tours/${encodeURIComponent(intent.tourId)}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      tour = await r.json();
    } catch (e) {
      console.warn('tour: insert intent — failed to load tour', e);
      localStorage.removeItem('__nexus_tour_insert');
      return;
    }
    // Banner stays at the top with a "Start capture" button.
    // The picker is NOT armed at boot — operator may need to
    // open a dropdown, scroll into view, or expand a panel
    // before pointing at the target. Picker only goes live
    // after they click Start (or close the banner via Cancel).
    const banner = document.createElement('div');
    banner.style.cssText = [
      'position:fixed','top:12px','left:50%','transform:translateX(-50%)',
      'background:#2563eb','color:#fff','padding:10px 16px',
      'border-radius:999px','box-shadow:0 4px 12px rgba(0,0,0,.25)',
      'z-index:2147483646','pointer-events:auto',
      'font:13px/1 system-ui,sans-serif','display:flex','gap:10px','align-items:center',
    ].join(';');
    banner.innerHTML = `
      <span>Insert mode — open / navigate to the target, then click Start</span>
      <button class="start" style="background:#fff;color:#2563eb;border:0;padding:5px 12px;border-radius:6px;cursor:pointer;font:inherit;font-weight:600">▶ Start capture</button>
      <button class="cancel" style="background:transparent;color:#fff;border:1px solid #fff;padding:5px 10px;border-radius:6px;cursor:pointer;font:inherit">Cancel</button>
    `;
    document.body.appendChild(banner);

    function cancel() {
      banner.remove();
      picker.stop();
      localStorage.removeItem('__nexus_tour_insert');
    }
    banner.querySelector('.cancel').addEventListener('click', cancel);
    banner.querySelector('.start').addEventListener('click', () => {
      // Swap banner text into "click the target" mode + hide
      // the Start button so the operator can't double-arm.
      banner.querySelector('span').textContent = 'Insert mode — click the target element';
      banner.querySelector('.start').remove();
      armPicker();
    });

    function armPicker() {
    picker.start(async (pick) => {
      banner.remove();
      picker.stop();
      // Build a step shaped like the recorder's normal onPicked,
      // minus the screenshot work — operators just want a
      // pointer to the element, not a fresh cover capture.
      const docLeft = pick.rect.left + window.scrollX;
      const docTop  = pick.rect.top  + window.scrollY;
      const newStep = {
        id: (crypto && crypto.randomUUID)
          ? crypto.randomUUID().replace(/-/g, '')
          : Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2),
        selector: pick.selector,
        label: pick.label,
        title: `Click ${pick.label}`,
        text: '',
        placement: 'bottom',
        children: [],
        rect_left:   Math.round(docLeft),
        rect_top:    Math.round(docTop),
        rect_width:  Math.round(pick.rect.width),
        rect_height: Math.round(pick.rect.height),
        cover_index: 0, // updated below to match the anchor's scene
      };
      // Find anchor step + splice newStep in at the right slot.
      const splice = insertNewStep(tour, intent.anchorStepId, intent.position, newStep);
      if (!splice) {
        alert('Could not find anchor step in tour. Insertion cancelled.');
        localStorage.removeItem('__nexus_tour_insert');
        return;
      }
      // Persist.
      try {
        const r = await fetch('/__nexus/tour/tours', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(tour),
        });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
      } catch (e) {
        alert('Insert save failed: ' + (e.message || e));
        return;
      }
      localStorage.removeItem('__nexus_tour_insert');
      // Bounce back to the preview tab the operator came from.
      if (intent.returnUrl) {
        location.href = intent.returnUrl;
      } else {
        location.reload();
      }
    }, cancel);
    } // armPicker
  } // runInsertCapture

  // insertNewStep walks tour.steps to find anchor, splices
  // newStep either as a sibling (after anchor) or as a child
  // (at end of anchor.children). Inherits anchor's cover_index
  // so the new step groups with the same scene in the preview.
  function insertNewStep(tour, anchorId, position, newStep) {
    function walk(arr, parent) {
      for (let i = 0; i < arr.length; i++) {
        if (arr[i].id === anchorId) {
          newStep.cover_index = Number.isInteger(arr[i].cover_index) ? arr[i].cover_index : 0;
          if (position === 'child') {
            if (!arr[i].children) arr[i].children = [];
            arr[i].children.push(newStep);
          } else {
            arr.splice(i + 1, 0, newStep);
          }
          return true;
        }
        if (arr[i].children && arr[i].children.length) {
          if (walk(arr[i].children, arr[i])) return true;
        }
      }
      return false;
    }
    return walk(tour.steps || [], null);
  }

  mountOverlay();
  checkPendingInsert();
})();