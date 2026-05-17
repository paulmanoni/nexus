// nexus-live.js — client runtime for the live/template engine.
//
// Bootstraps from <div data-nl-component="Name"> in the SSR shell.
// Opens a WebSocket to the same path, joins the named component,
// receives the initial Rendered tree, and from there:
//
//   - holds the tree in memory and mutates it from diff frames
//   - re-stitches HTML and morphs the DOM after each frame
//   - delegates DOM events to nl-on:* attributes and forwards them
//     to the server as "event" messages
//
// The diff applicator mirrors the server algorithm in diff.go; the
// stitch function mirrors rendered.go's HTML(). Keep them in sync
// when the wire shape changes.

(function () {
  "use strict";

  const mount = document.querySelector("[data-nl-component]");
  if (!mount) return;
  const componentName = mount.getAttribute("data-nl-component");

  // -------- WebSocket --------------------------------------------------

  const wsProto = location.protocol === "https:" ? "wss:" : "ws:";
  const wsURL = wsProto + "//" + location.host + location.pathname + location.search;

  // Local mirror of the server's Rendered tree. Set on "joined";
  // mutated in place on every "diff". All renders read from here.
  let tree = null;

  // Live socket — replaced on every reconnect. Wrap in a holder
  // so the closures (event delegation, push targets) always read
  // the current one rather than capturing a stale reference.
  let sock = null;

  // Session-resumption token from the most recent "joined" frame.
  // Sent in the next join after a disconnect so the server can
  // restore Filter/NewTitle/etc. from its parked-session pool
  // (engine.WithSessionResumption). Cleared on a clean close.
  let resumeToken = null;

  // Reconnect bookkeeping. attempt grows the backoff window;
  // closedByUser short-circuits the loop when the user navigates
  // away or explicitly disconnects.
  let attempt = 0;
  let closedByUser = false;
  const MAX_BACKOFF_MS = 30_000;

  function connect() {
    sock = new WebSocket(wsURL);

    sock.addEventListener("open", () => {
      attempt = 0;
      sock.send(JSON.stringify({
        type: "join",
        component: componentName,
        params: Object.fromEntries(new URLSearchParams(location.search)),
        // token is omitted on the very first join — server-side
        // claimParked treats "" as "no resumption requested".
        token: resumeToken || undefined,
      }));
    });

    sock.addEventListener("message", (e) => {
      let msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      switch (msg.type) {
        case "joined":
          if (msg.token) resumeToken = msg.token;
          tree = msg.r;
          render();
          // After live-navigate the server includes the new
          // path so the address bar reflects it without a
          // full reload. pushState here is a no-op for plain
          // joins (no msg.path on initial connect).
          if (msg.path && msg.path !== location.pathname + location.search) {
            history.pushState({ nlPath: msg.path }, "", msg.path);
          }
          break;
        case "diff":
          applyDiff(tree, msg.d);
          render();
          break;
        case "stream-op":
          handleStreamOp(msg);
          break;
        case "push":
          mount.dispatchEvent(new CustomEvent("nl:" + msg.event, { detail: msg.payload }));
          break;
        case "error":
          console.error("[nl] server:", msg.msg);
          break;
        case "reload":
          location.reload();
          break;
        case "pong":
          break;
      }
    });

    sock.addEventListener("close", (ev) => {
      // Code 1000 = normal closure (we initiated). Code 1001 =
      // going away (server shutting down, page navigating). In
      // both cases the user doesn't want a reconnect spinner.
      if (closedByUser || ev.code === 1000) {
        console.info("[nl] disconnected");
        return;
      }
      // Exponential backoff with a cap; randomized jitter (50%)
      // so a server restart doesn't cause every client to
      // reconnect in lockstep.
      attempt += 1;
      const base = Math.min(250 * 2 ** (attempt - 1), MAX_BACKOFF_MS);
      const delay = base * (0.5 + Math.random() * 0.5);
      console.info(`[nl] reconnect in ${Math.round(delay)}ms (attempt ${attempt})`);
      setTimeout(connect, delay);
    });
  }

  // Clean-close on navigation so the server can free resources
  // immediately instead of waiting for parkTTL to expire.
  window.addEventListener("beforeunload", () => {
    closedByUser = true;
    if (sock && sock.readyState === WebSocket.OPEN) sock.close(1000);
  });

  // ---- live-navigate ------------------------------------------------
  //
  // Intercept clicks on <a nl-navigate href="/foo"> and send a
  // "navigate" message over the existing socket instead of doing
  // a full page reload. The server responds with a "joined"
  // frame containing the new tree + new path; the message
  // handler above applies history.pushState.
  //
  // Skip when: modifier key is held (open-in-new-tab), the link
  // has target="_blank", or the href is external. Falling back
  // to default navigation in those cases is what users expect.
  document.addEventListener("click", (e) => {
    if (e.defaultPrevented) return;
    if (e.button !== 0) return; // only primary clicks
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target && e.target.closest && e.target.closest("a[nl-navigate]");
    if (!a) return;
    if (a.target && a.target !== "_self") return;
    const href = a.getAttribute("href");
    if (!href || /^[a-z]+:/i.test(href)) return; // skip external / mailto / etc.
    e.preventDefault();
    if (!sock || sock.readyState !== WebSocket.OPEN) {
      // Socket isn't ready — fall back to a regular nav so the
      // user isn't stuck on a stale page.
      location.href = href;
      return;
    }
    sock.send(JSON.stringify({ type: "navigate", path: href }));
  });

  // Back/forward should also flow through the live channel so
  // we don't reload the page. State is the path we pushed.
  window.addEventListener("popstate", (e) => {
    const path = (e.state && e.state.nlPath) || location.pathname + location.search;
    if (sock && sock.readyState === WebSocket.OPEN) {
      sock.send(JSON.stringify({ type: "navigate", path }));
    }
  });

  connect();

  // -------- Stitch (mirror of server rendered.go HTML) ----------------

  // stitch renders a Rendered or Comprehension to HTML. The two shapes
  // are distinguished by which child key is present: Rendered has "d"
  // (dynamics), Comprehension has "r" (rows).
  function stitch(node) {
    if (!node) return "";
    if (Array.isArray(node.r)) return stitchComp(node);
    return stitchRendered(node);
  }

  function stitchRendered(node) {
    let out = "";
    const s = node.s || [];
    const d = node.d || [];
    for (let i = 0; i < s.length; i++) {
      out += s[i];
      if (i < d.length) out += stitchDynamic(d[i]);
    }
    return out;
  }

  function stitchComp(node) {
    let out = "";
    const s = node.s || [];
    for (const row of node.r) {
      const d = row.d || [];
      for (let i = 0; i < s.length; i++) {
        out += s[i];
        if (i < d.length) out += stitchDynamic(d[i]);
      }
    }
    return out;
  }

  function stitchDynamic(v) {
    if (v === null || v === undefined) return "";
    if (typeof v === "string") return v;
    if (typeof v === "object") return stitch(v);
    return String(v);
  }

  // -------- Diff applicator (mirror of server diff.go) ----------------

  // Slot patches arrive in five shapes; classify by which key is present.
  function classifyPatch(p) {
    if (p === null) return "nil";
    if (typeof p === "string") return "leaf";
    if (typeof p !== "object") return "leaf";
    if (Array.isArray(p.r)) return "fullComp";
    if (Array.isArray(p.o)) return "compDiff";
    if (Array.isArray(p.s)) return "fullRendered";
    return "renderedDiff";
  }

  function applyDiff(node, diff) {
    if (!diff) return;
    for (const k of Object.keys(diff)) {
      const idx = +k;
      const patch = diff[k];
      const kind = classifyPatch(patch);
      const cur = node.d ? node.d[idx] : undefined;

      if (kind === "leaf" || kind === "nil") {
        node.d[idx] = patch;
        continue;
      }
      if (kind === "fullRendered" || kind === "fullComp") {
        node.d[idx] = patch;
        continue;
      }
      if (kind === "renderedDiff") {
        if (cur && typeof cur === "object" && !Array.isArray(cur.r)) {
          applyDiff(cur, patch);
        } else {
          node.d[idx] = patch;
        }
        continue;
      }
      if (kind === "compDiff") {
        if (cur && Array.isArray(cur.r)) {
          applyCompDiff(cur, patch);
        } else {
          // No prior Comprehension to merge into — promote the diff
          // by treating it as a full Comp built from its inserts.
          node.d[idx] = compFromDiff(patch, null);
        }
      }
    }
  }

  // applyCompDiff rebuilds rows in the new key order. For each key:
  //   - in inserts → use insert's d[]
  //   - in updates → clone prior row.d, apply the row-diff
  //   - else      → keep prior row verbatim
  function applyCompDiff(comp, diff) {
    const oldByKey = {};
    for (const row of (comp.r || [])) oldByKey[row.k] = row;

    const newRows = [];
    const updates = diff.u || {};
    const inserts = diff.i || {};

    for (const key of diff.o) {
      if (inserts[key] !== undefined) {
        newRows.push({ k: key, d: inserts[key] });
        continue;
      }
      const prev = oldByKey[key];
      if (!prev) continue; // shouldn't happen; defensive
      if (updates[key]) {
        const row = { k: key, d: prev.d.slice() };
        applyRowDiff(row, updates[key]);
        newRows.push(row);
      } else {
        newRows.push(prev);
      }
    }
    comp.r = newRows;
  }

  // Row patches share the slot-keyed structure with Rendered diffs;
  // reuse the same logic, just rooted at a row {d:[]} instead of a
  // full Rendered.
  function applyRowDiff(row, diff) {
    for (const k of Object.keys(diff)) {
      const idx = +k;
      const patch = diff[k];
      const kind = classifyPatch(patch);
      const cur = row.d[idx];

      if (kind === "leaf" || kind === "nil") {
        row.d[idx] = patch;
        continue;
      }
      if (kind === "fullRendered" || kind === "fullComp") {
        row.d[idx] = patch;
        continue;
      }
      if (kind === "renderedDiff") {
        if (cur && typeof cur === "object" && !Array.isArray(cur.r)) {
          applyDiff(cur, patch);
        } else {
          row.d[idx] = patch;
        }
        continue;
      }
      if (kind === "compDiff") {
        if (cur && Array.isArray(cur.r)) {
          applyCompDiff(cur, patch);
        } else {
          row.d[idx] = compFromDiff(patch, null);
        }
      }
    }
  }

  function compFromDiff(diff, prior) {
    const comp = { s: (prior && prior.s) || [], r: [] };
    for (const key of (diff.o || [])) {
      const inserts = diff.i || {};
      if (inserts[key] !== undefined) {
        comp.r.push({ k: key, d: inserts[key] });
      }
    }
    return comp;
  }

  // -------- DOM morph -------------------------------------------------

  // render stitches the current tree and morphs it into the mount.
  // The morpher is intentionally simple: walk parsed children in
  // parallel, mutate in place when tag matches, replace otherwise.
  // Real production use will outgrow this; that's the trigger to
  // swap in morphdom (~3KB extra).
  function render() {
    const html = stitch(tree);
    const tmp = document.createElement("div");
    tmp.innerHTML = html;
    morphChildren(mount, tmp);
    // Window/document event listeners are discovered from the
    // rendered tree; re-scan after morph so newly-mounted
    // elements with @window/@document modifiers get covered.
    syncGlobalListeners();
    // JS hooks lifecycle — mount/update/destroy per element
    // bearing nl-hook="HookName".
    syncHooks();
  }

  function morphChildren(target, source) {
    const t = Array.from(target.childNodes);
    const s = Array.from(source.childNodes);
    const max = Math.max(t.length, s.length);

    for (let i = 0; i < max; i++) {
      const tc = t[i];
      const sc = s[i];

      if (!sc) {
        if (tc) target.removeChild(tc);
        continue;
      }
      if (!tc) {
        target.appendChild(sc.cloneNode(true));
        continue;
      }
      if (tc.nodeType !== sc.nodeType || (tc.nodeType === 1 && tc.tagName !== sc.tagName)) {
        target.replaceChild(sc.cloneNode(true), tc);
        continue;
      }
      if (tc.nodeType === 3) { // TEXT_NODE
        if (tc.textContent !== sc.textContent) tc.textContent = sc.textContent;
        continue;
      }
      if (tc.nodeType === 1) { // ELEMENT_NODE
        syncAttributes(tc, sc);
        morphChildren(tc, sc);
      }
    }
  }

  function syncAttributes(target, source) {
    // Remove attrs on target not present on source.
    for (const a of Array.from(target.attributes)) {
      if (!source.hasAttribute(a.name)) target.removeAttribute(a.name);
    }
    // Add / update from source.
    for (const a of Array.from(source.attributes)) {
      if (target.getAttribute(a.name) !== a.value) {
        // Don't clobber the live value of a focused input — losing
        // focus and the cursor position mid-keystroke is the cardinal
        // sin of server-driven UI. The server will agree on next event.
        if (a.name === "value" && document.activeElement === target &&
            (target.tagName === "INPUT" || target.tagName === "TEXTAREA")) {
          continue;
        }
        target.setAttribute(a.name, a.value);
      }
    }
  }

  // -------- Event delegation -----------------------------------------

  // One delegated listener per event type on the mount handles every
  // nl-on:<event>[.mod...]="handler" attribute below it. Modifiers
  // (prevent / stop / once) are applied on the way out; everything
  // else flows to the server in the payload.
  const DELEGATED_EVENTS = ["click", "input", "change", "submit", "keydown", "keyup", "blur", "focus"];

  for (const evt of DELEGATED_EVENTS) {
    mount.addEventListener(evt, (e) => {
      let elem = e.target;
      while (elem && elem !== mount.parentNode) {
        if (elem.nodeType === 1) {
          const attr = findNlOnAttr(elem, evt);
          if (attr) {
            if (dispatchNlEvent(elem, attr, e)) return;
          }
        }
        elem = elem.parentNode;
      }
    }, true); // capture phase so we see the event before the user's own listeners
  }

  function findNlOnAttr(elem, evt) {
    const prefix = "nl-on:" + evt;
    for (const a of elem.attributes) {
      if (a.name === prefix || a.name.startsWith(prefix + ".")) return a;
    }
    return null;
  }

  // dispatchNlEvent applies modifiers and sends the event to the
  // server. Returns true when the event was consumed (stop modifier
  // applied, or handled and we don't want bubbling). Returns false
  // when a modifier filter (keyboard key, system modifier) rejected
  // the event — caller should keep walking ancestors / let the
  // event continue.
  function dispatchNlEvent(elem, attr, e) {
    const mods = attr.name.split(".").slice(1);
    const eventName = attr.value;

    // Keyboard-key modifier filter: @keydown.enter only fires on
    // Enter. Multiple key mods OR together (.enter.escape fires
    // on either). System mods (ctrl/shift/alt/meta) require the
    // matching modifier key to be held.
    if (typeof KeyboardEvent !== "undefined" && e instanceof KeyboardEvent) {
      const keyMods = mods.filter(m => KEY_NAMES.has(m));
      if (keyMods.length > 0) {
        const k = (e.key || "").toLowerCase();
        const matched = keyMods.some(m => k === (KEY_ALIASES[m] || m));
        if (!matched) return false;
      }
      if (mods.includes("ctrl") && !e.ctrlKey) return false;
      if (mods.includes("shift") && !e.shiftKey) return false;
      if (mods.includes("alt") && !e.altKey) return false;
      if (mods.includes("meta") && !e.metaKey) return false;
    }

    if (mods.includes("prevent")) e.preventDefault();
    if (mods.includes("stop")) e.stopPropagation();
    if (mods.includes("once")) elem.removeAttribute(attr.name);

    // Build payload:
    //   - every data-* attribute on the firing element
    //   - .value if the element has one (input/select/textarea)
    //   - .key for keyboard events
    const payload = {};
    for (const a of elem.attributes) {
      if (a.name.startsWith("data-")) {
        payload[a.name.slice(5)] = a.value;
      }
    }
    if ("value" in elem && elem.value !== undefined) {
      payload.value = elem.value;
    }
    if (e.key !== undefined) payload.key = e.key;

    if (!sock || sock.readyState !== WebSocket.OPEN) return true;
    sock.send(JSON.stringify({ type: "event", name: eventName, payload }));
    return true;
  }

  // Recognized keyboard-key modifier names. Values left side =
  // modifier as written in template; values that get aliased to
  // e.key's lowercased form live in KEY_ALIASES.
  const KEY_NAMES = new Set([
    "enter", "escape", "tab", "space", "backspace", "delete",
    "up", "down", "left", "right",
  ]);
  const KEY_ALIASES = {
    space: " ",
    up: "arrowup",
    down: "arrowdown",
    left: "arrowleft",
    right: "arrowright",
  };

  // -------- Stream ops (nl-stream containers) ------------------------
  //
  // Server pushes {type:"stream-op", stream, op, id, html}; we
  // find the nl-stream="<name>" element and mutate its children
  // without touching the surrounding template. The full-tree
  // diff doesn't run for stream ops — that's the whole point
  // (avoids re-rendering large lists on every append).
  //
  // Items are looked up by DOM id within the container, so
  // every appended/prepended/updated HTML fragment must carry
  // id="..." on its root element.
  function handleStreamOp(msg) {
    const container = mount.querySelector('[nl-stream="' + cssEscape(msg.stream) + '"]');
    if (!container) {
      console.warn('[nl] stream-op: no container for stream "' + msg.stream + '"');
      return;
    }
    switch (msg.op) {
      case "append":
      case "prepend": {
        const el = parseFragment(msg.html);
        if (!el) return;
        if (msg.op === "append") container.appendChild(el);
        else container.insertBefore(el, container.firstChild);
        break;
      }
      case "delete": {
        const existing = container.querySelector("#" + cssEscape(msg.id));
        if (existing) existing.remove();
        break;
      }
      case "update": {
        const fresh = parseFragment(msg.html);
        if (!fresh) return;
        const existing = container.querySelector("#" + cssEscape(msg.id));
        if (existing) existing.replaceWith(fresh);
        else container.appendChild(fresh);
        break;
      }
      case "reset":
        container.innerHTML = "";
        break;
    }
  }

  // parseFragment renders an HTML string into the first
  // element it produces. <template>'s parser handles arbitrary
  // child types correctly (e.g. <li> outside a <ul>); plain
  // innerHTML on a div would drop those.
  function parseFragment(html) {
    const tmp = document.createElement("template");
    tmp.innerHTML = (html || "").trim();
    return tmp.content.firstElementChild;
  }

  // cssEscape mirrors CSS.escape with a small polyfill so the
  // client works on older browsers. Only the characters that
  // commonly appear in stream names and ids need escaping.
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/[^a-zA-Z0-9_\-]/g, (c) => "\\" + c.charCodeAt(0).toString(16) + " ");
  }

  // -------- Global event targets (@window.X, @document.X) ------------

  // attachedGlobals tracks which (target, event) pairs already
  // have a listener installed on window or document. We never
  // remove these — a stale listener is a no-op once no element
  // bears the matching attribute (the dispatch loop simply finds
  // nothing to fire). Avoids the lifecycle bookkeeping a remove
  // path would need.
  const attachedGlobals = new Set();

  function syncGlobalListeners() {
    for (const el of mount.querySelectorAll("*")) {
      for (const a of el.attributes) {
        if (!a.name.startsWith("nl-on:")) continue;
        const parts = a.name.split(".");
        const target = parts.find(m => m === "window" || m === "document");
        if (!target) continue;
        const eventName = parts[0].slice("nl-on:".length);
        const key = target + ":" + eventName;
        if (attachedGlobals.has(key)) continue;
        attachedGlobals.add(key);
        const tg = target === "window" ? window : document;
        tg.addEventListener(eventName, makeGlobalHandler(target, eventName), true);
      }
    }
  }

  function makeGlobalHandler(target, eventName) {
    return (e) => {
      const suffix = "." + target;
      // First match wins, like the in-tree delegated dispatch.
      for (const el of mount.querySelectorAll("*")) {
        for (const a of el.attributes) {
          if (!a.name.startsWith("nl-on:" + eventName)) continue;
          if (!a.name.includes(suffix)) continue;
          if (dispatchNlEvent(el, a, e)) return;
        }
      }
    };
  }

  // -------- JS hooks (nl-hook) ---------------------------------------

  // window.NLHooks is the registry users write to:
  //
  //   window.NLHooks = {
  //     Tooltip: {
  //       mounted(el)   { ... },
  //       updated(el)   { ... },   // optional
  //       destroyed(el) { ... },   // optional
  //     },
  //   };
  //
  // Templates opt elements in by adding nl-hook="Tooltip". The
  // engine emits the attribute as-is; the client walks the tree
  // on every render to fire lifecycle.
  window.NLHooks = window.NLHooks || {};

  // mountedHooks tracks elements that have had mounted() called.
  // WeakMap because keys are DOM nodes that get GC'd when removed
  // from the tree.
  const mountedHooks = new WeakMap();
  // activeHooked is the set of currently-mounted hooked elements
  // — needed for destroyed() detection because a removed element
  // isn't reachable via querySelectorAll on the next render.
  let activeHooked = new Set();

  function syncHooks() {
    const seen = new Set();
    for (const el of mount.querySelectorAll("[nl-hook]")) {
      const name = el.getAttribute("nl-hook");
      const hook = window.NLHooks[name];
      if (!hook) continue;
      seen.add(el);
      if (!mountedHooks.has(el)) {
        mountedHooks.set(el, name);
        if (hook.mounted) {
          try { hook.mounted(el); } catch (err) { console.error("[nl] hook " + name + " mounted:", err); }
        }
      } else if (hook.updated) {
        try { hook.updated(el); } catch (err) { console.error("[nl] hook " + name + " updated:", err); }
      }
    }
    // Anything in activeHooked but not in seen was removed —
    // fire destroyed() before the WeakMap drops it.
    for (const el of activeHooked) {
      if (seen.has(el)) continue;
      const name = mountedHooks.get(el);
      const hook = name && window.NLHooks[name];
      if (hook && hook.destroyed) {
        try { hook.destroyed(el); } catch (err) { console.error("[nl] hook " + name + " destroyed:", err); }
      }
    }
    activeHooked = seen;
  }
})();
