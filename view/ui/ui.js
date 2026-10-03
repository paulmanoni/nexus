// nexus/view/ui: the browser behaviour of the component kit. It works by
// attributes and event delegation, so markup that live updates, shards and
// in-app navigation put on the page works without initialising anything:
//
//   loading      ui.Loading / data-ui-loading: a loader over elements until
//                the live page answers the event
//   toasts       nxui.toast(text, opts) and ui.Toast elements
//   hotkeys      data-ui-hotkey="mod+s" clicks the element
//   copy-link    data-ui-copy="/orders/7" copies the absolute URL
//   menus        data-ui-menu-trigger opens the sibling data-ui-menu
//   dialogs      data-ui-open / data-ui-close / Esc / backdrop
//   tabs         data-ui-tab marks itself selected and shows its panel
//   row clicks   data-ui-href on a row navigates as an in-app link
//
// It never touches the view runtime: an event's reply is seen as the live
// root (data-nx-live) dropping its aria-busy.
(function (root) {
  "use strict";
  if (root.nxui) return; // loaded twice (a vendored copy and the kit): first wins

  var ui = {};
  root.nxui = ui;

  // ---- pure helpers (tested without a DOM) -------------------------------

  var KEY_ALIASES = { esc: "escape", return: "enter", space: " ", spacebar: " ", slash: "/", plus: "+", del: "delete", up: "arrowup", down: "arrowdown", left: "arrowleft", right: "arrowright" };

  // parseHotkey reads "mod+shift+k" into the modifiers it needs and its key.
  function parseHotkey(combo) {
    var spec = { key: "", mod: false, ctrl: false, meta: false, shift: false, alt: false };
    var parts = String(combo || "").toLowerCase().split("+");
    // "mod++" names the + key: an empty last part after a "+".
    if (parts.length > 1 && parts[parts.length - 1] === "" && parts[parts.length - 2] === "") {
      parts.splice(parts.length - 2, 2, "+");
    }
    for (var i = 0; i < parts.length; i++) {
      var p = parts[i].trim();
      if (i < parts.length - 1) {
        if (p === "mod") spec.mod = true;
        else if (p === "ctrl" || p === "control") spec.ctrl = true;
        else if (p === "meta" || p === "cmd" || p === "command") spec.meta = true;
        else if (p === "shift") spec.shift = true;
        else if (p === "alt" || p === "option") spec.alt = true;
      } else {
        spec.key = KEY_ALIASES[p] || p;
      }
    }
    return spec;
  }

  // matchHotkey reports whether a keydown event matches a parsed spec. "mod"
  // is Cmd on a Mac and Ctrl elsewhere. Shift must match for a letter; for a
  // symbol ("?") the key itself already says it.
  function matchHotkey(spec, e, isMac) {
    if (!spec.key || typeof e.key !== "string") return false;
    var key = e.key.toLowerCase();
    if (key !== spec.key) return false;
    var ctrl = spec.ctrl || (spec.mod && !isMac);
    var meta = spec.meta || (spec.mod && isMac);
    if (!!e.ctrlKey !== ctrl || !!e.metaKey !== meta || !!e.altKey !== spec.alt) return false;
    if (/^[a-z0-9]$/.test(spec.key) || spec.shift) return !!e.shiftKey === spec.shift;
    return true;
  }

  // hasModifier: a combo that may fire while typing in a field.
  function hasModifier(spec) { return spec.mod || spec.ctrl || spec.meta || spec.alt; }

  // copyText is what data-ui-copy copies: a path becomes an absolute URL.
  function copyText(value, origin) {
    value = String(value || "");
    if (value.charAt(0) === "/" && value.charAt(1) !== "/") return origin + value;
    return value;
  }

  // placeMenu positions a menu (fixed) at its trigger's rect: below it and
  // right-aligned, or above when there is no room below.
  function placeMenu(rect, width, view) {
    var below = view.height - rect.bottom;
    var up = below < 220 && rect.top > below;
    var room = up ? rect.top - 6 : below - 6;
    var left = Math.max(8, Math.min(rect.right - width, view.width - width - 8));
    return {
      left: Math.round(left),
      top: up ? null : Math.round(rect.bottom + 4),
      bottom: up ? Math.round(view.height - rect.top + 4) : null,
      maxHeight: Math.round(Math.max(160, room - 8)),
    };
  }

  ui._parseHotkey = parseHotkey;
  ui._matchHotkey = matchHotkey;
  ui._copyText = copyText;
  ui._placeMenu = placeMenu;

  if (typeof document === "undefined") return;

  var doc = document;
  var isMac = /Mac|iPhone|iPad/.test((root.navigator && (root.navigator.platform || root.navigator.userAgent)) || "");

  function closest(el, sel) { return el && el.closest ? el.closest(sel) : null; }
  function all(sel, scope) { return Array.prototype.slice.call((scope || doc).querySelectorAll(sel)); }
  function visible(el) { return !!(el && el.isConnected && el.getClientRects().length); }

  // ---- loading ------------------------------------------------------------

  var pending = [];

  function cover(el) {
    el.classList.add("ui-loading");
    el.setAttribute("aria-busy", "true");
    if (root.getComputedStyle && root.getComputedStyle(el).position === "static") {
      el.style.position = "relative";
      el.setAttribute("data-ui-pos", "");
    }
  }

  function uncover(el) {
    el.classList.remove("ui-loading");
    el.removeAttribute("aria-busy");
    if (el.hasAttribute("data-ui-pos")) {
      el.style.position = "";
      el.removeAttribute("data-ui-pos");
    }
  }

  function release(entry) {
    var i = pending.indexOf(entry);
    if (i < 0) return;
    pending.splice(i, 1);
    clearTimeout(entry.timer);
    entry.els.forEach(uncover);
    if (entry.trigger && entry.trigger.removeAttribute) entry.trigger.removeAttribute("aria-busy");
  }

  // loading covers the elements named by ids (or, with none, the live root
  // the trigger is in) until that live root's next reply - or 30 seconds.
  ui.loading = function (trigger, ids) {
    if (typeof ids === "string") ids = ids.split(/\s+/);
    var liveRoot = closest(trigger, "[data-nx-live]");
    var els = [];
    (ids || []).forEach(function (id) {
      var el = id && doc.getElementById(id);
      if (el) els.push(el);
    });
    if (!els.length && (!ids || !ids.length) && liveRoot && liveRoot !== doc.body) els.push(liveRoot);
    els.forEach(cover);
    if (trigger && trigger.setAttribute && trigger.classList && trigger.classList.contains("ui-btn")) trigger.setAttribute("aria-busy", "true");
    var entry = { els: els, trigger: trigger, root: liveRoot, seen: !!(liveRoot && liveRoot.getAttribute("aria-busy") === "true") };
    entry.timer = setTimeout(function () { release(entry); }, 30000);
    pending.push(entry);
    return entry;
  };

  // A live root dropping aria-busy is its reply: lift what waited on it.
  function onBusy(target) {
    var busy = target.getAttribute("aria-busy") === "true";
    pending.slice().forEach(function (entry) {
      if (entry.root !== target) return;
      if (busy) entry.seen = true;
      else if (entry.seen) release(entry);
    });
  }

  // data-ui-loading runs in the capture phase, before the element's own
  // handler sends the event.
  function onLoadingAttr(e) {
    var el = closest(e.target, "[data-ui-loading]");
    if (!el) return;
    if (e.type === "click" && el.tagName === "FORM") return;
    if (e.type === "submit" && el.tagName !== "FORM") return;
    if (el.disabled) return;
    ui.loading(el, (el.getAttribute("data-ui-loading") || "").split(/\s+/).filter(Boolean));
  }

  // ---- toasts -------------------------------------------------------------

  var shownToasts = new Set();

  function toaster() {
    var t = doc.getElementById("ui-toaster");
    if (!t) {
      t = doc.createElement("div");
      t.id = "ui-toaster";
      t.className = "ui-toaster";
      t.setAttribute("role", "region");
      t.setAttribute("aria-label", "Notifications");
      // Outside <body>: a live update or in-app navigation patches <body>.
      doc.documentElement.appendChild(t);
    }
    return t;
  }

  function dismiss(el) {
    if (!el.isConnected || el.classList.contains("is-leaving")) return;
    el.classList.add("is-leaving");
    setTimeout(function () { el.remove(); }, 200);
  }

  // toast shows text; opts: { title, variant: success|danger|warning|info,
  // duration: ms (0 = 4000, -1 = until dismissed) }.
  ui.toast = function (text, opts) {
    opts = opts || {};
    var el = doc.createElement("div");
    el.className = "ui-toast";
    if (opts.variant) el.setAttribute("data-variant", opts.variant);
    el.setAttribute("role", opts.variant === "danger" ? "alert" : "status");
    var body = doc.createElement("div");
    body.className = "ui-toast-body";
    if (opts.title) {
      var title = doc.createElement("div");
      title.className = "ui-toast-title";
      title.textContent = opts.title;
      body.appendChild(title);
    }
    if (text) body.appendChild(doc.createTextNode(text));
    var close = doc.createElement("button");
    close.type = "button";
    close.className = "ui-toast-close";
    close.setAttribute("aria-label", "Dismiss");
    close.textContent = "\u00d7";
    close.addEventListener("click", function () { dismiss(el); });
    el.appendChild(body);
    el.appendChild(close);
    toaster().appendChild(el);
    var ms = Number(opts.duration) || 4000;
    if (ms > 0) setTimeout(function () { dismiss(el); }, ms);
    return el;
  };

  // Server-rendered toasts (ui.Toast) are hidden markers: each id shows once.
  function scanToasts() {
    all("[data-ui-toast]").forEach(function (m) {
      var id = m.getAttribute("data-ui-toast");
      if (shownToasts.has(id)) return;
      shownToasts.add(id);
      ui.toast(m.textContent.trim(), {
        title: m.getAttribute("data-ui-title") || "",
        variant: m.getAttribute("data-ui-variant") || "",
        duration: Number(m.getAttribute("data-ui-duration")) || 0,
      });
    });
  }

  // ---- copy ---------------------------------------------------------------

  ui.copy = function (value) {
    var text = copyText(value, location.origin);
    var done = function () { ui.toast(text === value ? "Copied" : "Link copied"); };
    var fallback = function () {
      var ta = doc.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.cssText = "position:fixed;top:-1000px;opacity:0";
      doc.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = doc.execCommand("copy"); } catch (err) { ok = false; }
      ta.remove();
      if (ok) done();
      else ui.toast(text, { title: "Copy this", duration: -1 });
    };
    if (root.navigator && navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, fallback);
    else fallback();
  };

  // ---- menus --------------------------------------------------------------

  var openMenu = null; // { trigger, menu }

  function menuOf(trigger) {
    var box = closest(trigger, "[data-ui-menu-root]");
    return box ? box.querySelector("[data-ui-menu]") : null;
  }

  function placeOpenMenu() {
    var m = openMenu.menu;
    var width = Math.max(m.offsetWidth, 192);
    var pos = placeMenu(openMenu.trigger.getBoundingClientRect(), width, { width: root.innerWidth, height: root.innerHeight });
    m.style.left = pos.left + "px";
    m.style.top = pos.top === null ? "" : pos.top + "px";
    m.style.bottom = pos.bottom === null ? "" : pos.bottom + "px";
    m.style.maxHeight = pos.maxHeight + "px";
  }

  function closeMenu(focus) {
    if (!openMenu) return;
    var o = openMenu;
    openMenu = null;
    o.menu.hidden = true;
    o.trigger.setAttribute("aria-expanded", "false");
    if (focus && o.trigger.isConnected) o.trigger.focus();
  }

  function toggleMenu(trigger) {
    var same = openMenu && openMenu.trigger === trigger && !openMenu.menu.hidden;
    closeMenu(false);
    if (same) return;
    var menu = menuOf(trigger);
    if (!menu) return;
    openMenu = { trigger: trigger, menu: menu };
    menu.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    placeOpenMenu();
  }

  function menuItems() {
    return openMenu ? all('[role="menuitem"]:not([disabled]):not([aria-disabled="true"])', openMenu.menu) : [];
  }

  var reflowing = 0;
  function onReflow() {
    if (!openMenu || reflowing) return;
    reflowing = requestAnimationFrame(function () {
      reflowing = 0;
      if (!openMenu) return;
      if (openMenu.menu.hidden || !visible(openMenu.trigger)) closeMenu(false);
      else placeOpenMenu();
    });
  }

  // ---- dialogs ------------------------------------------------------------

  function openDialogs() {
    return all("[data-ui-dialog]").filter(function (d) { return !d.hidden; });
  }

  function focusFirst(scope) {
    var f = scope.querySelector("[autofocus], input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([data-ui-dialog-close]):not([disabled]), a[href]");
    if (f) f.focus();
  }

  ui.dialog = {
    open: function (id) {
      var d = typeof id === "string" ? doc.getElementById(id) : id;
      if (!d) return;
      d.__uiOpener = doc.activeElement;
      d.hidden = false;
      focusFirst(d);
    },
    close: function (el) {
      var d = typeof el === "string" ? doc.getElementById(el) : closest(el, "[data-ui-dialog]");
      if (!d) return;
      d.hidden = true;
      if (d.__uiOpener && d.__uiOpener.isConnected) d.__uiOpener.focus();
    },
  };

  function bounce(d) {
    var panel = d.querySelector(".ui-dialog-panel");
    if (!panel || panel.classList.contains("ui-bounce")) return;
    panel.classList.add("ui-bounce");
    setTimeout(function () { panel.classList.remove("ui-bounce"); }, 300);
  }

  // dismissDialog is Esc or a backdrop click: its close button decides -
  // a live event for a server-owned dialog, closing for a browser one.
  function dismissDialog(d) {
    if (d.hasAttribute("data-persistent")) {
      bounce(d);
      return;
    }
    var close = d.querySelector("[data-ui-dialog-close]");
    if (close) close.click();
    else ui.dialog.close(d.firstElementChild || d);
  }

  // ---- tabs ---------------------------------------------------------------

  function selectTab(tab) {
    var list = closest(tab, "[data-ui-tabs]");
    if (!list) return;
    all("[data-ui-tab]", list).forEach(function (t) {
      var on = t === tab;
      t.setAttribute("aria-selected", on ? "true" : "false");
      var panel = t.getAttribute("aria-controls");
      var el = panel && doc.getElementById(panel);
      if (el) el.hidden = !on;
    });
  }

  // ---- row clicks ---------------------------------------------------------

  var ROW_SKIP = "a, button, input, select, textarea, label, summary, [data-ui-menu-root], [data-ui-menu], [data-ui-no-row-click]";

  function onRowClick(e) {
    var row = closest(e.target, "[data-ui-href]");
    if (!row || e.defaultPrevented || e.button !== 0) return;
    if (closest(e.target, ROW_SKIP)) return;
    var sel = root.getSelection && root.getSelection();
    if (sel && String(sel).length > 0) return; // selecting text, not opening
    var href = row.getAttribute("data-ui-href");
    if (e.metaKey || e.ctrlKey) {
      root.open(href, "_blank");
      return;
    }
    // An in-app link: the view runtime's navigation follows it.
    var a = doc.createElement("a");
    a.href = href;
    a.setAttribute("data-nx-nav", "");
    a.hidden = true;
    doc.body.appendChild(a);
    a.click();
    a.remove();
  }

  // ---- hotkeys ------------------------------------------------------------

  function onHotkey(e) {
    if (e.defaultPrevented || e.isComposing) return;
    var typing = closest(e.target, "input, textarea, select, [contenteditable]") !== null;
    var dialogs = openDialogs();
    var top = dialogs[dialogs.length - 1];
    var hit = null;
    all("[data-ui-hotkey]").some(function (el) {
      if (el.disabled || el.getAttribute("aria-disabled") === "true" || !visible(el)) return false;
      if (top && !top.contains(el)) return false; // an open dialog owns the keyboard
      return el.getAttribute("data-ui-hotkey").split(",").some(function (combo) {
        var spec = parseHotkey(combo);
        if (typing && !hasModifier(spec)) return false;
        if (matchHotkey(spec, e, isMac)) {
          hit = el;
          return true;
        }
        return false;
      });
    });
    if (!hit) return;
    e.preventDefault();
    hit.click();
  }

  function onKeydown(e) {
    if (openMenu) {
      if (e.key === "Escape") {
        e.preventDefault();
        closeMenu(true);
        return;
      }
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        var items = menuItems();
        if (items.length) {
          e.preventDefault();
          var i = items.indexOf(doc.activeElement);
          i = e.key === "ArrowDown" ? (i + 1) % items.length : (i <= 0 ? items.length - 1 : i - 1);
          items[i].focus();
        }
        return;
      }
    }
    if (e.key === "Escape") {
      var dialogs = openDialogs();
      if (dialogs.length) {
        e.preventDefault();
        dismissDialog(dialogs[dialogs.length - 1]);
        return;
      }
    }
    onHotkey(e);
  }

  // ---- wiring -------------------------------------------------------------

  function onClick(e) {
    var t = e.target;
    var el;
    if ((el = closest(t, "[data-ui-menu-trigger]"))) {
      e.preventDefault();
      toggleMenu(el);
      return;
    }
    if (openMenu) {
      if (closest(t, '[data-ui-menu] [role="menuitem"]')) setTimeout(function () { closeMenu(false); }, 0);
      else if (!closest(t, "[data-ui-menu]")) closeMenu(false);
    }
    if ((el = closest(t, "[data-ui-copy]"))) {
      e.preventDefault();
      ui.copy(el.getAttribute("data-ui-copy"));
    }
    if ((el = closest(t, "[data-ui-open]"))) {
      e.preventDefault();
      ui.dialog.open(el.getAttribute("data-ui-open"));
      return;
    }
    if ((el = closest(t, "[data-ui-close]"))) {
      ui.dialog.close(el);
      return;
    }
    if (t && t.hasAttribute && t.hasAttribute("data-ui-dialog")) {
      dismissDialog(t); // the backdrop itself
      return;
    }
    if ((el = closest(t, "[data-ui-tab]")) && !el.disabled) selectTab(el);
    onRowClick(e);
  }

  function boot() {
    doc.addEventListener("click", onLoadingAttr, true);
    doc.addEventListener("submit", onLoadingAttr, true);
    doc.addEventListener("click", onClick);
    doc.addEventListener("keydown", onKeydown);
    root.addEventListener("scroll", onReflow, { capture: true, passive: true });
    root.addEventListener("resize", onReflow, { passive: true });
    scanToasts();
    var scan = 0;
    new MutationObserver(function (records) {
      var added = false;
      records.forEach(function (r) {
        if (r.type === "attributes") {
          if (r.target.hasAttribute("data-nx-live")) onBusy(r.target);
        } else if (r.addedNodes.length) {
          added = true;
        }
      });
      if (openMenu && !openMenu.menu.isConnected) openMenu = null;
      if (added && !scan) scan = setTimeout(function () { scan = 0; scanToasts(); }, 0);
    }).observe(doc.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ["aria-busy"] });
  }

  if (doc.readyState === "loading") doc.addEventListener("DOMContentLoaded", boot);
  else boot();
})(typeof window !== "undefined" ? window : globalThis);
