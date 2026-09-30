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

  function wire(el, owner) {
    Array.from(el.attributes).forEach(function (a) {
      var spec, caps, fn;
      if (a.name.indexOf("data-nx-bind-") === 0) {
        var name = a.name.slice("data-nx-bind-".length);
        spec = JSON.parse(a.value);
        caps = hydrate(spec.caps || {});
        fn = twin(spec.fn);
        effect(function () { apply(el, name, fn(caps)); }, owner);
      } else if (a.name.indexOf("data-nx-on-") === 0) {
        var event = a.name.slice("data-nx-on-".length);
        spec = JSON.parse(a.value);
        caps = hydrate(spec.caps || {});
        fn = twin(spec.fn);
        el.addEventListener(event, function (e) {
          try {
            fn(caps, e);
          } catch (err) {
            console.error("nexus view:", err);
          }
        });
      }
    });
  }

  function walk(el, owner) {
    if (el.nodeType !== 1) return;
    if (el.tagName === "NX-SHARD") {
      shard(el, owner);
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
        headers: { "Content-Type": "application/json", Accept: "text/html" },
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
          mount();
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

  nx.signal = signal;
  root.__nx = nx;

  if (typeof document !== "undefined") {
    var boot = function () { walk(document.documentElement, []); };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
    else boot();
  }
})();
