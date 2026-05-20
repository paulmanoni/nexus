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

  if (window.__nexusTourMounted) return;
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
      padding: 6px; min-width: 180px; pointer-events: auto;
    }
    .menu button {
      display: block; width: 100%; text-align: left;
      background: transparent; border: 0; padding: 8px 12px;
      border-radius: 6px; cursor: pointer; font: inherit; color: #111;
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

    .picker-hi {
      position: fixed; pointer-events: none;
      border: 2px solid #2563eb;
      background: rgba(37,99,235,.10);
      border-radius: 4px;
      transition: all 80ms;
    }
    .picker-label {
      position: fixed; pointer-events: none;
      background: #2563eb; color: #fff;
      padding: 3px 8px; border-radius: 4px;
      font-size: 12px; max-width: 320px;
      overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    }

    .play-ring {
      position: fixed; pointer-events: none;
      border: 3px solid #f59e0b;
      border-radius: 6px;
      box-shadow: 0 0 0 9999px rgba(0,0,0,.35);
      transition: all 250ms;
    }
    .play-badge {
      position: fixed; pointer-events: none;
      background: #f59e0b; color: #111; font-weight: 700;
      width: 28px; height: 28px; border-radius: 50%;
      display: flex; align-items: center; justify-content: center;
      box-shadow: 0 2px 6px rgba(0,0,0,.3);
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

  function mountOverlay() {
    if (document.body) document.body.appendChild(overlayHost);
    else document.addEventListener('DOMContentLoaded', mountOverlay, { once: true });
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

    function onMove(e) {
      const t = e.target;
      if (!t || isOurs(t)) { hi.style.display = 'none'; label.style.display = 'none'; return; }
      moveTo(t.getBoundingClientRect(), describeElement(t));
    }
    function onClick(e) {
      const t = e.target;
      if (!t || isOurs(t)) return;
      e.preventDefault();
      e.stopPropagation();
      e.stopImmediatePropagation();
      const rect = t.getBoundingClientRect();
      const out = {
        selector: buildSelector(t),
        label: describeElement(t),
        rect: { left: rect.left, top: rect.top, width: rect.width, height: rect.height },
      };
      if (onPick) onPick(out);
    }
    function onKey(e) { if (e.key === 'Escape' && onCancel) onCancel(); }

    return {
      start(onPickCb, onCancelCb) {
        if (active) return;
        active = true;
        onPick = onPickCb; onCancel = onCancelCb;
        document.addEventListener('mousemove', onMove, true);
        document.addEventListener('click', onClick, true);
        document.addEventListener('keydown', onKey, true);
      },
      stop() {
        if (!active) return;
        active = false;
        hi.style.display = 'none';
        label.style.display = 'none';
        document.removeEventListener('mousemove', onMove, true);
        document.removeEventListener('click', onClick, true);
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
        <span>● Recording</span>
        <span class="count">0</span>
        <button class="edit-last" disabled>✎ Edit last step</button>
        <button class="stop">Stop &amp; Save</button>
        <button class="danger cancel">Cancel</button>
      `;
      root.appendChild(b);
      b.querySelector('.stop').addEventListener('click', () => stop(true));
      b.querySelector('.cancel').addEventListener('click', () => stop(false));
      b.querySelector('.edit-last').addEventListener('click', () => editLastStep());
      return b;
    }

    // editLastStep opens a small in-page form letting the
    // operator override the default title/text on the most
    // recently captured step. While the form is open the picker
    // is paused so a stray click doesn't append a new step.
    function editLastStep() {
      if (!lastStep) return;
      picker.stop();
      const editor = document.createElement('div');
      editor.className = 'editor';
      editor.innerHTML = `
        <div style="font-weight:600;margin-bottom:6px">Edit Step ${lastStep.title || ''}</div>
        <label>Title</label>
        <input class="ed-title" value="${escapeHtml(lastStep.title || '')}" />
        <label>Text shown to the user</label>
        <textarea class="ed-text">${escapeHtml(lastStep.text || '')}</textarea>
        <label>Placement</label>
        <select class="ed-place">
          <option value="bottom">bottom</option>
          <option value="top">top</option>
          <option value="left">left</option>
          <option value="right">right</option>
        </select>
        <div class="actions">
          <button class="ghost ed-cancel">Cancel</button>
          <button class="primary ed-save">Save</button>
        </div>
      `;
      root.appendChild(editor);
      const sel = editor.querySelector('.ed-place');
      sel.value = lastStep.placement || 'bottom';

      function close() {
        editor.remove();
        // Re-arm picker so the next click resumes recording.
        if (active) picker.start(onPicked, () => stop(false));
      }
      editor.querySelector('.ed-cancel').addEventListener('click', close);
      editor.querySelector('.ed-save').addEventListener('click', () => {
        lastStep.title = editor.querySelector('.ed-title').value || lastStep.title;
        lastStep.text  = editor.querySelector('.ed-text').value;
        lastStep.placement = sel.value;
        close();
      });
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
      // Re-arm the picker for the next click — recorders capture
      // every click until the operator hits Stop.
      picker.stop();
      // Build step
      const step = {
        selector: p.selector,
        label: p.label,
        title: `Step ${++badge}`,
        text: `Click ${p.label}.`,
        placement: 'bottom',
        children: [],
        // _rect kept on the local step only — stripped before save.
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
      // Re-arm.
      picker.start(onPicked, () => stop(false));
    }

    async function start(routePath) {
      if (active) return;
      active = true;
      lastStep = null;
      badge = 0;
      tour = {
        name: `Tour for ${routePath}`,
        route: routePath,
        description: '',
        steps: [],
      };
      bar = buildBar();
      fab.classList.add('recording');
      // First click — kick off the picker; from now on each click
      // re-arms it inside onPicked.
      picker.start(onPicked, () => stop(false));
    }

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

    function placeRing(rect) {
      ring.style.left = (rect.left - 4) + 'px';
      ring.style.top  = (rect.top - 4) + 'px';
      ring.style.width  = (rect.width + 8) + 'px';
      ring.style.height = (rect.height + 8) + 'px';
    }
    function placeBadge(rect, n) {
      badgeEl.textContent = String(n);
      badgeEl.style.left = (rect.left - 14) + 'px';
      badgeEl.style.top  = (rect.top - 14) + 'px';
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
      ? tours.map(t => `<button data-act="play" data-id="${t.id}">▶ Play: ${escapeHtml(t.name)}</button>`).join('')
      : `<div class="hint">No tour saved for this page.</div>`;
    menu.innerHTML = recordBtn + playBlock;
    root.appendChild(menu);
    menu.addEventListener('click', e => {
      const t = e.target.closest('button');
      if (!t) return;
      const act = t.getAttribute('data-act');
      closeMenu();
      if (act === 'record') return recorder.start(currentRoute());
      if (act === 'play') {
        const tour = tours.find(x => x.id === t.getAttribute('data-id'));
        if (tour) runner.start(tour);
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

  mountOverlay();
})();