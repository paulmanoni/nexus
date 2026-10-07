// nexus view - the browser runtime for reactive templ pages.
//
// Served as-is from the app binary: no bundler, no build step. It reads the
// data-nx-* attributes the server rendered, restores the signals they refer
// to, and keeps bindings, event handlers and shards live. The compiled
// expressions ("twins") come from /_view/twins.js, loaded before this file.
(function () {
  "use strict";
  var root = typeof window !== "undefined" ? window : globalThis;

  // ---- Go-compatible helpers the compiled expressions call ----------------

  var goSpace =
    /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;

  // utf8len counts bytes, as Go's len does for a string.
  function utf8len(s) {
    var n = 0;
    for (var ch of s) {
      var c = ch.codePointAt(0);
      n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
    }
    return n;
  }

  // mapRunes applies a case mapping rune by rune and keeps a rune whose
  // mapping is not a single rune, as Go's simple case mapping does (\u00df stays \u00df).
  function mapRunes(s, f) {
    var out = "";
    for (var ch of s) {
      var m = f(ch);
      out += Array.from(m).length === 1 ? m : ch;
    }
    return out;
  }

  var nx = root.__nx || {};
  nx.len = function (v) {
    if (typeof v === "string") return utf8len(v);
    return v == null ? 0 : v.length;
  };
  nx.strings = {
    trimSpace: function (s) { return s.replace(goSpace, ""); },
    toUpper: function (s) { return mapRunes(s, function (c) { return c.toUpperCase(); }); },
    toLower: function (s) { return mapRunes(s, function (c) { return c.toLowerCase(); }); },
    contains: function (s, sub) { return s.indexOf(sub) !== -1; },
    hasPrefix: function (s, p) { return s.startsWith(p); },
    hasSuffix: function (s, p) { return s.endsWith(p); },
  };
  nx.strconv = { itoa: function (n) { return String(n); } };

  // ---- signals and effects ------------------------------------------------

  var active = null;
  var store = new Map();

  // signal returns the handle for id. The first value seen for an id wins:
  // a shard re-render that refers to a signal the page already holds keeps
  // the browser's current value.
  function signal(id, value) {
    var s = store.get(id);
    if (!s) {
      s = { id: id, value: value, subs: new Set() };
      store.set(id, s);
    }
    return handleOf(s);
  }

  function handleOf(s) {
    return {
      $sig: s.id,
      get: function () {
        if (active) {
          s.subs.add(active);
          active.deps.add(s);
        }
        return s.value;
      },
      set: function (v) {
        if (Object.is(s.value, v)) return;
        s.value = v;
        s.subs.forEach(schedule);
      },
      peek: function () { return s.value; },
    };
  }

  var pending = new Set();
  var flushQueued = false;
  function schedule(e) {
    if (e.disposed) return;
    pending.add(e);
    if (!flushQueued) {
      flushQueued = true;
      queueMicrotask(flush);
    }
  }
  function flush() {
    flushQueued = false;
    var list = Array.from(pending);
    pending.clear();
    list.forEach(run);
  }
  function effect(fn, owner) {
    var e = { fn: fn, deps: new Set(), disposed: false };
    owner.push(e);
    run(e);
    return e;
  }
  function run(e) {
    if (e.disposed) return;
    e.deps.forEach(function (s) { s.subs.delete(e); });
    e.deps.clear();
    var prev = active;
    active = e;
    try {
      e.fn();
    } catch (err) {
      console.error("nexus view:", err);
    } finally {
      active = prev;
    }
  }
  function dispose(e) {
    e.disposed = true;
    e.deps.forEach(function (s) { s.subs.delete(e); });
    e.deps.clear();
  }

  // hydrate turns the {"$sig": id, "v": value} references the server wrote
  // into live signal handles.
  function hydrate(v) {
    if (Array.isArray(v)) return v.map(hydrate);
    if (v && typeof v === "object") {
      if (typeof v.$sig === "string") return signal(v.$sig, v.v);
      var o = {};
      for (var k in v) o[k] = hydrate(v[k]);
      return o;
    }
    return v;
  }
  // track reads every signal inside v, so the running effect depends on it.
  function track(v) {
    if (Array.isArray(v)) return v.forEach(track);
    if (v && typeof v === "object") {
      if (typeof v.$sig === "string" && typeof v.get === "function") return void v.get();
      for (var k in v) track(v[k]);
    }
  }
  // snapshot is v with every signal replaced by its current value, for a
  // shard request.
  function snapshot(v) {
    if (Array.isArray(v)) return v.map(snapshot);
    if (v && typeof v === "object") {
      if (typeof v.$sig === "string" && typeof v.peek === "function") return { $sig: v.$sig, v: v.peek() };
      var o = {};
      for (var k in v) o[k] = snapshot(v[k]);
      return o;
    }
    return v;
  }

  function twin(id) {
    var t = (root.__nxTwins || {})[id];
    if (!t) throw new Error("nexus view: no compiled expression " + id + " - regenerate the templates");
    return t;
  }

  // ---- DOM wiring ---------------------------------------------------------

  var boolAttrs = { hidden: 1, disabled: 1, readonly: 1, required: 1, open: 1, multiple: 1, selected: 1 };

  function apply(el, name, v) {
    if (name === "text") {
      var t = v == null ? "" : String(v);
      if (el.textContent !== t) el.textContent = t;
    } else if (name === "show") {
      el.hidden = !v;
    } else if (name === "value") {
      var s = v == null ? "" : String(v);
      if (el.value !== s) el.value = s;
    } else if (name === "checked") {
      el.checked = !!v;
    } else if (typeof v === "boolean" || boolAttrs[name]) {
      el.toggleAttribute(name, !!v);
    } else if (v == null) {
      el.removeAttribute(name);
    } else {
      el.setAttribute(name, String(v));
    }
  }

  // wired remembers what each element was wired with, so wiring again (after
  // a live update patches the page) leaves unchanged elements alone and
  // rewires only those whose data-nx-* attributes changed.
  var wired = new WeakMap();

  function nxSignature(el) {
    var sig = "";
    Array.from(el.attributes).forEach(function (a) {
      if (a.name.indexOf("data-nx-bind-") === 0 || a.name.indexOf("data-nx-on-") === 0) sig += a.name + "=" + a.value + ";";
    });
    return sig;
  }

  function unwire(el) {
    var w = wired.get(el);
    if (!w) return;
    w.effects.forEach(dispose);
    w.listeners.forEach(function (l) { el.removeEventListener(l[0], l[1]); });
    wired.delete(el);
  }

  function wire(el, owner) {
    var sig = nxSignature(el);
    var prev = wired.get(el);
    if (prev && prev.sig === sig) return;
    unwire(el);
    if (!sig) return;
    var w = { sig: sig, effects: [], listeners: [] };
    wired.set(el, w);
    Array.from(el.attributes).forEach(function (a) {
      var spec, caps, fn;
      if (a.name.indexOf("data-nx-bind-") === 0) {
        var name = a.name.slice("data-nx-bind-".length);
        spec = JSON.parse(a.value);
        caps = hydrate(spec.caps || {});
        fn = twin(spec.fn);
        var e = effect(function () { apply(el, name, fn(caps)); }, owner);
        w.effects.push(e);
      } else if (a.name.indexOf("data-nx-on-") === 0) {
        var event = a.name.slice("data-nx-on-".length);
        spec = JSON.parse(a.value);
        caps = hydrate(spec.caps || {});
        fn = twin(spec.fn);
        var handler = function (ev) {
          try {
            fn(caps, ev);
          } catch (err) {
            console.error("nexus view:", err);
          }
        };
        el.addEventListener(event, handler);
        w.listeners.push([event, handler]);
      }
    });
  }

  function walk(el, owner) {
    if (el.nodeType !== 1) return;
    if (el.tagName === "NX-SHARD") {
      shard(el, owner);
      return;
    }
    if (el.tagName === "NX-ISLAND") {
      island(el); // its insides belong to the island's framework
      return;
    }
    wire(el, owner);
    Array.from(el.children).forEach(function (c) { walk(c, owner); });
  }

  // shard keeps a server-rendered component live. Its last child, a
  // <template data-nx-shard-meta>, says which component it is, where it sits
  // on the page, its arguments, the signals it reads on the server and the
  // signals it owns. When a signal it reads changes, the browser posts the
  // arguments and the current values of its own signals, and swaps in the
  // HTML the server renders. Changes in one tick share one request, and a
  // newer request aborts an older one.
  function shard(el, owner) {
    var inner = [];
    var watcher = null;
    var ctrl = null;

    function meta() {
      var t = el.querySelector(":scope > template[data-nx-shard-meta]");
      return t ? JSON.parse(t.getAttribute("data-nx-shard-meta")) : null;
    }

    function mount() {
      Array.from(el.children).forEach(function (c) { walk(c, inner); });
      var m = meta();
      if (!m) return;
      var args = hydrate(m.args || []);
      var reads = hydrate(m.reads || []);
      var first = true;
      watcher = effect(function () {
        track(reads);
        if (first) {
          first = false;
          return;
        }
        refresh(m, args);
      }, owner);
    }

    function refresh(m, args) {
      if (ctrl) ctrl.abort();
      var mine = (ctrl = new AbortController());
      var states = {};
      (m.states || []).forEach(function (id) {
        var s = store.get(id);
        if (s) states[id] = s.value;
      });
      el.setAttribute("aria-busy", "true");
      fetch("/_view/shard/" + encodeURIComponent(m.shard), {
        method: "POST",
        headers: csrfHeaders({ "Content-Type": "application/json", Accept: "text/html" }),
        body: JSON.stringify({ path: m.path, args: snapshot(args), states: states }),
        signal: mine.signal,
        credentials: "same-origin",
      })
        .then(function (res) {
          if (!res.ok) throw new Error("shard " + m.shard + ": HTTP " + res.status);
          return res.text();
        })
        .then(function (html) {
          if (mine !== ctrl) return;
          inner.forEach(dispose);
          inner.length = 0;
          if (watcher) dispose(watcher);
          el.innerHTML = html;
          ownFields(el);
          mount();
          sweepIslands();
        })
        .catch(function (err) {
          if (err.name !== "AbortError") console.error("nexus view:", err);
        })
        .finally(function () {
          if (mine === ctrl) el.removeAttribute("aria-busy");
        });
    }

    mount();
  }

  // ---- live pages ---------------------------------------------------------

  // morph patches el to match next in place: attributes and children are
  // updated, matching elements by id or else by position and tag, so focus,
  // selection and the value being typed survive a live update.
  var resetting = new Set(); // view.Forms whose fields a morph resets

  function morph(el, next) {
    // data-nx-ignore: the element is the browser's while its id holds (a
    // chart a script drew into, say); a new id replaces it.
    if (el.hasAttribute("data-nx-ignore") && el.id && el.id === next.id) return;
    if (el.hasAttribute("data-nx-stream") && next.hasAttribute("data-nx-stream")) {
      morphStream(el, next);
      return;
    }
    if (el.tagName === "NX-ISLAND" && islands.has(el)) {
      // A mounted island keeps its DOM: new props reach it through walk.
      // Another island in its place unmounts it, and the fallback returns.
      if (next.getAttribute("data-c") === el.getAttribute("data-c")) {
        var mounted = el.getAttribute("data-nx-island"); // the browser's own
        syncAttributes(el, next);
        if (mounted) el.setAttribute("data-nx-island", mounted);
        return;
      }
      unmountIsland(el);
    }
    if (!isField(el)) {
      // A view.Form loaded or reset (its generation moved): its fields take
      // the server's values, typed or not, and nothing is touched any more.
      var reset = el.tagName === "FORM" && el.hasAttribute("data-nx-form") &&
        el.getAttribute("data-nx-gen") !== next.getAttribute("data-nx-gen");
      syncAttributes(el, next);
      if (reset) {
        resetting.add(el);
        formTouches.delete(el);
      }
      morphChildren(el, next);
      if (reset) resetting.delete(el);
      return;
    }
    // A form field. What the user sees is kept while the field has focus;
    // otherwise the field takes the server's value when that value changed
    // since the last render - or on every render, for a field marked
    // data-nx-value (view.Value).
    var focused = typeof document !== "undefined" && el === document.activeElement;
    var forced = !!(el.form && resetting.has(el.form));
    var seen = fieldState(el);
    var before = serverValue(el);
    syncAttributes(el, next);
    if (el.tagName === "TEXTAREA") {
      if (el.textContent !== next.textContent) el.textContent = next.textContent;
    } else {
      morphChildren(el, next);
    }
    if (forced) takeServerValue(el);
    else if (focused) restoreField(el, seen);
    else if (el.hasAttribute("data-nx-value") || serverValue(el) !== before) takeServerValue(el);
  }

  // morphStream applies a view.Stream's change to its list once: the list
  // keeps its rows, and the render carries only the rows that changed.
  function morphStream(el, next) {
    var applied = el.__nxStream !== undefined ? el.__nxStream : el.getAttribute("data-nx-stream-seq");
    var seq = next.getAttribute("data-nx-stream-seq");
    syncAttributes(el, next);
    el.__nxStream = seq;
    if (seq === applied) return;
    var ops = {};
    try {
      ops = JSON.parse(next.getAttribute("data-nx-stream-ops") || "{}");
    } catch (err) {}
    var byId = function (id) {
      for (var c = el.firstElementChild; c; c = c.nextElementSibling) if (c.id === id) return c;
      return null;
    };
    if (ops.r) while (el.firstChild) el.removeChild(el.firstChild);
    (ops.d || []).forEach(function (id) {
      var c = byId(id);
      if (c) el.removeChild(c);
    });
    var at = {};
    (ops.i || []).forEach(function (p) { at[p[0]] = p[1]; });
    Array.from(next.children).forEach(function (child) {
      var old = child.id ? byId(child.id) : null;
      if (old) {
        morph(old, child);
        return;
      }
      var pos = at[child.id];
      var before = typeof pos === "number" && pos >= 0 ? el.children[pos] : null;
      if (before) el.insertBefore(child, before);
      else el.appendChild(child);
    });
    if (ops.l > 0) while (el.children.length > ops.l) el.removeChild(el.lastElementChild);
    if (ops.l < 0) while (el.children.length > -ops.l) el.removeChild(el.firstElementChild);
  }

  function isField(el) {
    return el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT";
  }

  function checkable(el) {
    return el.tagName === "INPUT" && (el.type === "checkbox" || el.type === "radio");
  }

  // serverValue is a field's value as the server rendered it: an input's
  // value (or checked) attribute, a textarea's text, the options a select
  // marks selected - or, under view.Value, the field's value attribute.
  function serverValue(el) {
    if (checkable(el)) return el.hasAttribute("checked") ? "1" : "";
    if (el.hasAttribute("data-nx-value") && el.hasAttribute("value")) return el.getAttribute("value");
    if (el.tagName === "TEXTAREA") return el.textContent;
    if (el.tagName === "SELECT") return markedOptions(el);
    return el.getAttribute("value");
  }

  // takeServerValue sets what the user sees to the server's value.
  function takeServerValue(el) {
    if (checkable(el)) {
      el.checked = el.hasAttribute("checked");
      return;
    }
    var owned = el.hasAttribute("data-nx-value") && el.hasAttribute("value");
    if (el.tagName === "SELECT") {
      if (owned) {
        var want = el.getAttribute("value");
        if (el.value !== want) el.value = want;
        return;
      }
      var any = false;
      Array.from(el.options).forEach(function (o) {
        o.selected = o.hasAttribute("selected");
        any = any || o.selected;
      });
      if (!any && !el.multiple && el.options.length) el.selectedIndex = 0;
      return;
    }
    var v = owned || el.tagName !== "TEXTAREA" ? el.getAttribute("value") : el.textContent;
    if (v == null) v = "";
    if (el.value !== v) el.value = v;
  }

  // fieldState is what the user sees in a field; restoreField puts it back
  // after a patch, so a focused field is never overwritten (a browser lets a
  // changed value or selected attribute through to a field the user has not
  // edited yet).
  function fieldState(el) {
    if (checkable(el)) return el.checked;
    if (el.tagName === "SELECT") return Array.from(el.options).map(function (o) { return o.selected ? o.value : null; });
    return el.value;
  }

  function restoreField(el, seen) {
    if (checkable(el)) {
      if (el.checked !== seen) el.checked = seen;
    } else if (el.tagName === "SELECT") {
      // By value: the options may have changed under the user.
      var chosen = new Set(seen.filter(function (v) { return v !== null; }));
      Array.from(el.options).forEach(function (o) {
        var on = chosen.has(o.value);
        if (o.selected !== on) o.selected = on;
      });
    } else if (el.value !== seen) {
      el.value = seen;
    }
  }

  // markedOptions are the options the server marks selected, as it rendered
  // them last.
  function markedOptions(select) {
    return Array.from(select.options)
      .filter(function (o) { return o.hasAttribute("selected"); })
      .map(function (o) { return o.value; })
      .join("\u0000");
  }

  // ownFields gives the fields under root marked with view.Value their
  // server value: a select or a textarea doesn't read a value attribute on
  // its own.
  function ownFields(root) {
    var list = Array.from(root.querySelectorAll("[data-nx-value]"));
    if (root.hasAttribute("data-nx-value")) list.unshift(root);
    list.forEach(function (el) {
      if (isField(el) && !(typeof document !== "undefined" && el === document.activeElement)) takeServerValue(el);
    });
  }

  function syncAttributes(el, next) {
    Array.from(el.attributes).forEach(function (a) {
      if (a.name === "aria-busy" && busyEls.has(el)) return; // its reply is on its way
      if (!next.hasAttribute(a.name)) el.removeAttribute(a.name);
    });
    Array.from(next.attributes).forEach(function (a) {
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    });
    keepJS(el, next);
  }

  // busyEls are the elements waiting for their event's reply: a form being
  // submitted, a data-nx-busy element whose event went. They carry
  // aria-busy, which a re-render meanwhile leaves alone, until the reply.
  var busyEls = new Set();

  function markBusy(state, ref, el) {
    el.setAttribute("aria-busy", "true");
    busyEls.add(el);
    (state.submits[ref] = state.submits[ref] || []).push(el);
  }

  function unmarkBusy(el) {
    busyEls.delete(el);
    el.removeAttribute("aria-busy");
  }

  // ---- JS commands (view.JS) ----------------------------------------------

  // jsState holds, per element, each attribute a command changed: the
  // server's value when it did and the value it left. A re-render that
  // renders the attribute as before gets the command's value back; one that
  // renders it differently wins, and the attribute is the server's again.
  var jsState = new WeakMap();

  function keepJS(el, next) {
    var touched = jsState.get(el);
    if (!touched) return;
    touched.forEach(function (rec, name) {
      if (next.getAttribute(name) !== rec.server) {
        touched.delete(name);
        return;
      }
      if (rec.value === null) el.removeAttribute(name);
      else if (el.getAttribute(name) !== rec.value) el.setAttribute(name, rec.value);
    });
    if (!touched.size) jsState.delete(el);
  }

  // jsTouch runs change on el, remembering what it does to attribute name.
  function jsTouch(el, name, change) {
    var touched = jsState.get(el);
    if (!touched) jsState.set(el, (touched = new Map()));
    var rec = touched.get(name);
    if (!rec) touched.set(name, (rec = { server: el.getAttribute(name) }));
    change();
    rec.value = el.getAttribute(name);
  }

  function jsTargets(el, a) {
    if (a.scope === "within") {
      var box = el.closest(a.within);
      if (!box) return [];
      return a.to ? Array.from(box.querySelectorAll(a.to)) : [box];
    }
    if (!a.to) return [el];
    if (a.scope === "closest") {
      var c = el.closest(a.to);
      return c ? [c] : [];
    }
    return Array.from((a.scope === "inner" ? el : document).querySelectorAll(a.to));
  }

  // styleDisplay reads and setDisplay writes the display declaration of the
  // style attribute: the attribute, not the style object, is what the page
  // re-renders and what a command must keep.
  var displayDecl = /(^|;)\s*display\s*:[^;]*;?/i;
  function styleDisplay(el) {
    var m = /(?:^|;)\s*display\s*:\s*([^;]*)/i.exec(el.getAttribute("style") || "");
    return m ? m[1].trim() : "";
  }
  function setDisplay(el, value) {
    jsTouch(el, "style", function () {
      var rest = (el.getAttribute("style") || "").replace(displayDecl, "$1").replace(/^;|;\s*$/, "").trim();
      var style = value ? (rest ? rest + "; " : "") + "display: " + value : rest;
      if (style) el.setAttribute("style", style);
      else el.removeAttribute("style");
    });
  }
  function shown(el) {
    if (el.hasAttribute("hidden") || styleDisplay(el) === "none") return false;
    return typeof getComputedStyle !== "function" || getComputedStyle(el).display !== "none";
  }
  function show(el, a) {
    if (el.hasAttribute("hidden")) jsTouch(el, "hidden", function () { el.removeAttribute("hidden"); });
    setDisplay(el, a.display || "");
    if (!a.display && typeof getComputedStyle === "function" && getComputedStyle(el).display === "none") setDisplay(el, "block");
  }
  function classes(a) { return words(a.names); }
  function words(s) { return String(s || "").split(/\s+/).filter(Boolean); }

  // A transition's classes are the browser's alone: they come and go with
  // it and are not kept across re-renders. A new one on an element cancels
  // the one running there.
  var running = new WeakMap();
  function nextFrame(fn) {
    if (typeof requestAnimationFrame === "function") requestAnimationFrame(function () { requestAnimationFrame(fn); });
    else setTimeout(fn, 16);
  }
  function animate(el, a, before, after) {
    var prev = running.get(el);
    if (prev) prev.finish();
    var anim = a.anim || ["", "", ""];
    var during = words(anim[0]), from = words(anim[1]), to = words(anim[2]);
    var done = false, timer = null;
    var finish = function () {
      if (done) return;
      done = true;
      clearTimeout(timer);
      during.concat(from, to).forEach(function (c) { el.classList.remove(c); });
      running.delete(el);
      if (after) after();
    };
    running.set(el, { finish: finish });
    during.concat(from).forEach(function (c) { el.classList.add(c); });
    if (before) before();
    nextFrame(function () {
      if (done) return;
      from.forEach(function (c) { el.classList.remove(c); });
      to.forEach(function (c) { el.classList.add(c); });
      timer = setTimeout(finish, a.time == null ? 200 : a.time);
    });
  }
  function showAnimated(el, a) {
    if (a.anim) animate(el, a, function () { show(el, a); });
    else show(el, a);
  }
  function hideAnimated(el, a) {
    if (a.anim) animate(el, a, null, function () { setDisplay(el, "none"); });
    else setDisplay(el, "none");
  }

  var focusStack = [];
  var focusable = "a[href], button:not([disabled]), input:not([disabled]):not([type=hidden]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";
  function firstFocusable(scope) {
    var auto = scope.querySelector("[autofocus]");
    if (auto) return auto;
    return Array.from(scope.querySelectorAll(focusable)).find(function (f) { return !f.closest("[data-nx-nofocus]"); });
  }

  var jsOps = {
    show: function (el, a) { jsTargets(el, a).forEach(function (t) { showAnimated(t, a); }); },
    hide: function (el, a) { jsTargets(el, a).forEach(function (t) { hideAnimated(t, a); }); },
    toggle: function (el, a) {
      jsTargets(el, a).forEach(function (t) { if (shown(t)) hideAnimated(t, a); else showAnimated(t, a); });
    },
    transition: function (el, a) {
      jsTargets(el, a).forEach(function (t) { animate(t, { anim: [a.names, "", ""], time: a.time }); });
    },
    push_focus: function (el, a) {
      var t = jsTargets(el, a)[0];
      if (t) focusStack.push(t);
    },
    pop_focus: function () {
      var t = focusStack.pop();
      if (t && t.isConnected) t.focus();
    },
    exec: function (el, a, e) {
      jsTargets(el, a).forEach(function (t) {
        var code = (t.getAttribute(a.attr) || "").trim();
        if (!code) return;
        if (code.charAt(0) === "[") nx.js(t, e, JSON.parse(code));
        else new Function("event", code).call(t, e);
      });
    },
    dispatch: function (el, a) {
      jsTargets(el, a).forEach(function (t) {
        t.dispatchEvent(new CustomEvent(a.event, { bubbles: a.bubbles !== false, detail: a.detail }));
      });
    },
    add_class: function (el, a) {
      jsTargets(el, a).forEach(function (t) { jsTouch(t, "class", function () { classes(a).forEach(function (c) { t.classList.add(c); }); }); });
    },
    remove_class: function (el, a) {
      jsTargets(el, a).forEach(function (t) { jsTouch(t, "class", function () { classes(a).forEach(function (c) { t.classList.remove(c); }); }); });
    },
    toggle_class: function (el, a) {
      jsTargets(el, a).forEach(function (t) { jsTouch(t, "class", function () { classes(a).forEach(function (c) { t.classList.toggle(c); }); }); });
    },
    set_attr: function (el, a) {
      jsTargets(el, a).forEach(function (t) { jsTouch(t, a.name, function () { t.setAttribute(a.name, a.value); }); });
    },
    remove_attr: function (el, a) {
      jsTargets(el, a).forEach(function (t) { jsTouch(t, a.name, function () { t.removeAttribute(a.name); }); });
    },
    toggle_attr: function (el, a) {
      jsTargets(el, a).forEach(function (t) {
        jsTouch(t, a.name, function () {
          if (t.hasAttribute(a.name)) t.removeAttribute(a.name);
          else t.setAttribute(a.name, a.value);
        });
      });
    },
    focus: function (el, a) {
      var t = jsTargets(el, a)[0];
      if (t) t.focus();
    },
    focus_first: function (el, a) {
      var t = jsTargets(el, a)[0];
      var f = t && firstFocusable(t);
      if (f) f.focus();
    },
    push: function (el, a) { nx.live.send(el, a.event, a.args, a.ct); },
    // confirm stops the commands after it unless the user agrees.
    confirm: function (el, a) { return window.confirm(a.message); },
    set_value: function (el, a) {
      jsTargets(el, a).forEach(function (t) {
        if (!("value" in t)) return;
        t.value = a.value;
        t.dispatchEvent(new Event("input", { bubbles: true }));
        t.dispatchEvent(new Event("change", { bubbles: true }));
      });
    },
    copy: function (el, a) {
      var text = a.text;
      if (text == null) {
        var t = jsTargets(el, a)[0];
        if (!t) return;
        text = "value" in t && t.tagName !== "BUTTON" ? t.value : t.textContent.trim();
      }
      copyText(text);
      el.setAttribute("data-copied", "");
      setTimeout(function () { el.removeAttribute("data-copied"); }, 1500);
    },
    scroll_to: function (el, a) {
      var t = jsTargets(el, a)[0];
      if (t) t.scrollIntoView({ block: "nearest", behavior: "smooth" });
    },
  };

  // copyText puts text on the clipboard; without the clipboard API (a page
  // not served over https), through a selected textarea.
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).catch(function () { copyByCommand(text); });
      return;
    }
    copyByCommand(text);
  }
  function copyByCommand(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (err) { /* nothing to copy with */ }
    ta.remove();
  }

  // runPushed runs what the page's views pushed with a reply (PushJS,
  // PushEvent), once it is applied; a command's own element is the live root.
  function runPushed(root, pushed) {
    (pushed || []).forEach(function (p) {
      try {
        if (p.js) nx.js(root, null, p.js);
        else if (p.event) window.dispatchEvent(new CustomEvent(p.event, { detail: p.detail }));
      } catch (err) {
        console.error("nexus view: a pushed command failed:", err);
      }
    });
  }

  // js runs what view.JS rendered: [[command, args], ...], in order.
  nx.js = function (el, e, ops) {
    for (var i = 0; i < ops.length; i++) {
      if (ops[i][0] === "debounce" || ops[i][0] === "throttle") {
        // The steps after it run at its pace.
        var rest = ops.slice(i + 1);
        nx[ops[i][0]](el, e, (ops[i][1] || {}).ms || 0, function () { nx.js(el, e, rest); });
        return;
      }
      var run = jsOps[ops[i][0]];
      if (!run) {
        console.warn("nexus view: unknown JS command " + ops[i][0]);
        continue;
      }
      if (run(el, ops[i][1] || {}, e) === false) return; // a confirm declined
    }
  };

  // debounce and throttle run fn, the steps after a view.Debounce or
  // view.Throttle in a chain, at a pace kept per element and event type. A submit held back is still kept from
  // reaching the server as a page load.
  var paced = new WeakMap();
  function pacedFor(el, key) {
    var m = paced.get(el);
    if (!m) paced.set(el, (m = {}));
    return m[key] || (m[key] = { el: el });
  }
  function holdSubmit(e) {
    if (e && e.type === "submit") e.preventDefault();
  }
  var debouncing = new Set();
  nx.debounce = function (el, e, ms, fn) {
    holdSubmit(e);
    var type = e ? e.type : "";
    var p = pacedFor(el, "debounce " + type);
    clearTimeout(p.timer);
    p.type = type;
    p.run = function () {
      clearTimeout(p.timer);
      debouncing.delete(p);
      fn.call(el, e);
    };
    debouncing.add(p);
    p.timer = setTimeout(p.run, ms);
  };
  nx.throttle = function (el, e, ms, fn) {
    var p = pacedFor(el, "throttle " + (e ? e.type : ""));
    if (p.closed) {
      holdSubmit(e);
      // A field's last value still goes when the interval is up.
      if (e && (e.type === "input" || e.type === "change")) p.last = function () { fn.call(el, e); };
      return;
    }
    p.closed = true;
    var reopen = function () {
      var last = p.last;
      p.last = null;
      if (!last) {
        p.closed = false;
        return;
      }
      last();
      setTimeout(reopen, ms);
    };
    setTimeout(reopen, ms);
    fn.call(el, e);
  };
  // A form being submitted first runs the debounced scripts waiting inside
  // it, so the submit never overtakes them.
  if (typeof document !== "undefined") {
    document.addEventListener("submit", function (e) {
      var form = e.target;
      debouncing.forEach(function (p) {
        if (p.type !== "submit" && form.contains && form.contains(p.el)) p.run();
      });
    }, true);
  }

  function sameKind(a, b) {
    if (a.nodeType !== b.nodeType) return false;
    if (a.nodeType !== 1) return true;
    if (a.tagName !== b.tagName) return false;
    return !a.id || !b.id || a.id === b.id;
  }

  function morphChildren(el, next) {
    var kids = Array.from(next.childNodes);
    var cur = el.firstChild;
    kids.forEach(function (n) {
      var match = null;
      if (n.nodeType === 1 && n.id) {
        // The same id on another tag is a new element: a disabled <span>
        // that became a link is replaced, not given the link's attributes.
        var byId = Array.from(el.children).find(function (c) { return c.id === n.id; });
        if (byId && byId.tagName === n.tagName) match = byId;
      }
      if (!match && cur && sameKind(cur, n)) match = cur;
      if (match) {
        if (match !== cur) el.insertBefore(match, cur);
        if (n.nodeType === 1) morph(match, n);
        else if (match.nodeValue !== n.nodeValue) match.nodeValue = n.nodeValue;
        cur = match.nextSibling;
      } else {
        el.insertBefore(document.importNode(n, true), cur);
      }
    });
    while (cur) {
      var gone = cur;
      cur = cur.nextSibling;
      if (gone.nodeType === 1) {
        unwire(gone);
        Array.from(gone.querySelectorAll("*")).forEach(unwire);
      }
      el.removeChild(gone);
    }
  }

  // reapply re-runs the bindings under root after a live patch. The server
  // renders a signal's value as it knows it, but browser-side state (what
  // the user typed into a field bound to a signal) is newer: the signal wins.
  function reapply(root) {
    [root].concat(Array.from(root.querySelectorAll("*"))).forEach(function (el) {
      var w = wired.get(el);
      if (w) w.effects.forEach(run);
    });
  }

  // Live updates arrive as patches against the previous render, cut into
  // tokens exactly as the server cuts them (diff.go): n copies n old tokens,
  // -n skips n, a string is inserted, [p, n] inserts n old tokens from p.
  function tokenize(html) {
    var out = [];
    var start = 0;
    for (var i = 0; i < html.length; i++) {
      var ch = html.charCodeAt(i);
      if (ch === 60) { // <
        if (i > start) {
          out.push(html.slice(start, i));
          start = i;
        }
      } else if (ch === 62 || ch === 34) { // > "
        out.push(html.slice(start, i + 1));
        start = i + 1;
      } else if (ch === 59 && i >= 4 && html.slice(i - 4, i + 1) === "&#34;") { // an escaped quote
        out.push(html.slice(start, i + 1));
        start = i + 1;
      }
    }
    if (start < html.length) out.push(html.slice(start));
    return out;
  }

  // The connection's dictionary of long tokens, built exactly as the
  // server builds it (diff.go): every token of 16+ characters in order of
  // first appearance, up to 4096, reset with every full render.
  function newDict() { return { ids: new Map(), words: [] }; }

  function observe(dict, tokens) {
    tokens.forEach(function (t) {
      if (utf8len(t) < 16 || dict.words.length >= 4096 || dict.ids.has(t)) return;
      dict.ids.set(t, dict.words.length);
      dict.words.push(t);
    });
  }

  function applyPatch(old, patch, dict) {
    var out = [];
    var i = 0;
    patch.forEach(function (step) {
      if (typeof step === "string") {
        tokenize(step).forEach(function (t) { out.push(t); });
      } else if (Array.isArray(step)) {
        if (step.length === 1) out.push(dict.words[step[0]]);
        else for (var r = 0; r < step[1]; r++) out.push(old[step[0] + r]);
      } else if (step > 0) {
        for (var n = 0; n < step; n++) out.push(old[i++]);
      } else {
        i -= step;
      }
    });
    return out;
  }

  // _patcher is the browser side of a connection's patches, for tests.
  nx._patcher = function () {
    var last = [], dict = newDict();
    return {
      full: function (html) { last = tokenize(html); dict = newDict(); observe(dict, last); },
      apply: function (patchJSON) {
        last = applyPatch(last, JSON.parse(patchJSON), dict);
        observe(dict, last);
        return last.join("");
      },
    };
  };

  // A live page's replies carry its render tree, or the change to it
  // (rdiff.go): frames of statics (sent once per connection, by id) with the
  // dynamics between them - markup, nested frames, loops of item frames.
  // The browser keeps the tree, applies each change, and renders it back to
  // markup to morph the page with, as Phoenix LiveView does.
  function treeFrame(obj, statics) {
    if (obj.s) statics.set(obj.t, obj.s);
    return { t: obj.t, d: obj.d.map(function (v) { return treeDyn(v, statics); }) };
  }

  // Long markup the connection keeps: {r: id, v: markup} the first time,
  // {r: id} after that. Kept beside the statics, under negative keys.
  function treeDyn(v, statics) {
    if (typeof v === "string") return v;
    if (v.r !== undefined) {
      if (v.v !== undefined) statics.set(-1 - v.r, v.v);
      var kept = statics.get(-1 - v.r);
      if (kept === undefined) throw new Error("no string " + v.r);
      return kept;
    }
    if (v.c) return { c: v.c.map(function (f) { return treeFrame(f, statics); }) };
    return treeFrame(v, statics);
  }

  function treeUpdate(frame, obj, statics) {
    Object.keys(obj.u).forEach(function (k) {
      var i = +k;
      if (!(i < frame.d.length)) throw new Error("no dynamic " + k);
      frame.d[i] = treeChange(frame.d[i], obj.u[k], statics);
    });
  }

  function treeChange(old, change, statics) {
    if (typeof change === "string") return change;
    if (change.u) {
      if (!old || !old.d) throw new Error("an update of a non-frame");
      treeUpdate(old, change, statics);
      return old;
    }
    if (change.k) {
      if (!old || !old.c) throw new Error("item steps on a non-loop");
      var out = [], i = 0;
      change.k.forEach(function (step) {
        if (typeof step === "number") {
          if (step > 0) {
            if (i + step > old.c.length) throw new Error("past the items");
            for (var n = 0; n < step; n++) out.push(old.c[i++]);
          } else {
            i -= step;
          }
        } else if (step.u) {
          if (i >= old.c.length) throw new Error("past the items");
          treeUpdate(old.c[i], step, statics);
          out.push(old.c[i++]);
        } else {
          out.push(treeFrame(step, statics));
        }
      });
      return { c: out };
    }
    if (change.p) {
      if (typeof old !== "string") throw new Error("a patch of a non-string");
      return applyPatch(tokenize(old), change.p, newDict()).join("");
    }
    return treeDyn(change, statics);
  }

  function treeHTML(frame, statics) {
    var s = statics.get(frame.t);
    if (!s || s.length !== frame.d.length + 1) throw new Error("no statics " + frame.t);
    var out = s[0];
    for (var i = 0; i < frame.d.length; i++) {
      var d = frame.d[i];
      if (typeof d === "string") out += d;
      else if (d.c) d.c.forEach(function (f) { out += treeHTML(f, statics); });
      else out += treeHTML(d, statics);
      out += s[i + 1];
    }
    return out;
  }

  // applyTree applies a reply's tree to what the connection holds and
  // returns the page's markup.
  function applyTree(state, msg) {
    if (msg.reset || !state.statics) state.statics = new Map();
    if (msg.full) state.tree = treeFrame(msg.tree, state.statics);
    else if (!state.tree) throw new Error("a change with no tree");
    else treeUpdate(state.tree, msg.tree, state.statics);
    return treeHTML(state.tree, state.statics);
  }

  // _tree is the browser side of a connection's trees, for tests.
  nx._tree = function () {
    var state = {};
    return {
      apply: function (msgJSON) { return applyTree(state, JSON.parse(msgJSON)); },
    };
  };

  // live connects each live root to its socket: the server sends the page's
  // tree after mounting and the change after every event; the browser
  // patches the page in place. A dropped connection reconnects and carries
  // on with the page's state on the server (resume.go); when that state is
  // gone the page mounts afresh, and the browser first sends its forms back
  // so what the user typed survives.
  var liveRoots = new Map(); // root element -> { ws }
  var changeEvents = new WeakMap(); // form -> the view.Change event it sends

  // The page's first render came over HTTP: the first connection names it
  // (data-nx-live-join) so the server sends nothing it already has. A
  // reconnect gets a fresh mount and its full render. Events sent while
  // disconnected wait in a queue and go out once the socket is back.
  function connectLive(root) {
    var path = root.getAttribute("data-nx-live");
    var join = root.getAttribute("data-nx-live-join");
    var state = { ws: null, delay: 500, timer: null, ref: 0, submits: {}, queue: [], path: path, closed: false,
      statics: null, tree: null, resume: "", recovering: new Set(), opened: false };
    liveRoots.set(root, state);
    function open() {
      clearTimeout(state.timer);
      state.timer = null;
      var url = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + state.path +
        "?url=" + encodeURIComponent(location.pathname + location.search);
      if (join) url += "&join=" + encodeURIComponent(join);
      else if (state.resume) url += "&resume=" + encodeURIComponent(state.resume);
      join = null; // a join is good for the first connection only
      var ws = new WebSocket(url);
      state.ws = ws;
      ws.onopen = function () {
        state.delay = 500;
        root.setAttribute("data-nx-live-state", "connected");
        if (state.opened) recover(state, root, ws);
        state.opened = true;
        var queued = state.queue;
        state.queue = [];
        queued.forEach(function (msg) { ws.send(JSON.stringify(msg)); });
      };
      ws.onmessage = function (m) {
        var msg = JSON.parse(m.data);
        unsentForms.clear(); // what was typed has reached the server: its render says if it's dirty
        if (msg.resume) state.resume = msg.resume;
        if (msg.uploads) startUploads(msg.uploads);
        if (msg.redirect) {
          // Not a live page this connection can open: load it.
          state.closed = true;
          ws.close();
          liveRoots.delete(root);
          navigate(msg.redirect, state.navPush !== false);
          return;
        }
        var push = state.navPush !== false;
        if (msg.patch || msg.nav) state.navPush = undefined;
        if (msg.nav) {
          // Another live page took the connection: it is this root's now.
          state.path = msg.live;
          root.setAttribute("data-nx-live", msg.live);
        }
        var recovered = msg.ref && state.recovering.delete(msg.ref);
        if (msg.ref && msg.ref === state.priming) {
          // The tree of the page as it is (prime): kept, nothing to patch.
          state.priming = 0;
          try {
            if (msg.tree) applyTree(state, msg);
          } catch (err) {
            state.tree = null;
          }
          return;
        }
        if (!msg.tree && !msg.error && !msg.ref) {
          // The resume token alone: the page came over HTTP and the socket
          // had nothing to send. Ask for its tree once things are quiet, so
          // the first navigation can travel as a change too.
          if (!state.tree) prime(state, ws);
          runPushed(root, msg.push);
          return;
        }
        root.removeAttribute("aria-busy");
        var waiting = msg.ref ? state.submits[msg.ref] : null;
        if (msg.ref) delete state.submits[msg.ref];
        if (waiting) waiting.forEach(unmarkBusy);
        // The form this event submitted, if it was one: reset on success.
        var submitted = waiting && waiting.filter(function (el) { return el.tagName === "FORM"; })[0];
        if (msg.error && !recovered) {
          console.error("nexus view: live:", msg.error);
          root.setAttribute("data-nx-live-error", msg.error);
          return;
        }
        if (!msg.error) root.removeAttribute("data-nx-live-error");
        var html;
        try {
          html = msg.tree ? applyTree(state, msg) : state.tree && treeHTML(state.tree, state.statics);
        } catch (err) {
          // Out of step with the server: ask for the whole tree.
          state.tree = null;
          ws.send(JSON.stringify({ event: "__resync" }));
          return;
        }
        // While forms are being sent back after a fresh mount, hold the page
        // as it is: the fresh render would close what they reopen.
        if (state.recovering.size > 0 || !html) {
          runPushed(root, msg.push);
          return;
        }
        var next;
        if (root.tagName === "BODY") {
          var doc = new DOMParser().parseFromString(html, "text/html");
          if (msg.nav) mergeHead(doc.head);
          next = doc.body;
          next.setAttribute("data-nx-live", state.path);
          next.setAttribute("data-nx-live-state", "connected");
        } else {
          next = root.cloneNode(false);
          next.innerHTML = html;
        }
        morph(root, next);
        ownFields(root);
        walk(root, []);
        reapply(root);
        sweepIslands();
        // A successful submit resets its form to the server-rendered values.
        if (submitted && !msg.invalid && submitted.isConnected) {
          submitted.reset();
          ownFields(submitted);
        }
        var moved = msg.patch || msg.nav;
        if (moved && push && moved !== location.pathname + location.search) history.pushState({ nx: true }, "", moved);
        if (msg.nav && push) window.scrollTo(0, 0);
        if (moved) navigated();
        runPushed(root, msg.push);
      };
      ws.onclose = function () {
        // Replies to what this socket carried won't come: nothing stays busy.
        Object.keys(state.submits).forEach(function (ref) { state.submits[ref].forEach(unmarkBusy); });
        state.submits = {};
        if (state.closed) return; // navigated away: stay closed
        root.setAttribute("data-nx-live-state", "disconnected");
        // Back off with jitter, so a restarted server is not hit by every
        // page at once.
        state.timer = setTimeout(open, state.delay * (0.75 + Math.random() / 2));
        state.delay = Math.min(state.delay * 2, 10000);
      };
    }
    state.reconnectNow = function () {
      if (state.closed || !state.timer) return;
      state.delay = 500;
      open();
    };
    open();
  }

  // Reconnect at once when the network returns or the tab is looked at
  // again, instead of waiting out the backoff.
  function reconnectAll() {
    liveRoots.forEach(function (state) { if (state.reconnectNow) state.reconnectNow(); });
  }

  // syncLive matches sockets to the page: roots that left the document, or
  // now name another live page, close; new roots connect.
  function syncLive() {
    liveRoots.forEach(function (state, root) {
      if (!root.isConnected || root.getAttribute("data-nx-live") !== state.path) {
        state.closed = true;
        if (state.ws) state.ws.close();
        liveRoots.delete(root);
      }
    });
    Array.from(document.querySelectorAll("[data-nx-live]")).forEach(function (root) {
      if (!liveRoots.has(root)) connectLive(root);
    });
  }

  // ---- islands (view.NewIsland) --------------------------------------------

  // An island is a component of the Vite frontend mounted into an
  // <nx-island>: data-c names it, data-l is the loader module (which maps
  // names to lazily imported adapters), data-p its props as JSON, data-when
  // when to mount, data-ssr that the server rendered its HTML. An adapter
  // exports mount(el, props, ctx), returning { update(props), unmount() }.
  // Signals in the props stay live: the island gets their values, is
  // updated when one changes, and sets a top-level one with ctx.set.
  var islands = new Map(); // element -> state
  var loaders = new Map(); // loader URL -> its module (a promise)

  function island(el) {
    var props = el.getAttribute("data-p") || "{}";
    var st = islands.get(el);
    if (st) {
      if (st.props !== props) {
        st.props = props;
        st.live = hydrate(JSON.parse(props));
        if (st.handle) watchIsland(el, st, true);
      }
      return;
    }
    st = { name: el.getAttribute("data-c"), props: props, live: hydrate(JSON.parse(props)), handle: null, effects: [], stop: null, gone: false };
    islands.set(el, st);
    if (!el.hasAttribute("data-l")) {
      console.error("nexus view: island " + st.name + " cannot load: " + el.getAttribute("data-error"));
      return;
    }
    st.stop = when(el, el.getAttribute("data-when") || "load", function () {
      st.stop = null;
      loadIsland(el, st);
    });
  }

  // when calls go once the island's strategy allows, and returns a function
  // that cancels the wait.
  function when(el, strategy, go) {
    if (strategy === "idle") {
      var idle = root.requestIdleCallback || function (f) { return setTimeout(f, 200); };
      var cancelIdle = root.cancelIdleCallback || clearTimeout;
      var id = idle(go);
      return function () { cancelIdle(id); };
    }
    if (strategy === "visible" && root.IntersectionObserver) {
      var io = new IntersectionObserver(function (entries) {
        if (entries.some(function (e) { return e.isIntersecting; })) {
          io.disconnect();
          go();
        }
      }, { rootMargin: "200px" });
      io.observe(el);
      return function () { io.disconnect(); };
    }
    if (strategy.indexOf("media:") === 0 && root.matchMedia) {
      var mq = root.matchMedia(strategy.slice(6));
      if (!mq.matches) {
        var onChange = function () {
          if (!mq.matches) return;
          mq.removeEventListener("change", onChange);
          go();
        };
        mq.addEventListener("change", onChange);
        return function () { mq.removeEventListener("change", onChange); };
      }
    }
    go();
    return null;
  }

  // plain is v with every signal replaced by its current value.
  function plain(v) {
    if (Array.isArray(v)) return v.map(plain);
    if (v && typeof v === "object") {
      if (typeof v.$sig === "string" && typeof v.peek === "function") return v.peek();
      var o = {};
      for (var k in v) o[k] = plain(v[k]);
      return o;
    }
    return v;
  }

  function signalProps(live) {
    return Object.keys(live).filter(function (k) {
      return live[k] && typeof live[k].$sig === "string" && typeof live[k].set === "function";
    });
  }

  function islandContext(el, st) {
    return {
      hydrate: el.hasAttribute("data-ssr"),
      signals: signalProps(st.live),
      set: function (name, v) {
        var h = st.live && st.live[name];
        if (h && typeof h.set === "function") h.set(v);
        else console.warn("nexus view: island " + st.name + ": " + name + " is not a signal prop");
      },
    };
  }

  function loadIsland(el, st) {
    var url = el.getAttribute("data-l");
    var loader = loaders.get(url);
    if (!loader) {
      loader = importer().then(function (load) { return load(url); });
      loaders.set(url, loader);
    }
    loader
      .then(function (m) {
        var map = (m && m.islands) || root.__nxIslands || {};
        var load = map[st.name];
        if (!load) throw new Error("no island named " + st.name + " in src/islands");
        return load();
      })
      .then(function (m) {
        if (st.gone) return;
        st.handle = m.mount(el, plain(st.live), islandContext(el, st)) || {};
        el.removeAttribute("data-ssr"); // hydrated once; a remount renders afresh
        el.setAttribute("data-nx-island", "mounted");
        watchIsland(el, st, false);
      })
      .catch(function (err) {
        el.setAttribute("data-nx-island", "failed");
        console.error("nexus view: island " + st.name + ":", err);
      });
  }

  // watchIsland (re)starts the effect that hands the island new values when
  // a signal in its props changes; now also updates it straight away (new
  // props from the server).
  function watchIsland(el, st, now) {
    st.effects.forEach(dispose);
    st.effects = [];
    var first = !now;
    effect(function () {
      track(st.live);
      if (first) {
        first = false;
        return;
      }
      updateIsland(el, st);
    }, st.effects);
  }

  // importer resolves to import() once /_view/import.js has run.
  function importer() {
    if (root.__nxImport) return Promise.resolve(root.__nxImport);
    return new Promise(function (resolve, reject) {
      var t = setTimeout(function () { reject(new Error("/_view/import.js did not load: is view.Script() in the page head?")); }, 10000);
      root.addEventListener("nx:import", function () {
        clearTimeout(t);
        resolve(root.__nxImport);
      }, { once: true });
    });
  }

  // updateIsland hands an island its current props; a module without
  // update is mounted again.
  function updateIsland(el, st) {
    if (!st.handle) return;
    if (st.handle.update) {
      st.handle.update(plain(st.live));
      return;
    }
    if (st.handle.unmount) st.handle.unmount();
    st.handle = null;
    st.effects.forEach(dispose);
    st.effects = [];
    loadIsland(el, st);
  }

  function unmountIsland(el) {
    var st = islands.get(el);
    if (!st) return;
    islands.delete(el);
    st.gone = true;
    if (st.stop) st.stop();
    st.effects.forEach(dispose);
    if (st.handle && st.handle.unmount) st.handle.unmount();
    el.removeAttribute("data-nx-island");
  }

  // sweepIslands unmounts islands that left the page.
  function sweepIslands() {
    islands.forEach(function (st, el) {
      if (!el.isConnected) unmountIsland(el);
    });
  }

  // ---- in-app navigation (view.Link) ---------------------------------------

  // navigate replaces the page with the one at url without reloading: the
  // body is patched in place, stylesheets and scripts the new head needs are
  // added, and live sockets follow. Anything unexpected falls back to a
  // normal page load, including a page that isn't a view page (one a
  // redirect led to, or another app's shell), which only a real load renders
  // as its server meant it.
  function navigate(url, push) {
    var target = url;
    return fetch(url, { headers: { Accept: "text/html" }, credentials: "same-origin" })
      .then(function (res) {
        var type = res.headers.get("Content-Type") || "";
        if (res.url) target = res.url;
        if (!res.ok || type.indexOf("text/html") !== 0) throw new Error("not a page");
        return res.text().then(function (text) { return { text: text, url: target }; });
      })
      .then(function (page) {
        var doc = new DOMParser().parseFromString(page.text, "text/html");
        if (!doc.querySelector('script[src*="/_view/runtime.js"]')) throw new Error("not a view page");
        mergeHead(doc.head);
        morph(document.body, doc.body);
        ownFields(document.body);
        if (push) history.pushState({ nx: true }, "", page.url);
        walk(document.body, []);
        reapply(document.body);
        syncLive();
        sweepIslands();
        if (push) window.scrollTo(0, 0);
        navigated();
      })
      .catch(function () {
        if (push) location.assign(target);
        else location.replace(target);
      });
  }

  function mergeHead(head) {
    if (head.querySelector("title")) document.title = head.querySelector("title").textContent;
    Array.from(head.querySelectorAll('link[rel="stylesheet"][href]')).forEach(function (l) {
      if (!document.head.querySelector('link[rel="stylesheet"][href="' + l.getAttribute("href") + '"]')) {
        document.head.appendChild(document.importNode(l, true));
      }
    });
    Array.from(head.querySelectorAll("script[src]")).forEach(function (sc) {
      var src = sc.getAttribute("src");
      if (document.head.querySelector('script[src="' + src + '"]')) return;
      var el = document.createElement("script"); // an imported script would not run
      Array.from(sc.attributes).forEach(function (a) { el.setAttribute(a.name, a.value); });
      document.head.appendChild(el);
    });
  }

  // navigated tells the page's own scripts it moved (a menu marking the
  // current page): an nx:navigate event on window.
  function navigated() {
    window.dispatchEvent(new Event("nx:navigate"));
  }

  // liveNavigate moves to url over the socket of the page's live root, when
  // there is one and it is connected: the server patches the page or opens
  // the other live page on the connection (navigate.go). It reports whether
  // it did.
  function liveNavigate(url, push) {
    var root = document.body && document.body.hasAttribute("data-nx-live") ? document.body : null;
    var state = root && liveRoots.get(root);
    if (!state || !state.ws || state.ws.readyState !== 1) return false;
    var u = new URL(url, location.href);
    if (u.origin !== location.origin) return false;
    state.navPush = push;
    liveSend({ root: root, state: state }, { event: "__nav", url: u.pathname + u.search });
    return true;
  }

  // ---- view.ConfirmLeave ---------------------------------------------------

  // unsentForms have changes typed since the server last answered.
  var unsentForms = new Set();
  var leaveConfirmed = false;

  // leaveQuestion is what to ask before leaving the page: the question of a
  // ConfirmLeave form with changes, "" when there is none.
  function leaveQuestion() {
    if (typeof document === "undefined") return "";
    var forms = document.querySelectorAll("form[data-nx-confirm-leave]");
    for (var i = 0; i < forms.length; i++) {
      var f = forms[i];
      if (f.hasAttribute("data-nx-dirty") || unsentForms.has(f)) return f.getAttribute("data-nx-confirm-leave");
    }
    return "";
  }

  // mayLeave asks, when there are changes to lose, whether to leave.
  function mayLeave() {
    var q = leaveQuestion();
    if (!q) return true;
    var ok = typeof root.confirm === "function" ? root.confirm(q) : true;
    if (ok) leaveConfirmed = true;
    return ok;
  }

  if (typeof window !== "undefined" && window.addEventListener) {
    window.addEventListener("beforeunload", function (e) {
      if (leaveConfirmed || !leaveQuestion()) return;
      e.preventDefault();
      e.returnValue = "";
    });
  }

  function onNavClick(e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest && e.target.closest("a[data-nx-nav]");
    if (!a || (a.target && a.target !== "_self") || a.hasAttribute("download")) return;
    var url = new URL(a.href, location.href);
    if (url.origin !== location.origin) return;
    if (url.pathname === location.pathname && url.search === location.search && url.hash) return; // same page anchor
    e.preventDefault();
    if (!mayLeave()) return;
    if (!liveNavigate(url.href, true)) navigate(url.href, true);
  }

  function liveState(el, event) {
    var root = el.closest("[data-nx-live]");
    var state = root && liveRoots.get(root);
    if (!state) {
      console.warn("nexus view: " + event + " is not inside a live page");
      return null;
    }
    return { root: root, state: state };
  }

  // liveSend sends an event, or queues it while the socket is down.
  function liveSend(live, msg) {
    msg.ref = ++live.state.ref;
    live.root.setAttribute("aria-busy", "true");
    var ws = live.state.ws;
    if (ws && ws.readyState === 1) ws.send(JSON.stringify(msg));
    else if (live.state.queue.length < 50) live.state.queue.push(msg);
    return msg.ref;
  }

  // formFields collects a form's fields: name -> every value.
  function formFields(form) {
    var out = {};
    new FormData(form).forEach(function (v, k) {
      if (typeof v !== "string") return; // files are not sent over the socket
      (out[k] = out[k] || []).push(v);
    });
    return out;
  }

  var changeTimers = new WeakMap();

  // A view.Form's fields the user changed, and those of them they left: a
  // field's error shows once it is touched.
  var formTouches = new WeakMap();
  function formTouch(form) {
    var t = formTouches.get(form);
    if (!t) formTouches.set(form, (t = { edited: new Set(), touched: new Set() }));
    return t;
  }
  // fieldNames are the form's controls, present or not in its values - an
  // unticked checkbox sends nothing, but its name says it was shown.
  function fieldNames(form) {
    var names = new Set();
    Array.prototype.forEach.call(form.elements, function (el) {
      if (el.name) names.add(el.name);
    });
    return Array.from(names);
  }
  function sendForm(form) {
    clearTimeout(changeTimers.get(form));
    var live = liveState(form, "__form");
    if (!live) return;
    var msg = {
      event: "__form",
      formName: form.getAttribute("data-nx-form"),
      form: formFields(form),
      touched: Array.from(formTouch(form).touched),
      fields: fieldNames(form),
    };
    var then = form.getAttribute("data-nx-then");
    if (then) msg.then = then;
    liveSend(live, target(msg, form, form.getAttribute("data-nx-form-ct") || undefined));
  }
  if (typeof document !== "undefined") {
    document.addEventListener("focusout", function (e) {
      var el = e.target, form = el && el.form;
      if (!form || !el.name || !form.hasAttribute("data-nx-form")) return;
      var t = formTouch(form);
      if (!t.edited.has(el.name) || t.touched.has(el.name)) return;
      t.touched.add(el.name);
      // Leaving a field the user changed shows its error now, not on the
      // next keystroke - when the form is live at all.
      if (form.hasAttribute("data-nx-then") || /__nx\.live\.form/.test(form.getAttribute("oninput") || "")) sendForm(form);
    }, true);
  }

  // prime asks, after a short pause, for the tree of the page the browser
  // already shows (__resync answered without patching).
  function prime(state, ws) {
    setTimeout(function () {
      if (state.tree || state.ws !== ws || ws.readyState !== 1) return;
      state.priming = ++state.ref;
      ws.send(JSON.stringify({ ref: state.priming, event: "__resync" }));
    }, 300);
  }

  // recover sends, on a reconnect, each form that validates as the user
  // types (view.Change) to its event with what it holds - before anything
  // queued - so a page that mounted afresh has the form back.
  function recover(state, root, ws) {
    Array.from(root.querySelectorAll("form")).forEach(function (form) {
      var change = changeEvents.get(form);
      if (!change) return;
      var ref = ++state.ref;
      state.recovering.add(ref);
      ws.send(JSON.stringify(target({ ref: ref, event: change.event, form: formFields(form) }, form, change.comp)));
    });
  }

  // target addresses an event of a live component's method (comp, its
  // type) to the instance el is inside: the nearest component of the type.
  function target(msg, el, comp) {
    if (!comp) return msg;
    var c = el.closest("[data-nx-ct]");
    while (c && c.getAttribute("data-nx-ct") !== comp) c = c.parentElement && c.parentElement.closest("[data-nx-ct]");
    if (c) msg.c = comp + "#" + c.getAttribute("data-nx-c");
    return msg;
  }

  // ---- uploads (view.Upload) ----------------------------------------------

  var uploadFiles = {}; // ref -> the File chosen, until its request starts
  var uploadXHRs = {}; // ref -> its request, while it runs
  var uploadSeq = 0;

  // onUploadChange offers the files chosen in an upload input to the page;
  // the server answers with where to send the ones it accepts.
  function onUploadChange(e) {
    var input = e.target;
    if (!input || !input.getAttribute || !input.hasAttribute("data-nx-upload") || !input.files) return;
    var name = input.getAttribute("data-nx-upload");
    var live = liveState(input, name);
    if (!live) return;
    var files = [];
    Array.from(input.files).forEach(function (f) {
      var ref = "u" + Date.now().toString(36) + (++uploadSeq);
      uploadFiles[ref] = f;
      files.push({ ref: ref, name: f.name, size: f.size, type: f.type || "" });
    });
    input.value = "";
    if (files.length) liveSend(live, target({ event: "__upload", upload: { name: name, files: files } }, input, input.getAttribute("data-nx-upload-ct")));
  }

  // startUploads sends each accepted file to its URL; the server follows
  // the bytes and the page renders the progress.
  function startUploads(urls) {
    Object.keys(urls).forEach(function (ref) {
      var file = uploadFiles[ref];
      delete uploadFiles[ref];
      if (!file) return;
      var xhr = new XMLHttpRequest();
      uploadXHRs[ref] = xhr;
      xhr.open("POST", urls[ref]);
      var h = csrfHeaders({ "Content-Type": "application/octet-stream" });
      Object.keys(h).forEach(function (k) { xhr.setRequestHeader(k, h[k]); });
      xhr.onloadend = function () { delete uploadXHRs[ref]; };
      xhr.send(file);
    });
  }

  // readThis fills in the arguments view.This() rendered as {"$nx": read}
  // markers with what el holds now.
  function readThis(el, args) {
    if (!Array.isArray(args)) return args;
    return args.map(function (a) {
      if (!a || typeof a !== "object" || Array.isArray(a) || typeof a.$nx !== "string") return a;
      switch (a.$nx) {
        case "value": return el && el.value != null ? String(el.value) : "";
        case "checked": return !!(el && el.checked);
        case "attr": return el && el.getAttribute ? el.getAttribute(a.name) : null;
      }
      return a;
    });
  }

  nx.live = {
    // cancelUpload is what view.CancelUpload renders: stop an entry's
    // request and have the page drop it.
    cancelUpload: function (el, name, ref, comp) {
      var xhr = uploadXHRs[ref];
      if (xhr) xhr.abort();
      delete uploadXHRs[ref];
      delete uploadFiles[ref];
      var live = liveState(el, name);
      if (live) liveSend(live, target({ event: "__cancel_upload", upload: { name: name, ref: ref } }, el, comp));
    },
    // send is what view.Send renders into an on* attribute; comp names the
    // live component type the method belongs to.
    send: function (el, event, args, comp) {
      var busy = el && el.hasAttribute && el.hasAttribute("data-nx-busy");
      if (busy && el.getAttribute("aria-busy") === "true") return; // sent already: the reply is on its way
      var live = liveState(el, event);
      if (!live) return;
      var ref = liveSend(live, target({ event: event, args: readThis(el, args) }, el, comp));
      if (busy) markBusy(live.state, ref, el);
    },
    // submit is what view.Submit renders into a form's onsubmit.
    submit: function (e, form, event, comp) {
      if (e) e.preventDefault();
      if (form.hasAttribute("aria-busy")) return; // sent already: the reply is on its way
      var live = liveState(form, event);
      if (!live) return;
      var msg = { event: event, form: formFields(form) };
      var name = form.getAttribute("data-nx-form");
      if (name !== null) {
        msg.formName = name;
        msg.touched = Array.from(formTouch(form).touched);
        msg.fields = fieldNames(form);
      }
      var ref = liveSend(live, target(msg, form, comp));
      markBusy(live.state, ref, form);
      // A data-nx-busy button that submitted it waits for the reply too.
      var by = e && e.submitter;
      if (by && by.hasAttribute("data-nx-busy")) markBusy(live.state, ref, by);
    },
    // form is what a live view.Form renders into its oninput/onchange: the
    // fields go to the form, checked as the user types, after typing pauses.
    form: function (e, form) {
      if (e && e.target && e.target.name) formTouch(form).edited.add(e.target.name);
      unsentForms.add(form);
      leaveConfirmed = false; // new changes: ask again
      clearTimeout(changeTimers.get(form));
      changeTimers.set(form, setTimeout(function () { sendForm(form); }, 150));
    },
    // change is what view.Change renders into a form's oninput/onchange:
    // the fields go to the server after typing pauses.
    change: function (e, form, event, comp) {
      changeEvents.set(form, { event: event, comp: comp });
      clearTimeout(changeTimers.get(form));
      changeTimers.set(form, setTimeout(function () {
        var live = liveState(form, event);
        if (live) liveSend(live, target({ event: event, form: formFields(form) }, form, comp));
      }, 150));
    },
  };

  // ---- CSRF ---------------------------------------------------------------

  // csrfToken is the XSRF-TOKEN cookie nexus's CSRF middleware keeps
  // readable for scripts; "" when CSRF is off.
  function csrfToken() {
    var m = /(?:^|;\s*)XSRF-TOKEN=([^;]*)/.exec(document.cookie || "");
    return m ? decodeURIComponent(m[1]) : "";
  }

  function csrfHeaders(h) {
    var t = csrfToken();
    if (t) h["X-XSRF-TOKEN"] = t;
    return h;
  }

  // onFormSubmit gives a plain same-origin POST form the token as its
  // csrf_token field, unless it carries one already.
  function onFormSubmit(e) {
    var form = e.target;
    if (!form || form.tagName !== "FORM") return;
    var by = e.submitter;
    var method = (by && by.getAttribute("formmethod")) || form.getAttribute("method") || "get";
    if (method.toLowerCase() !== "post") return;
    var t = csrfToken();
    if (!t || form.querySelector('input[name="csrf_token"]')) return;
    var action = (by && by.getAttribute("formaction")) || form.getAttribute("action") || location.href;
    if (new URL(action, location.href).origin !== location.origin) return;
    var f = document.createElement("input");
    f.type = "hidden";
    f.name = "csrf_token";
    f.value = t;
    form.appendChild(f);
  }

  nx.signal = signal;
  root.__nx = nx;

  if (typeof document !== "undefined") {
    var boot = function () {
      walk(document.documentElement, []);
      ownFields(document.documentElement);
      syncLive();
      document.addEventListener("click", onNavClick);
      document.addEventListener("submit", onFormSubmit, true);
      document.addEventListener("change", onUploadChange, true);
      window.addEventListener("online", reconnectAll);
      document.addEventListener("visibilitychange", function () {
        if (document.visibilityState === "visible") reconnectAll();
      });
      // Only entries this runtime made are patched in place; the browser
      // restores every other entry itself.
      var mark = function () {
        if (!history.state || !history.state.nx) history.replaceState(Object.assign({}, history.state, { nx: true }), "");
      };
      mark();
      window.addEventListener("hashchange", mark);
      window.addEventListener("popstate", function (e) {
        if (e.state && e.state.nx && !liveNavigate(location.href, false)) navigate(location.href, false);
      });
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
    else boot();
  }
})();
