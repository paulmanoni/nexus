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

  // ---- live pages ---------------------------------------------------------

  // morph patches el to match next in place: attributes and children are
  // updated, matching elements by id or else by position and tag, so focus,
  // selection and the value being typed survive a live update.
  function morph(el, next) {
    syncAttributes(el, next);
    morphChildren(el, next);
  }

  // A form field's current value belongs to the user until the server
  // says otherwise: it is overwritten only when the server's value (or
  // checked) attribute itself changed, and never while the field has focus.
  function syncAttributes(el, next) {
    var field = el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT";
    var oldValue = el.getAttribute("value"), oldChecked = el.hasAttribute("checked");
    Array.from(el.attributes).forEach(function (a) {
      if (!next.hasAttribute(a.name)) el.removeAttribute(a.name);
    });
    Array.from(next.attributes).forEach(function (a) {
      if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    });
    if (!field || (typeof document !== "undefined" && el === document.activeElement)) return;
    var newValue = next.getAttribute("value");
    if (el.tagName !== "SELECT" && newValue !== oldValue) el.value = newValue == null ? "" : newValue;
    if ((el.type === "checkbox" || el.type === "radio") && next.hasAttribute("checked") !== oldChecked) {
      el.checked = next.hasAttribute("checked");
    }
  }

  function sameKind(a, b) {
    if (a.nodeType !== b.nodeType) return false;
    if (a.nodeType !== 1) return true;
    if (a.tagName !== b.tagName) return false;
    return !a.id || !b.id || a.id === b.id;
  }

  function morphChildren(el, next) {
    if (el.tagName === "TEXTAREA") return; // its text is its value
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
  // tokens after every '>' exactly as the server cuts them (diff.go):
  // n copies n old tokens, -n skips n, a string is inserted.
  function tokenize(html) {
    var out = [];
    while (html.length) {
      var i = html.indexOf(">");
      if (i < 0) {
        out.push(html);
        break;
      }
      out.push(html.slice(0, i + 1));
      html = html.slice(i + 1);
    }
    return out;
  }

  function applyPatch(old, patch) {
    var out = [];
    var i = 0;
    patch.forEach(function (step) {
      if (typeof step === "string") {
        tokenize(step).forEach(function (t) { out.push(t); });
      } else if (step > 0) {
        for (var n = 0; n < step; n++) out.push(old[i++]);
      } else {
        i -= step;
      }
    });
    return out;
  }

  nx._applyPatch = function (oldHTML, patchJSON) {
    return applyPatch(tokenize(oldHTML), JSON.parse(patchJSON)).join("");
  };

  // live connects each live root to its socket: the server sends the
  // rendered page after mounting and after every event; the browser patches
  // it in place. A dropped connection reconnects, and the server mounts
  // again.
  var liveRoots = new Map(); // root element -> { ws }

  function connectLive(root) {
    var path = root.getAttribute("data-nx-live");
    var state = { ws: null, delay: 500, ref: 0, submits: {}, path: path, closed: false };
    liveRoots.set(root, state);
    function open() {
      var ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + path);
      state.ws = ws;
      ws.onopen = function () {
        state.delay = 500;
        root.setAttribute("data-nx-live-state", "connected");
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
          var tokens = applyPatch(state.last || [], msg.patch);
          if (tokens.length !== msg.n) {
            // Out of step with the server: ask for the whole render.
            ws.send(JSON.stringify({ event: "__resync" }));
            return;
          }
          state.last = tokens;
          html = tokens.join("");
        } else if (typeof msg.html === "string") {
          state.last = tokenize(msg.html);
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
        walk(root, []);
        reapply(root);
        // A successful submit resets its form to the server-rendered values.
        if (submitted && !msg.invalid && submitted.isConnected) submitted.reset();
      };
      ws.onclose = function () {
        if (state.closed) return; // navigated away: stay closed
        root.setAttribute("data-nx-live-state", "disconnected");
        setTimeout(open, state.delay);
        state.delay = Math.min(state.delay * 2, 10000);
      };
    }
    open();
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

  // ---- in-app navigation (view.Link) ---------------------------------------

  // navigate replaces the page with the one at url without reloading: the
  // body is patched in place, stylesheets and scripts the new head needs are
  // added, and live sockets follow. Anything unexpected falls back to a
  // normal page load.
  function navigate(url, push) {
    return fetch(url, { headers: { Accept: "text/html" }, credentials: "same-origin" })
      .then(function (res) {
        var type = res.headers.get("Content-Type") || "";
        if (!res.ok || type.indexOf("text/html") !== 0) throw new Error("not a page");
        return res.text().then(function (text) { return { text: text, url: res.url || url }; });
      })
      .then(function (page) {
        var doc = new DOMParser().parseFromString(page.text, "text/html");
        mergeHead(doc.head);
        morph(document.body, doc.body);
        if (push) history.pushState({ nx: true }, "", page.url);
        walk(document.body, []);
        reapply(document.body);
        syncLive();
        if (push) window.scrollTo(0, 0);
      })
      .catch(function () {
        location.href = url;
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
    if (!state || !state.ws || state.ws.readyState !== 1) {
      console.warn("nexus view: live page not connected yet; " + event + " dropped");
      return null;
    }
    return { root: root, state: state };
  }

  function liveSend(live, msg) {
    msg.ref = ++live.state.ref;
    live.root.setAttribute("aria-busy", "true");
    live.state.ws.send(JSON.stringify(msg));
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
      syncLive();
      document.addEventListener("click", onNavClick);
      window.addEventListener("popstate", function () { navigate(location.href, false); });
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
    else boot();
  }
})();
