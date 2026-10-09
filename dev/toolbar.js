// The nexus debug toolbar (nexus dev only): a handle on the page's edge and
// a drawer showing a request's panels, rendered by the server
// (toolbar.templ). Fetch and XHR calls are listed as they answer.
(() => {
  if (window.__nxToolbar) return;
  const script = document.currentScript;
  const pageId = script && script.dataset.nexusToolbar;
  if (!pageId) return;
  const HEADER = 'X-Nexus-Toolbar';
  const KEY = 'nexus.toolbar';
  const state = { open: false, hidden: false, panel: 'SQL', current: pageId, list: [], views: {} };
  try {
    const saved = JSON.parse(localStorage.getItem(KEY) || '{}');
    state.open = !!saved.open;
    state.hidden = !!saved.hidden;
    state.panel = saved.panel || 'SQL';
  } catch (_) {}
  const save = () => {
    try { localStorage.setItem(KEY, JSON.stringify({ open: state.open, hidden: state.hidden, panel: state.panel })); } catch (_) {}
  };

  const host = document.createElement('div');
  host.setAttribute('data-nexus-toolbar', '');
  host.style.cssText = 'all:initial;position:fixed;z-index:2147483647;top:0;right:0;';
  const root = host.attachShadow({ mode: 'open' });
  root.innerHTML = '<link rel="stylesheet" href="/__nexus/toolbar/toolbar.css"><div id="app"></div>';
  const app = root.getElementById('app');
  // The console's theme choice, else the OS preference.
  const theme = () => {
    let t = '';
    try { t = localStorage.getItem('nexus.theme'); } catch (_) {}
    if (t !== 'light' && t !== 'dark') t = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    host.dataset.theme = t;
  };
  theme();
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', theme);
  window.addEventListener('storage', (e) => { if (e.key === 'nexus.theme') theme(); });
  // The nexus logo (docs/public/logo.svg) on a white tile.
  const mark = (size) => `<svg class="mark" viewBox="0 0 32 32" width="${size}" height="${size}" fill="none" aria-label="nexus">
    <rect width="32" height="32" rx="7" fill="#fff"></rect>
    <g transform="translate(3 3) scale(.8125)">
      <path d="M6 6 L6 26 M6 26 L26 6 M26 6 L26 26" stroke="#064e3b" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"></path>
      <circle cx="6" cy="6" r="3.4" fill="#064e3b"></circle><circle cx="6" cy="26" r="3.4" fill="#064e3b"></circle>
      <circle cx="26" cy="6" r="3.4" fill="#064e3b"></circle><circle cx="26" cy="26" r="3.4" fill="#064e3b"></circle>
      <circle cx="16" cy="16" r="3.6" fill="#10b981"></circle>
    </g></svg>`;
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const origFetch = window.fetch.bind(window);

  // load is a request's view, as the server rendered it.
  async function load(id) {
    if (state.views[id]) return state.views[id];
    const res = await origFetch('/__nexus/toolbar/requests/' + id);
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    const tpl = document.createElement('template');
    tpl.innerHTML = await res.text();
    const view = tpl.content.firstElementChild;
    state.views[id] = view;
    const entry = state.list.find((e) => e.id === id);
    if (entry) entry.label = view.dataset.label;
    return view;
  }

  function add(id, label) {
    if (!id || state.list.some((e) => e.id === id)) return;
    state.list.push({ id, label });
    load(id).then(draw).catch(() => {});
  }

  function showPanel(view) {
    const names = [...view.querySelectorAll('nav button')].map((b) => b.dataset.panel);
    const name = names.includes(state.panel) ? state.panel : names[0];
    view.querySelectorAll('[data-panel]').forEach((el) => {
      if (el.tagName === 'BUTTON') el.classList.toggle('on', el.dataset.panel === name);
      else el.hidden = el.dataset.panel !== name;
    });
  }

  function draw() {
    const view = state.views[state.current];
    if (!state.open) {
      const tone = view ? view.dataset.tone : '';
      const label = state.hidden ? '' : view ? view.dataset.handle : '…';
      app.innerHTML = `<button class="handle ${esc(tone)}${state.hidden ? ' hidden' : ''}" title="nexus debug toolbar">
        ${mark(20)}${label ? `<span class="figs">${esc(label)}</span>` : ''}</button>`;
      app.firstElementChild.onclick = () => { state.open = true; save(); draw(); };
      return;
    }
    const opts = state.list.map((e) =>
      `<option value="${esc(e.id)}"${e.id === state.current ? ' selected' : ''}>${esc(e.label)}</option>`).join('');
    app.innerHTML = `<div class="drawer"><div class="top"><span class="brand">${mark(22)}nexus <small>debug</small></span>
      <select title="Requests on this page">${opts}</select>
      <button data-act="hide" title="Show or hide the figures on the handle">${state.hidden ? 'Show figures' : 'Hide figures'}</button>
      <button data-act="close" title="Close (Esc)">Close</button></div><div class="slot"></div></div>`;
    const slot = app.querySelector('.slot');
    if (view) {
      showPanel(view);
      slot.replaceWith(view);
      view.querySelectorAll('nav button').forEach((b) => {
        b.onclick = () => { state.panel = b.dataset.panel; save(); showPanel(view); };
      });
    } else {
      slot.innerHTML = '<div class="empty">Loading…</div>';
      load(state.current).then(draw).catch((e) => { slot.innerHTML = `<div class="err">${esc(e.message)}</div>`; });
    }
    app.querySelector('select').onchange = (e) => { state.current = e.target.value; draw(); };
    app.querySelector('[data-act=close]').onclick = () => { state.open = false; save(); draw(); };
    app.querySelector('[data-act=hide]').onclick = () => { state.hidden = !state.hidden; save(); draw(); };
  }

  // Fetch and XHR answers carry their request's toolbar ID in a header.
  window.fetch = async (...args) => {
    const res = await origFetch(...args);
    try {
      const id = res.headers.get(HEADER);
      if (id) add(id, new URL(res.url, location.href).pathname);
    } catch (_) {}
    return res;
  };
  const xhrOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    this.addEventListener('load', () => {
      try {
        const id = this.getResponseHeader(HEADER);
        if (id) add(id, `${method} ${new URL(url, location.href).pathname}`);
      } catch (_) {}
    });
    return xhrOpen.call(this, method, url, ...rest);
  };

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && state.open) { state.open = false; save(); draw(); }
  });

  // What the page does after its load — a live view's socket events — is
  // listed as the server records it.
  let after = 0;
  const poll = async () => {
    if (document.visibilityState !== 'visible') return;
    try {
      const res = await origFetch('/__nexus/toolbar/requests/' + pageId + '/children?after=' + after);
      if (!res.ok) { clearInterval(timer); return; }
      const body = await res.json();
      after = body.next;
      body.items.forEach((it) => add(it.id, it.label));
    } catch (_) {}
  };
  const timer = setInterval(poll, 1500);

  window.__nxToolbar = { add, open: () => { state.open = true; draw(); } };
  add(pageId, `${location.pathname} (page)`);
  // On <html>, not <body>: in-app navigation replaces the body.
  document.documentElement.appendChild(host);
  draw();
})();
