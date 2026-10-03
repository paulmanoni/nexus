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
  function morph(el, next) {
    // data-nx-ignore: the element is the browser's while its id holds (a
    // chart a script drew into, say); a new id replaces it.
    if (el.hasAttribute("data-nx-ignore") && el.id && el.id === next.id) return;
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
      syncAttributes(el, next);
      morphChildren(el, next);
      return;
    }
    // A form field. What the user sees is kept while the field has focus;
    // otherwise the field takes the server's value when that value changed
    // since the last render - or on every render, for a field marked
    // data-nx-value (view.Value).
    var focused = typeof document !== "undefined" && el === document.activeElement;
    var seen = fieldState(el);
    var before = serverValue(el);
    syncAttributes(el, next);
    if (el.tagName === "TEXTAREA") {
      if (el.textContent !== next.textContent) el.textContent = next.textContent;
    } else {
      morphChildren(el, next);
    }
    if (focused) restoreField(el, seen);
    else if (el.hasAttribute("data-nx-value") || serverValue(el) !== before) takeServerValue(el);
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
      if (!next.hasAttribute(a.name)) el.removeAttribute(a.name);
    });
    Array.from(next.attributes).forEach(function (a) {
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    });
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
        var byId = Array.from(el.children).find(function (c) { return c.id === n.id; });
        if (byId) match = byId;
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

  // live connects each live root to its socket: the server sends the
  // rendered page after mounting and after every event; the browser patches
  // it in place. A dropped connection reconnects, and the server mounts
  // again.
  var liveRoots = new Map(); // root element -> { ws }

  // The page's first render came over HTTP: the first connection names it
  // (data-nx-live-join) so the server sends nothing it already has. A
  // reconnect gets a fresh mount and its full render. Events sent while
  // disconnected wait in a queue and go out once the socket is back.
  function connectLive(root) {
    var path = root.getAttribute("data-nx-live");
    var join = root.getAttribute("data-nx-live-join");
    var state = { ws: null, delay: 500, timer: null, ref: 0, submits: {}, queue: [], path: path, closed: false };
    liveRoots.set(root, state);
    function open() {
      clearTimeout(state.timer);
      state.timer = null;
      var url = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + path;
      if (join) url += "?join=" + encodeURIComponent(join);
      join = null; // a join is good for the first connection only
      var ws = new WebSocket(url);
      state.ws = ws;
      ws.onopen = function () {
        state.delay = 500;
        root.setAttribute("data-nx-live-state", "connected");
        var queued = state.queue;
        state.queue = [];
        queued.forEach(function (msg) { ws.send(JSON.stringify(msg)); });
      };
      ws.onmessage = function (m) {
        var msg = JSON.parse(m.data);
        root.removeAttribute("aria-busy");
        var submitted = msg.ref ? state.submits[msg.ref] : null;
        if (msg.ref) delete state.submits[msg.ref];
        if (submitted) submitted.removeAttribute("aria-busy");
        if (msg.error) {
          console.error("nexus view: live:", msg.error);
          root.setAttribute("data-nx-live-error", msg.error);
          return;
        }
        root.removeAttribute("data-nx-live-error");
        var html;
        if (msg.patch) {
          var tokens = applyPatch(state.last || [], msg.patch, state.dict);
          if (tokens.length !== msg.n || tokens.indexOf(undefined) >= 0) {
            // Out of step with the server: ask for the whole render.
            ws.send(JSON.stringify({ event: "__resync" }));
            return;
          }
          state.last = tokens;
          observe(state.dict, tokens);
          html = tokens.join("");
        } else if (typeof msg.html === "string") {
          state.last = tokenize(msg.html);
          state.dict = newDict();
          observe(state.dict, state.last);
          html = msg.html;
        } else {
          return;
        }
        var next;
        if (root.tagName === "BODY") {
          next = new DOMParser().parseFromString(html, "text/html").body;
          next.setAttribute("data-nx-live", path);
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
      };
      ws.onclose = function () {
        if (state.closed) return; // navigated away: stay closed
        root.setAttribute("data-nx-live-state", "disconnected");
        state.last = null; // the reconnect sends a full render
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

  function onNavClick(e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest && e.target.closest("a[data-nx-nav]");
    if (!a || (a.target && a.target !== "_self") || a.hasAttribute("download")) return;
    var url = new URL(a.href, location.href);
    if (url.origin !== location.origin) return;
    if (url.pathname === location.pathname && url.search === location.search && url.hash) return; // same page anchor
    e.preventDefault();
    navigate(url.href, true);
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

  nx.live = {
    // send is what view.Send renders into an on* attribute.
    send: function (el, event, args) {
      var live = liveState(el, event);
      if (live) liveSend(live, { event: event, args: args });
    },
    // submit is what view.Submit renders into a form's onsubmit.
    submit: function (e, form, event) {
      if (e) e.preventDefault();
      var live = liveState(form, event);
      if (!live) return;
      form.setAttribute("aria-busy", "true");
      var ref = liveSend(live, { event: event, form: formFields(form) });
      live.state.submits[ref] = form;
    },
    // change is what view.Change renders into a form's oninput/onchange:
    // the fields go to the server after typing pauses.
    change: function (e, form, event) {
      clearTimeout(changeTimers.get(form));
      changeTimers.set(form, setTimeout(function () {
        var live = liveState(form, event);
        if (live) liveSend(live, { event: event, form: formFields(form) });
      }, 150));
    },
  };

  nx.signal = signal;
  root.__nx = nx;

  if (typeof document !== "undefined") {
    var boot = function () {
      walk(document.documentElement, []);
      ownFields(document.documentElement);
      syncLive();
      document.addEventListener("click", onNavClick);
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
        if (e.state && e.state.nx) navigate(location.href, false);
      });
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
    else boot();
  }
})();
