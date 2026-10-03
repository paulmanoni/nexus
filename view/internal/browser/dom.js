// A small DOM for running the view runtime in goja: documents, elements,
// text, events with bubbling and inline on* handlers, form fields with the
// browser's value/checked/selected semantics (dirty flags, form reset,
// select normalisation), CSS selectors, timers on a virtual clock, fetch,
// WebSocket, URL, FormData, DOMParser, location and history. HTML is parsed
// by the Go side (golang.org/x/net/html) and handed over as nested arrays.
//
// It is as small as the runtime and the test driver need, not a browser:
// no layout, no CSS, no scripts other than the ones the host runs.
(function (G) {
  "use strict";
  var go = G.__go;

  // ---- events ------------------------------------------------------------

  class Event {
    constructor(type, init) {
      init = init || {};
      this.type = type;
      this.bubbles = !!init.bubbles;
      this.cancelable = !!init.cancelable;
      this.defaultPrevented = false;
      this.target = null;
      this.currentTarget = null;
      this._stop = false;
      this._stopNow = false;
      this.isTrusted = true;
    }
    preventDefault() { if (this.cancelable) this.defaultPrevented = true; }
    stopPropagation() { this._stop = true; }
    stopImmediatePropagation() { this._stop = true; this._stopNow = true; }
  }
  class MouseEvent extends Event {
    constructor(type, init) {
      super(type, init);
      init = init || {};
      this.button = init.button || 0;
      this.metaKey = !!init.metaKey;
      this.ctrlKey = !!init.ctrlKey;
      this.shiftKey = !!init.shiftKey;
      this.altKey = !!init.altKey;
    }
  }
  class KeyboardEvent extends Event {
    constructor(type, init) {
      super(type, init);
      this.key = (init && init.key) || "";
    }
  }
  class CustomEvent extends Event {
    constructor(type, init) {
      super(type, init);
      this.detail = init && init.detail !== undefined ? init.detail : null;
    }
  }
  class SubmitEvent extends Event {
    constructor(type, init) {
      super(type, init);
      this.submitter = (init && init.submitter) || null;
    }
  }

  var inlineHandlers = new Map();
  function inlineHandler(code) {
    var f = inlineHandlers.get(code);
    if (!f) {
      f = new Function("event", code);
      inlineHandlers.set(code, f);
    }
    return f;
  }

  function report(err) {
    G.console.error("uncaught:", err && err.stack ? err.stack : String(err));
  }

  class EventTarget {
    addEventListener(type, fn, opts) {
      if (!fn) return;
      var capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
      var once = !!(opts && typeof opts === "object" && opts.once);
      var ls = this._ls || (this._ls = {});
      var list = ls[type] || (ls[type] = []);
      if (list.some(function (l) { return l.fn === fn && l.capture === capture; })) return;
      list.push({ fn: fn, capture: capture, once: once });
    }
    removeEventListener(type, fn, opts) {
      var capture = typeof opts === "boolean" ? opts : !!(opts && opts.capture);
      var list = this._ls && this._ls[type];
      if (!list) return;
      this._ls[type] = list.filter(function (l) { return !(l.fn === fn && l.capture === capture); });
    }
    dispatchEvent(ev) {
      ev.target = this;
      var path = [];
      for (var n = this; n; n = n.parentNode) path.push(n);
      if (path[path.length - 1] === G.document) path.push(G);
      var i;
      for (i = path.length - 1; i > 0 && !ev._stop; i--) invoke(path[i], ev, "capture");
      if (!ev._stop) invoke(this, ev, "target");
      if (ev.bubbles) for (i = 1; i < path.length && !ev._stop; i++) invoke(path[i], ev, "bubble");
      ev.currentTarget = null;
      return !ev.defaultPrevented;
    }
  }

  function invoke(node, ev, phase) {
    ev.currentTarget = node;
    if (phase !== "capture" && node.nodeType === 1) {
      var code = node.getAttribute("on" + ev.type);
      if (code) {
        try {
          if (inlineHandler(code).call(node, ev) === false) ev.preventDefault();
        } catch (err) {
          report(err);
        }
      }
    }
    var list = node._ls && node._ls[ev.type];
    if (!list) return;
    list.slice().forEach(function (l) {
      if (ev._stopNow) return;
      if (phase === "capture" && !l.capture) return;
      if (phase === "bubble" && l.capture) return;
      if (l.once) node.removeEventListener(ev.type, l.fn, l.capture);
      try {
        if (typeof l.fn === "function") l.fn.call(node, ev);
        else l.fn.handleEvent(ev);
      } catch (err) {
        report(err);
      }
    });
  }

  // ---- nodes -------------------------------------------------------------

  class Node extends EventTarget {
    constructor(doc) {
      super();
      this.ownerDocument = doc;
      this.parentNode = null;
      this._kids = [];
    }
    get childNodes() { return this._kids.slice(); }
    get firstChild() { return this._kids[0] || null; }
    get lastChild() { return this._kids[this._kids.length - 1] || null; }
    get nextSibling() {
      var p = this.parentNode;
      return p ? p._kids[p._kids.indexOf(this) + 1] || null : null;
    }
    get previousSibling() {
      var p = this.parentNode;
      return p ? p._kids[p._kids.indexOf(this) - 1] || null : null;
    }
    get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
    get isConnected() {
      var n = this;
      while (n.parentNode) n = n.parentNode;
      return n.nodeType === 9;
    }
    hasChildNodes() { return this._kids.length > 0; }
    contains(n) {
      for (; n; n = n.parentNode) if (n === this) return true;
      return false;
    }
    getRootNode() {
      var n = this;
      while (n.parentNode) n = n.parentNode;
      return n;
    }
    insertBefore(node, ref) {
      if (node.nodeType === 11) {
        node._kids.slice().forEach(function (k) { this.insertBefore(k, ref); }, this);
        return node;
      }
      if (node.contains(this)) throw new Error("HierarchyRequestError: a node can't contain itself");
      if (node.parentNode) node.parentNode.removeChild(node);
      var i = ref ? this._kids.indexOf(ref) : -1;
      if (ref && i < 0) throw new Error("NotFoundError: the reference node is not a child");
      if (i < 0) this._kids.push(node);
      else this._kids.splice(i, 0, node);
      node.parentNode = this;
      inserted(node);
      return node;
    }
    appendChild(node) { return this.insertBefore(node, null); }
    append() {
      Array.from(arguments).forEach(function (n) {
        this.appendChild(typeof n === "string" ? new Text(this.ownerDocument, n) : n);
      }, this);
    }
    removeChild(node) {
      var i = this._kids.indexOf(node);
      if (i < 0) throw new Error("NotFoundError: not a child");
      this._kids.splice(i, 1);
      node.parentNode = null;
      removed(node, this);
      return node;
    }
    replaceChild(node, old) {
      this.insertBefore(node, old);
      return this.removeChild(old);
    }
    remove() { if (this.parentNode) this.parentNode.removeChild(this); }
    get textContent() {
      var s = "";
      this._kids.forEach(function (k) { if (k.nodeType !== 8) s += k.textContent; });
      return s;
    }
    set textContent(v) {
      this._kids.slice().forEach(function (k) { this.removeChild(k); }, this);
      v = v == null ? "" : String(v);
      if (v !== "") this.appendChild(new Text(this.ownerDocument, v));
    }
  }

  class CharacterData extends Node {
    constructor(doc, data) {
      super(doc);
      this.data = String(data);
    }
    get nodeValue() { return this.data; }
    set nodeValue(v) { this.data = String(v); }
    get textContent() { return this.data; }
    set textContent(v) { this.data = String(v); }
    get length() { return this.data.length; }
    cloneNode() { return new this.constructor(this.ownerDocument, this.data); }
  }
  class Text extends CharacterData {
    get nodeType() { return 3; }
    get nodeName() { return "#text"; }
  }
  class Comment extends CharacterData {
    get nodeType() { return 8; }
    get nodeName() { return "#comment"; }
  }

  class DocumentFragment extends Node {
    get nodeType() { return 11; }
    get nodeName() { return "#document-fragment"; }
    get children() { return this._kids.filter(isElement); }
    querySelector(s) { return query(this, s, true)[0] || null; }
    querySelectorAll(s) { return query(this, s, false); }
    cloneNode(deep) {
      var f = new DocumentFragment(this.ownerDocument);
      if (deep) this._kids.forEach(function (k) { f.appendChild(k.cloneNode(true)); });
      return f;
    }
  }

  function isElement(n) { return n.nodeType === 1; }

  var VOID = { area: 1, base: 1, br: 1, col: 1, embed: 1, hr: 1, img: 1, input: 1, link: 1, meta: 1, source: 1, track: 1, wbr: 1 };
  var FOCUSABLE = { INPUT: 1, SELECT: 1, TEXTAREA: 1, BUTTON: 1, A: 1 };

  class Element extends Node {
    constructor(doc, name, ns) {
      super(doc);
      this.namespaceURI = ns || "html";
      this.localName = this.namespaceURI === "html" ? String(name).toLowerCase() : String(name);
      this._attrs = [];
      // Form field state, as a browser keeps it: null means "not dirty"
      // (the field shows its default, read from the markup).
      this._value = null;
      this._checked = null;
      this._sel = false; // an option's selectedness
      this._dirty = false; // an option's dirtiness
    }
    get nodeType() { return 1; }
    get tagName() { return this.namespaceURI === "html" ? this.localName.toUpperCase() : this.localName; }
    get nodeName() { return this.tagName; }
    get nodeValue() { return null; }

    // attributes
    _name(n) {
      n = String(n);
      return this.namespaceURI === "html" ? n.toLowerCase() : n;
    }
    get attributes() { return this._attrs.map(function (a) { return { name: a.name, value: a.value }; }); }
    getAttributeNames() { return this._attrs.map(function (a) { return a.name; }); }
    getAttribute(n) {
      n = this._name(n);
      for (var i = 0; i < this._attrs.length; i++) if (this._attrs[i].name === n) return this._attrs[i].value;
      return null;
    }
    hasAttribute(n) { return this.getAttribute(n) !== null; }
    hasAttributes() { return this._attrs.length > 0; }
    setAttribute(n, v) {
      n = this._name(n);
      v = String(v);
      for (var i = 0; i < this._attrs.length; i++) {
        if (this._attrs[i].name === n) {
          var old = this._attrs[i].value;
          this._attrs[i].value = v;
          if (old !== v) attrChanged(this, n, old, v);
          return;
        }
      }
      this._attrs.push({ name: n, value: v });
      attrChanged(this, n, null, v);
    }
    removeAttribute(n) {
      n = this._name(n);
      for (var i = 0; i < this._attrs.length; i++) {
        if (this._attrs[i].name === n) {
          var old = this._attrs[i].value;
          this._attrs.splice(i, 1);
          attrChanged(this, n, old, null);
          return;
        }
      }
    }
    toggleAttribute(n, force) {
      var has = this.hasAttribute(n);
      var want = force === undefined ? !has : !!force;
      if (want && !has) this.setAttribute(n, "");
      if (!want && has) this.removeAttribute(n);
      return want;
    }
    get id() { return this.getAttribute("id") || ""; }
    set id(v) { this.setAttribute("id", v); }
    get className() { return this.getAttribute("class") || ""; }
    set className(v) { this.setAttribute("class", v); }
    get classList() {
      var el = this;
      var list = function () { return el.className.split(/\s+/).filter(Boolean); };
      return {
        contains: function (c) { return list().indexOf(c) >= 0; },
        add: function () {
          var l = list();
          Array.from(arguments).forEach(function (c) { if (l.indexOf(c) < 0) l.push(c); });
          el.className = l.join(" ");
        },
        remove: function () {
          var rm = Array.from(arguments);
          el.className = list().filter(function (c) { return rm.indexOf(c) < 0; }).join(" ");
        },
        toggle: function (c, force) {
          var on = force === undefined ? !this.contains(c) : !!force;
          if (on) this.add(c);
          else this.remove(c);
          return on;
        },
      };
    }
    get dataset() {
      var out = {};
      this._attrs.forEach(function (a) {
        if (a.name.indexOf("data-") === 0) {
          out[a.name.slice(5).replace(/-([a-z])/g, function (_, c) { return c.toUpperCase(); })] = a.value;
        }
      });
      return out;
    }
    get hidden() { return this.hasAttribute("hidden"); }
    set hidden(v) { this.toggleAttribute("hidden", !!v); }
    get disabled() { return this.hasAttribute("disabled"); }
    set disabled(v) { this.toggleAttribute("disabled", !!v); }
    get multiple() { return this.hasAttribute("multiple"); }
    set multiple(v) { this.toggleAttribute("multiple", !!v); }
    get readOnly() { return this.hasAttribute("readonly"); }
    get required() { return this.hasAttribute("required"); }
    get name() { return this.getAttribute("name") || ""; }
    set name(v) { this.setAttribute("name", v); }
    get href() {
      var h = this.getAttribute("href");
      if (h === null) return "";
      try {
        return new URL(h, G.location.href).href;
      } catch (e) {
        return h;
      }
    }
    set href(v) { this.setAttribute("href", v); }
    get target() { return this.getAttribute("target") || ""; }
    get style() { return this._style || (this._style = {}); }

    // tree
    get children() { return this._kids.filter(isElement); }
    get childElementCount() { return this.children.length; }
    get firstElementChild() { return this.children[0] || null; }
    get lastElementChild() {
      var c = this.children;
      return c[c.length - 1] || null;
    }
    get nextElementSibling() {
      for (var n = this.nextSibling; n; n = n.nextSibling) if (n.nodeType === 1) return n;
      return null;
    }
    get previousElementSibling() {
      for (var n = this.previousSibling; n; n = n.previousSibling) if (n.nodeType === 1) return n;
      return null;
    }
    querySelector(s) { return query(this, s, true)[0] || null; }
    querySelectorAll(s) { return query(this, s, false); }
    getElementsByTagName(t) { return query(this, t, false); }
    matches(s) { return matchesSelector(this, compile(s), this); }
    closest(s) {
      var sel = compile(s);
      for (var n = this; n && n.nodeType === 1; n = n.parentNode) if (matchesSelector(n, sel, n)) return n;
      return null;
    }
    cloneNode(deep) {
      var c = new Element(this.ownerDocument, this.localName, this.namespaceURI);
      this._attrs.forEach(function (a) { c._attrs.push({ name: a.name, value: a.value }); });
      c._value = this._value;
      c._checked = this._checked;
      c._sel = this._sel;
      c._dirty = this._dirty;
      if (deep) this._kids.forEach(function (k) { c.appendChild(k.cloneNode(true)); });
      return c;
    }
    get innerHTML() { return this._kids.map(serialize).join(""); }
    set innerHTML(html) {
      var frag = parseFragment(this.ownerDocument, String(html), this.localName);
      this._kids.slice().forEach(function (k) { this.removeChild(k); }, this);
      this.appendChild(frag);
    }
    get outerHTML() { return serialize(this); }
    insertAdjacentHTML(where, html) {
      var frag = parseFragment(this.ownerDocument, String(html), this.localName);
      switch (String(where).toLowerCase()) {
        case "beforebegin": this.parentNode.insertBefore(frag, this); break;
        case "afterbegin": this.insertBefore(frag, this.firstChild); break;
        case "beforeend": this.appendChild(frag); break;
        case "afterend": this.parentNode.insertBefore(frag, this.nextSibling); break;
      }
    }
    getBoundingClientRect() { return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
    scrollIntoView() {}

    // focus
    focus() {
      var doc = this.ownerDocument;
      if (!doc || doc._active === this) return;
      var prev = doc.activeElement;
      doc._active = this;
      if (prev && prev !== doc.body) {
        prev.dispatchEvent(new Event("blur"));
        prev.dispatchEvent(new Event("focusout", { bubbles: true }));
      }
      this.dispatchEvent(new Event("focus"));
      this.dispatchEvent(new Event("focusin", { bubbles: true }));
    }
    blur() {
      var doc = this.ownerDocument;
      if (!doc || doc._active !== this) return;
      doc._active = null;
      this.dispatchEvent(new Event("blur"));
      this.dispatchEvent(new Event("focusout", { bubbles: true }));
    }

    // click runs the click and, unless it is cancelled, the activation
    // behaviour of the element or its nearest activatable ancestor: a
    // checkbox toggles, a submit button submits its form, a link navigates.
    click() {
      var act = this.closest("button, input, a[href], label, select, textarea");
      if (act && (act.tagName === "BUTTON" || act.tagName === "INPUT" || act.tagName === "SELECT" || act.tagName === "TEXTAREA") && act.disabled) return;
      var toggle = act && act.tagName === "INPUT" && (act.type === "checkbox" || act.type === "radio") ? act : null;
      var before = toggle ? toggle._checked : null, was = toggle ? toggle.checked : false;
      if (toggle) toggle.checked = toggle.type === "checkbox" ? !was : true;
      var ok = this.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
      if (toggle) {
        if (!ok) {
          toggle._checked = before;
          if (toggle.type === "radio" && was) toggle.checked = true;
        } else if (toggle.checked !== was) {
          toggle.dispatchEvent(new Event("input", { bubbles: true }));
          toggle.dispatchEvent(new Event("change", { bubbles: true }));
        }
        return;
      }
      if (!ok || !act) return;
      if (act.tagName === "A") {
        G.location.assign(act.href);
      } else if (act.tagName === "BUTTON" || (act.tagName === "INPUT" && (act.type === "submit" || act.type === "reset"))) {
        var form = act.form;
        if (!form) return;
        if (act.type === "submit") form.requestSubmit(act);
        else if (act.type === "reset") form.reset();
      }
    }

    // form fields
    get type() {
      var t = (this.getAttribute("type") || "").toLowerCase();
      switch (this.tagName) {
        case "INPUT": return t || "text";
        case "BUTTON": return t === "button" || t === "reset" ? t : "submit";
        case "SELECT": return this.multiple ? "select-multiple" : "select-one";
        case "TEXTAREA": return "textarea";
      }
      return t;
    }
    set type(v) { this.setAttribute("type", v); }
    get form() {
      if (this.tagName === "FORM") return null;
      var f = this.parentNode;
      for (; f && f.nodeType === 1; f = f.parentNode) if (f.tagName === "FORM") return f;
      return null;
    }
    get value() {
      switch (this.tagName) {
        case "INPUT":
          if (this.type === "checkbox" || this.type === "radio") {
            var v = this.getAttribute("value");
            return v === null ? "on" : v;
          }
          if (this._value !== null) return this._value;
          return this.getAttribute("value") || "";
        case "TEXTAREA":
          return this._value !== null ? this._value : this.textContent;
        case "SELECT":
          var o = this.options.find(function (x) { return x.selected; });
          return o ? o.value : "";
        case "OPTION":
          var ov = this.getAttribute("value");
          return ov !== null ? ov : this.textContent.replace(/\s+/g, " ").trim();
        case "BUTTON":
        case "DATA":
        case "OUTPUT":
          return this.getAttribute("value") || "";
      }
      return undefined;
    }
    set value(v) {
      v = v == null ? "" : String(v);
      switch (this.tagName) {
        case "INPUT":
          if (this.type === "checkbox" || this.type === "radio") this.setAttribute("value", v);
          else this._value = v;
          return;
        case "TEXTAREA":
          this._value = v;
          return;
        case "SELECT":
          var hit = false;
          this.options.forEach(function (o) {
            o._sel = !hit && o.value === v;
            if (o._sel) {
              hit = true;
              o._dirty = true;
            }
          });
          return;
        default:
          this.setAttribute("value", v);
      }
    }
    get defaultValue() { return this.tagName === "TEXTAREA" ? this.textContent : this.getAttribute("value") || ""; }
    set defaultValue(v) {
      if (this.tagName === "TEXTAREA") this.textContent = v;
      else this.setAttribute("value", v);
    }
    get checked() { return this._checked !== null ? this._checked : this.hasAttribute("checked"); }
    set checked(v) {
      this._checked = !!v;
      if (this._checked && this.type === "radio") radioGroup(this).forEach(function (r) { if (r !== this) r._checked = false; }, this);
    }
    get defaultChecked() { return this.hasAttribute("checked"); }
    get options() {
      if (this.tagName !== "SELECT") return undefined;
      return query(this, "option", false);
    }
    get selectedOptions() { return this.options.filter(function (o) { return o.selected; }); }
    get selectedIndex() {
      if (this.tagName !== "SELECT") return undefined;
      return this.options.findIndex(function (o) { return o.selected; });
    }
    set selectedIndex(n) {
      this.options.forEach(function (o, i) {
        o._sel = i === n;
        if (o._sel) o._dirty = true;
      });
    }
    get selected() { return this._sel; }
    set selected(v) {
      this._dirty = true;
      this._sel = !!v;
      var sel = selectOf(this);
      if (sel && this._sel && !sel.multiple) {
        sel.options.forEach(function (o) { if (o !== this) o._sel = false; }, this);
      }
      if (sel) normalizeSelect(sel);
    }
    get defaultSelected() { return this.hasAttribute("selected"); }
    get index() {
      var sel = selectOf(this);
      return sel ? sel.options.indexOf(this) : 0;
    }
    get text() { return this.textContent; }
    get elements() {
      if (this.tagName !== "FORM") return undefined;
      return query(this, "input, select, textarea, button", false);
    }
    get method() { return (this.getAttribute("method") || "get").toLowerCase(); }
    get action() { return new URL(this.getAttribute("action") || "", G.location.href).href; }
    reset() {
      if (this.tagName !== "FORM") return;
      if (!this.dispatchEvent(new Event("reset", { bubbles: true, cancelable: true }))) return;
      this.elements.forEach(function (el) {
        el._value = null;
        el._checked = null;
        if (el.tagName === "SELECT") {
          el.options.forEach(function (o) {
            o._dirty = false;
            o._sel = o.hasAttribute("selected");
          });
          normalizeSelect(el);
        }
      });
    }
    requestSubmit(submitter) {
      if (this.tagName !== "FORM") return;
      if (!this.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true, submitter: submitter || null }))) return;
      this.submit(submitter);
    }
    submit(submitter) {
      // A form nothing handled: the browser would navigate.
      var fd = new FormData(this, submitter);
      var body = fd._entries.map(function (e) {
        return encodeURIComponent(e[0]) + "=" + encodeURIComponent(e[1]);
      }).join("&");
      var method = ((submitter && submitter.getAttribute("formmethod")) || this.method).toUpperCase();
      var action = new URL(this.getAttribute("action") || G.location.href, G.location.href);
      if (method === "GET") {
        go.navigate("GET", action.origin + action.pathname + (body ? "?" + body : ""), "", "");
      } else {
        go.navigate(method, action.href, body, "application/x-www-form-urlencoded");
      }
    }
  }

  function radioGroup(r) {
    var name = r.name;
    if (!name) return [r];
    var scope = r.form || r.getRootNode();
    return query(scope, "input", false).filter(function (x) { return x.type === "radio" && x.name === name && x.form === r.form; });
  }

  function selectOf(opt) {
    var p = opt.parentNode;
    if (p && p.nodeType === 1 && p.tagName === "OPTGROUP") p = p.parentNode;
    return p && p.nodeType === 1 && p.tagName === "SELECT" ? p : null;
  }

  // normalizeSelect is the browser's selectedness setting algorithm for a
  // single-choice select: none chosen picks the first enabled option, more
  // than one keeps the last.
  function normalizeSelect(sel) {
    if (sel.multiple) return;
    var opts = sel.options;
    var on = opts.filter(function (o) { return o._sel; });
    if (on.length === 0) {
      var first = opts.find(function (o) { return !o.disabled; });
      if (first) first._sel = true;
    } else if (on.length > 1) {
      on.slice(0, -1).forEach(function (o) { o._sel = false; });
    }
  }

  function attrChanged(el, name, old, v) {
    if (name === "selected" && el.tagName === "OPTION" && !el._dirty) {
      el._sel = v !== null;
      var sel = selectOf(el);
      if (sel) {
        if (el._sel && !sel.multiple) sel.options.forEach(function (o) { if (o !== el) o._sel = false; });
        normalizeSelect(sel);
      }
    } else if (name === "multiple" && el.tagName === "SELECT") {
      normalizeSelect(el);
    }
  }

  function inserted(node) {
    if (node.nodeType !== 1) return;
    var opts = node.tagName === "OPTION" ? [node] : node.tagName === "OPTGROUP" ? query(node, "option", false) : [];
    opts.forEach(function (o) {
      var sel = selectOf(o);
      if (!sel) return;
      if (o._sel && !sel.multiple) sel.options.forEach(function (x) { if (x !== o) x._sel = false; });
      normalizeSelect(sel);
    });
    if (node.tagName === "SELECT") normalizeSelect(node);
    query(node, "select", false).forEach(normalizeSelect);
  }

  function removed(node, parent) {
    if (node.nodeType !== 1) return;
    var doc = node.ownerDocument;
    if (doc && doc._active && node.contains(doc._active)) doc._active = null;
    if (node.tagName === "OPTION" || node.tagName === "OPTGROUP") {
      var sel = parent.tagName === "SELECT" ? parent : parent.tagName === "OPTGROUP" ? selectOf(parent) : null;
      if (sel) normalizeSelect(sel);
    }
  }

  class Document extends Node {
    constructor() {
      super(null);
      this.ownerDocument = null;
      this._active = null;
      this.readyState = "complete";
      this.visibilityState = "visible";
      this.hidden = false;
      this.defaultView = G;
    }
    get nodeType() { return 9; }
    get nodeName() { return "#document"; }
    get documentElement() { return this._kids.find(isElement) || null; }
    get head() { return this._find("HEAD"); }
    get body() { return this._find("BODY"); }
    _find(tag) {
      var root = this.documentElement;
      return (root && root.children.find(function (c) { return c.tagName === tag; })) || null;
    }
    get title() {
      var t = this.querySelector("title");
      return t ? t.textContent.replace(/\s+/g, " ").trim() : "";
    }
    set title(v) {
      var t = this.querySelector("title");
      if (!t && this.head) t = this.head.appendChild(this.createElement("title"));
      if (t) t.textContent = v;
    }
    get activeElement() {
      if (this._active && this._active.isConnected && this.contains(this._active)) return this._active;
      return this.body;
    }
    get children() { return this._kids.filter(isElement); }
    get textContent() { return null; }
    createElement(tag) { return new Element(this, tag, "html"); }
    createElementNS(ns, tag) { return new Element(this, tag, /svg/.test(ns) ? "svg" : /MathML/.test(ns) ? "math" : "html"); }
    createTextNode(s) { return new Text(this, s); }
    createComment(s) { return new Comment(this, s); }
    createDocumentFragment() { return new DocumentFragment(this); }
    importNode(n, deep) { return adopt(n.cloneNode(!!deep), this); }
    adoptNode(n) {
      if (n.parentNode) n.parentNode.removeChild(n);
      return adopt(n, this);
    }
    querySelector(s) { return query(this, s, true)[0] || null; }
    querySelectorAll(s) { return query(this, s, false); }
    getElementById(id) { return this.querySelector("#" + cssEscape(id)); }
    getElementsByTagName(t) { return query(this, t, false); }
  }

  function adopt(n, doc) {
    n.ownerDocument = doc;
    n._kids.forEach(function (k) { adopt(k, doc); });
    return n;
  }

  function cssEscape(s) { return String(s).replace(/([^a-zA-Z0-9_ -￿-])/g, "\\$1"); }

  // ---- parsing and serialising -------------------------------------------

  function build(doc, j) {
    if (j[0] === 3) return new Text(doc, j[1]);
    if (j[0] === 8) return new Comment(doc, j[1]);
    var el = new Element(doc, j[1], j[2]);
    j[3].forEach(function (a) {
      el._attrs.push({ name: a[0], value: a[1] });
      attrChanged(el, a[0], null, a[1]);
    });
    j[4].forEach(function (k) { el.appendChild(build(doc, k)); });
    return el;
  }

  function parseDocument(html) {
    var doc = new Document();
    JSON.parse(go.parseDocument(html)).forEach(function (j) { doc.appendChild(build(doc, j)); });
    return doc;
  }

  function parseFragment(doc, html, context) {
    var frag = new DocumentFragment(doc);
    JSON.parse(go.parseFragment(html, context)).forEach(function (j) { frag.appendChild(build(doc, j)); });
    return frag;
  }

  var RAW = { script: 1, style: 1, xmp: 1, iframe: 1, noembed: 1, noframes: 1, plaintext: 1 };

  function escText(s) { return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/ /g, "&nbsp;"); }
  function escAttr(s) { return s.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/ /g, "&nbsp;"); }

  function serialize(n) {
    if (n.nodeType === 3) {
      var p = n.parentNode;
      return p && p.nodeType === 1 && RAW[p.localName] ? n.data : escText(n.data);
    }
    if (n.nodeType === 8) return "<!--" + n.data + "-->";
    if (n.nodeType === 9 || n.nodeType === 11) return n._kids.map(serialize).join("");
    var s = "<" + n.localName;
    n._attrs.forEach(function (a) { s += " " + a.name + (a.value === "" ? "" : '="' + escAttr(a.value) + '"'); });
    s += ">";
    if (VOID[n.localName] && n.namespaceURI === "html") return s;
    return s + n._kids.map(serialize).join("") + "</" + n.localName + ">";
  }

  class DOMParser {
    parseFromString(s) { return parseDocument(String(s)); }
  }

  // ---- selectors ---------------------------------------------------------
  //
  // Compound selectors (tag, #id, .class, [attr], [attr=v] with = ~= ^= $=
  // *= |=, :scope, :checked, :disabled, :enabled, :not(…), :first-child,
  // :last-child) joined by descendant, >, + and ~, in comma lists.

  var compiled = new Map();

  function compile(src) {
    var c = compiled.get(src);
    if (c) return c;
    var p = { s: String(src), i: 0 };
    var list = [];
    for (;;) {
      list.push(parseComplex(p));
      ws(p);
      if (p.i >= p.s.length || p.s[p.i] === ")") break;
      if (p.s[p.i] !== ",") throw new SyntaxError("bad selector " + JSON.stringify(src));
      p.i++;
    }
    if (p.i < p.s.length && p.s[p.i] !== ")") throw new SyntaxError("bad selector " + JSON.stringify(src));
    compiled.set(src, list);
    return list;
  }

  function ws(p) { while (p.i < p.s.length && /\s/.test(p.s[p.i])) p.i++; }

  function parseComplex(p) {
    var parts = [];
    var comb = null;
    ws(p);
    for (;;) {
      var cmp = parseCompound(p);
      if (!cmp.length) throw new SyntaxError("bad selector " + JSON.stringify(p.s));
      parts.push({ comb: comb, cmp: cmp });
      var had = p.i < p.s.length && /\s/.test(p.s[p.i]);
      ws(p);
      var ch = p.s[p.i];
      if (ch === ">" || ch === "+" || ch === "~") {
        comb = ch;
        p.i++;
        ws(p);
      } else if (had && p.i < p.s.length && ch !== "," && ch !== ")") {
        comb = " ";
      } else {
        return parts;
      }
    }
  }

  function ident(p) {
    var m = /^(?:\\.|[\w -￿-])+/.exec(p.s.slice(p.i));
    if (!m) throw new SyntaxError("bad selector " + JSON.stringify(p.s));
    p.i += m[0].length;
    return m[0].replace(/\\(.)/g, "$1");
  }

  function parseCompound(p) {
    var out = [];
    for (;;) {
      var ch = p.s[p.i];
      if (ch === "*") {
        p.i++;
      } else if (ch === "#") {
        p.i++;
        out.push({ k: "id", v: ident(p) });
      } else if (ch === ".") {
        p.i++;
        out.push({ k: "class", v: ident(p) });
      } else if (ch === "[") {
        p.i++;
        ws(p);
        var name = ident(p).toLowerCase();
        ws(p);
        var op = null, val = null;
        var m = /^([~^$*|]?=)/.exec(p.s.slice(p.i));
        if (m) {
          op = m[1];
          p.i += op.length;
          ws(p);
          var q = p.s[p.i];
          if (q === '"' || q === "'") {
            var end = p.s.indexOf(q, p.i + 1);
            if (end < 0) throw new SyntaxError("bad selector " + JSON.stringify(p.s));
            val = p.s.slice(p.i + 1, end);
            p.i = end + 1;
          } else {
            val = ident(p);
          }
          ws(p);
        }
        if (p.s[p.i] !== "]") throw new SyntaxError("bad selector " + JSON.stringify(p.s));
        p.i++;
        out.push({ k: "attr", name: name, op: op, v: val });
      } else if (ch === ":") {
        p.i++;
        var ps = ident(p).toLowerCase();
        if (ps === "not") {
          if (p.s[p.i] !== "(") throw new SyntaxError("bad selector " + JSON.stringify(p.s));
          p.i++;
          var start = p.i, depth = 1;
          while (p.i < p.s.length && depth) {
            if (p.s[p.i] === "(") depth++;
            else if (p.s[p.i] === ")") depth--;
            p.i++;
          }
          out.push({ k: "not", v: compile(p.s.slice(start, p.i - 1)) });
        } else {
          out.push({ k: "pseudo", v: ps });
        }
      } else if (ch && /[\w-]/.test(ch) && (out.length === 0 || p.s[p.i - 1] === "*")) {
        out.push({ k: "tag", v: ident(p).toLowerCase() });
      } else {
        if (p.s[p.i - 1] === "*" && !out.length) out.push({ k: "any" });
        return out;
      }
    }
  }

  function matchCompound(el, cmp, scope) {
    for (var i = 0; i < cmp.length; i++) {
      var c = cmp[i];
      switch (c.k) {
        case "any":
          break;
        case "tag":
          if (el.localName.toLowerCase() !== c.v) return false;
          break;
        case "id":
          if (el.getAttribute("id") !== c.v) return false;
          break;
        case "class":
          if (el.className.split(/\s+/).indexOf(c.v) < 0) return false;
          break;
        case "attr":
          var a = el.getAttribute(c.name);
          if (a === null) return false;
          if (c.op === "=" && a !== c.v) return false;
          if (c.op === "~=" && a.split(/\s+/).indexOf(c.v) < 0) return false;
          if (c.op === "^=" && (!c.v || a.indexOf(c.v) !== 0)) return false;
          if (c.op === "$=" && (!c.v || a.slice(-c.v.length) !== c.v)) return false;
          if (c.op === "*=" && (!c.v || a.indexOf(c.v) < 0)) return false;
          if (c.op === "|=" && a !== c.v && a.indexOf(c.v + "-") !== 0) return false;
          break;
        case "not":
          if (matchesSelector(el, c.v, scope)) return false;
          break;
        case "pseudo":
          switch (c.v) {
            case "scope": if (el !== scope) return false; break;
            case "checked": if (!(el.tagName === "OPTION" ? el.selected : el.tagName === "INPUT" && el.checked)) return false; break;
            case "disabled": if (!el.disabled) return false; break;
            case "enabled": if (el.disabled) return false; break;
            case "first-child": if (el.previousElementSibling) return false; break;
            case "last-child": if (el.nextElementSibling) return false; break;
            default: throw new SyntaxError("unsupported selector :" + c.v);
          }
          break;
      }
    }
    return true;
  }

  function matchParts(el, parts, k, scope) {
    if (!matchCompound(el, parts[k].cmp, scope)) return false;
    if (k === 0) return true;
    var n;
    switch (parts[k].comb) {
      case ">":
        n = el.parentElement;
        return !!n && matchParts(n, parts, k - 1, scope);
      case " ":
        for (n = el.parentElement; n; n = n.parentElement) if (matchParts(n, parts, k - 1, scope)) return true;
        return false;
      case "+":
        n = el.previousElementSibling;
        return !!n && matchParts(n, parts, k - 1, scope);
      case "~":
        for (n = el.previousElementSibling; n; n = n.previousElementSibling) if (matchParts(n, parts, k - 1, scope)) return true;
        return false;
    }
    return false;
  }

  function matchesSelector(el, list, scope) {
    for (var i = 0; i < list.length; i++) if (matchParts(el, list[i], list[i].length - 1, scope)) return true;
    return false;
  }

  function query(root, src, first) {
    var list = compile(src);
    var scope = root.nodeType === 9 ? root.documentElement : root;
    var out = [];
    (function visit(n) {
      for (var i = 0; i < n._kids.length; i++) {
        var k = n._kids[i];
        if (k.nodeType !== 1) continue;
        if (matchesSelector(k, list, scope)) {
          out.push(k);
          if (first) return true;
        }
        if (visit(k)) return true;
      }
      return false;
    })(root);
    return out;
  }

  // ---- forms, URLs, network ----------------------------------------------

  class FormData {
    constructor(form, submitter) {
      this._entries = [];
      if (!form) return;
      var self = this;
      form.elements.forEach(function (el) {
        var name = el.getAttribute("name");
        if (!name || el.disabled || el.closest("fieldset[disabled]")) return;
        var t = el.type;
        if (el.tagName === "BUTTON" || t === "submit" || t === "image" || t === "reset" || t === "button") {
          if (el === submitter) self.append(name, el.value);
          return;
        }
        if (t === "file") return;
        if (t === "checkbox" || t === "radio") {
          if (el.checked) self.append(name, el.value);
          return;
        }
        if (el.tagName === "SELECT") {
          el.options.forEach(function (o) { if (o.selected && !o.disabled) self.append(name, o.value); });
          return;
        }
        self.append(name, el.value);
      });
    }
    append(k, v) { this._entries.push([String(k), String(v)]); }
    set(k, v) {
      this.delete(k);
      this.append(k, v);
    }
    delete(k) { this._entries = this._entries.filter(function (e) { return e[0] !== k; }); }
    get(k) {
      var e = this._entries.find(function (x) { return x[0] === k; });
      return e ? e[1] : null;
    }
    getAll(k) { return this._entries.filter(function (x) { return x[0] === k; }).map(function (x) { return x[1]; }); }
    has(k) { return this._entries.some(function (x) { return x[0] === k; }); }
    forEach(fn, self) { this._entries.forEach(function (e) { fn.call(self, e[1], e[0], this); }, this); }
    entries() { return this._entries.slice()[Symbol.iterator](); }
    [Symbol.iterator]() { return this.entries(); }
  }

  class URLSearchParams {
    constructor(s) {
      this._e = [];
      s = String(s || "").replace(/^\?/, "");
      if (s) s.split("&").forEach(function (kv) {
        var i = kv.indexOf("=");
        var k = i < 0 ? kv : kv.slice(0, i), v = i < 0 ? "" : kv.slice(i + 1);
        this._e.push([decodeURIComponent(k.replace(/\+/g, " ")), decodeURIComponent(v.replace(/\+/g, " "))]);
      }, this);
    }
    get(k) {
      var e = this._e.find(function (x) { return x[0] === k; });
      return e ? e[1] : null;
    }
    has(k) { return this._e.some(function (x) { return x[0] === k; }); }
    toString() { return this._e.map(function (e) { return encodeURIComponent(e[0]) + "=" + encodeURIComponent(e[1]); }).join("&"); }
  }

  class URL {
    constructor(u, base) {
      var r = JSON.parse(go.resolveURL(String(u), base == null ? "" : String(base)));
      if (r.error) throw new TypeError("Invalid URL: " + u);
      Object.assign(this, r);
      this.searchParams = new URLSearchParams(this.search);
    }
    toString() { return this.href; }
    toJSON() { return this.href; }
  }

  class Headers {
    constructor(h) {
      this._h = {};
      if (h) Object.keys(h).forEach(function (k) { this._h[k.toLowerCase()] = String(h[k]); }, this);
    }
    get(k) {
      var v = this._h[String(k).toLowerCase()];
      return v === undefined ? null : v;
    }
    has(k) { return this.get(k) !== null; }
    forEach(fn) { Object.keys(this._h).forEach(function (k) { fn(this._h[k], k); }, this); }
  }

  class Response {
    constructor(r) {
      this.status = r.status;
      this.ok = r.status >= 200 && r.status < 300;
      this.url = r.url;
      this.headers = new Headers(r.headers);
      this.redirected = !!r.redirected;
      this._body = r.body;
    }
    text() { return Promise.resolve(this._body); }
    json() {
      var b = this._body;
      return new Promise(function (resolve) { resolve(JSON.parse(b)); });
    }
  }

  class AbortController {
    constructor() { this.signal = { aborted: false, addEventListener: function () {}, removeEventListener: function () {} }; }
    abort() { this.signal.aborted = true; }
  }

  function abortError() {
    var e = new Error("The operation was aborted.");
    e.name = "AbortError";
    return e;
  }

  function fetch(input, init) {
    init = init || {};
    return new Promise(function (resolve, reject) {
      if (init.signal && init.signal.aborted) return reject(abortError());
      var url = new URL(typeof input === "string" ? input : input.url, G.location.href).href;
      var headers = init.headers || {};
      var r = JSON.parse(go.fetch(String(init.method || "GET").toUpperCase(), url, JSON.stringify(headers), init.body == null ? "" : String(init.body)));
      if (r.error) return reject(new TypeError("Failed to fetch: " + r.error));
      resolve(new Response(r));
    });
  }

  class WebSocket extends EventTarget {
    constructor(url) {
      super();
      this.url = String(url);
      this.readyState = 0;
      this.onopen = this.onmessage = this.onclose = this.onerror = null;
      this._id = go.wsOpen(this.url, this);
    }
    send(data) {
      if (this.readyState !== 1) throw new Error("InvalidStateError: WebSocket is not open");
      go.wsSend(this._id, String(data));
    }
    close() {
      if (this.readyState >= 2) return;
      this.readyState = 2;
      go.wsClose(this._id);
    }
    _open() {
      if (this.readyState !== 0) return;
      this.readyState = 1;
      this._fire("open", new Event("open"));
    }
    _message(data) {
      if (this.readyState !== 1) return;
      var ev = new Event("message");
      ev.data = data;
      this._fire("message", ev);
    }
    _close(code) {
      if (this.readyState === 3) return;
      this.readyState = 3;
      var ev = new Event("close");
      ev.code = code || 1006;
      ev.wasClean = code === 1000;
      this._fire("close", ev);
    }
    _fire(type, ev) {
      var h = this["on" + type];
      if (h) {
        try {
          h.call(this, ev);
        } catch (err) {
          report(err);
        }
      }
      this.dispatchEvent(ev);
    }
  }
  WebSocket.CONNECTING = 0;
  WebSocket.OPEN = 1;
  WebSocket.CLOSING = 2;
  WebSocket.CLOSED = 3;

  // ---- timers: a virtual clock the host advances -------------------------

  var clock = 0, seq = 0, timers = [];
  function addTimer(fn, ms, args, every) {
    var id = ++seq;
    ms = Math.max(0, Number(ms) || 0);
    timers.push({ id: id, at: clock + ms, fn: fn, args: args, every: every ? Math.max(1, ms) : 0 });
    return id;
  }
  G.setTimeout = function (fn, ms) { return addTimer(fn, ms, Array.prototype.slice.call(arguments, 2), false); };
  G.setInterval = function (fn, ms) { return addTimer(fn, ms, Array.prototype.slice.call(arguments, 2), true); };
  G.clearTimeout = G.clearInterval = function (id) { timers = timers.filter(function (t) { return t.id !== id; }); };
  G.requestAnimationFrame = function (fn) { return addTimer(function () { fn(clock); }, 16, [], false); };
  G.cancelAnimationFrame = G.clearTimeout;
  G.queueMicrotask = function (fn) { Promise.resolve().then(fn); };

  function nextTimer() {
    var best = null;
    timers.forEach(function (t) { if (!best || t.at < best.at || (t.at === best.at && t.id < best.id)) best = t; });
    return best;
  }

  // ---- location and history ----------------------------------------------

  var here = new URL("http://localhost/");
  var location = {
    get href() { return here.href; },
    set href(v) { this.assign(v); },
    get origin() { return here.origin; },
    get protocol() { return here.protocol; },
    get host() { return here.host; },
    get hostname() { return here.hostname; },
    get port() { return here.port; },
    get pathname() { return here.pathname; },
    get search() { return here.search; },
    get hash() { return here.hash; },
    assign: function (u) { go.navigate("GET", new URL(u, here.href).href, "", ""); },
    replace: function (u) { go.navigate("GET", new URL(u, here.href).href, "", ""); },
    reload: function () { go.navigate("GET", here.href, "", ""); },
    toString: function () { return here.href; },
  };

  var entries = [{ url: here.href, state: null }], at = 0;
  var history = {
    get state() { return entries[at].state; },
    get length() { return entries.length; },
    pushState: function (state, _title, url) {
      entries = entries.slice(0, at + 1);
      if (url != null) here = new URL(url, here.href);
      entries.push({ url: here.href, state: clone(state) });
      at++;
    },
    replaceState: function (state, _title, url) {
      if (url != null) here = new URL(url, here.href);
      entries[at] = { url: here.href, state: clone(state) };
    },
    go: function (n) {
      var to = at + n;
      if (to < 0 || to >= entries.length || n === 0) return;
      at = to;
      here = new URL(entries[at].url);
      G.setTimeout(function () {
        var ev = new Event("popstate");
        ev.state = entries[at].state;
        G.dispatchEvent(ev);
      }, 0);
    },
    back: function () { this.go(-1); },
    forward: function () { this.go(1); },
  };

  function clone(v) { return v === undefined ? null : JSON.parse(JSON.stringify(v)); }

  // ---- console -----------------------------------------------------------

  function fmt(args) {
    return Array.prototype.map.call(args, function (a) {
      if (typeof a === "string") return a;
      if (a instanceof Error) return a.stack || String(a);
      try {
        return JSON.stringify(a);
      } catch (e) {
        return String(a);
      }
    }).join(" ");
  }
  G.console = {};
  ["log", "info", "warn", "error", "debug"].forEach(function (level) {
    G.console[level] = function () { go.console(level, fmt(arguments)); };
  });

  // ---- the window --------------------------------------------------------

  G.window = G.self = G;
  G._ls = {};
  G.addEventListener = EventTarget.prototype.addEventListener;
  G.removeEventListener = EventTarget.prototype.removeEventListener;
  G.dispatchEvent = function (ev) {
    ev.target = G;
    invoke(G, ev, "target");
    return !ev.defaultPrevented;
  };
  G.location = location;
  G.history = history;
  G.navigator = { userAgent: "nexus-viewtest", onLine: true, language: "en" };
  G.scrollTo = G.scroll = function () {};
  G.Node = Node;
  G.Element = G.HTMLElement = Element;
  G.Text = Text;
  G.Comment = Comment;
  G.Document = Document;
  G.DocumentFragment = DocumentFragment;
  G.Event = Event;
  G.MouseEvent = MouseEvent;
  G.KeyboardEvent = KeyboardEvent;
  G.CustomEvent = CustomEvent;
  G.SubmitEvent = SubmitEvent;
  G.EventTarget = EventTarget;
  G.DOMParser = DOMParser;
  G.FormData = FormData;
  G.URL = URL;
  G.URLSearchParams = URLSearchParams;
  G.Headers = Headers;
  G.Response = Response;
  G.AbortController = AbortController;
  G.fetch = fetch;
  G.WebSocket = WebSocket;
  G.document = new Document();

  // ---- the host's handle -------------------------------------------------

  function normText(s) { return String(s).replace(/\s+/g, " ").trim(); }

  // field finds a form field by name, else any element by CSS selector.
  function find(loc) {
    if (/^[A-Za-z_][\w.:-]*(\[\])?$/.test(loc)) {
      var byName = G.document.querySelector('[name="' + loc.replace(/"/g, '\\"') + '"]');
      if (byName) return byName;
    }
    return G.document.querySelector(loc);
  }

  function editable(el) {
    if (!el) return "no element";
    if (el.tagName !== "INPUT" && el.tagName !== "TEXTAREA" && el.tagName !== "SELECT") return "not a form field: " + serialize(el).slice(0, 200);
    if (el.disabled) return "the field is disabled";
    if (el.readOnly) return "the field is read-only";
    return "";
  }

  function fire(el, type) { el.dispatchEvent(new Event(type, { bubbles: true })); }

  function visible(el) {
    for (var n = el; n && n.nodeType === 1; n = n.parentNode) {
      if (n.hasAttribute("hidden")) return false;
      var t = n.tagName;
      if (t === "TEMPLATE" || t === "SCRIPT" || t === "STYLE" || t === "HEAD") return false;
      if (t === "INPUT" && n.type === "hidden") return false;
    }
    return el.isConnected;
  }

  G.__dom = {
    load: function (html, url) {
      here = new URL(url);
      entries = [{ url: here.href, state: null }];
      at = 0;
      var doc = parseDocument(html);
      doc.readyState = "complete";
      G.document = doc;
    },
    nextTimer: function () {
      var t = nextTimer();
      return t ? t.at : -1;
    },
    now: function () { return clock; },
    runTimer: function () {
      var t = nextTimer();
      if (!t) return false;
      clock = Math.max(clock, t.at);
      if (t.every) t.at = clock + t.every;
      else timers = timers.filter(function (x) { return x !== t; });
      try {
        t.fn.apply(G, t.args);
      } catch (err) {
        report(err);
      }
      return true;
    },
    find: find,
    findAll: function (sel) { return G.document.querySelectorAll(sel); },
    exists: function (loc) { return !!find(loc); },
    count: function (sel) { return G.document.querySelectorAll(sel).length; },
    text: function (el) { return normText(el.textContent); },
    html: function (el) { return el ? serialize(el) : "<!DOCTYPE html>" + serialize(G.document); },
    attr: function (el, name) { return el.getAttribute(name); },
    value: function (el) {
      if (el.tagName === "SELECT" && el.multiple) return el.selectedOptions.map(function (o) { return o.value; }).join(",");
      return el.value == null ? "" : String(el.value);
    },
    checked: function (el) { return el.tagName === "OPTION" ? el.selected : !!el.checked; },
    disabled: function (el) { return !!el.disabled || !!el.closest("fieldset[disabled]"); },
    visible: visible,
    focused: function (el) { return G.document.activeElement === el; },
    url: function () { return here.href; },
    back: function () { history.back(); },
    // fill types value into a field: focus, the value, input and change.
    fill: function (el, value) {
      var bad = editable(el);
      if (bad) return bad;
      if (el.tagName === "SELECT") return "a select: use Select";
      if (el.type === "checkbox" || el.type === "radio") return "a " + el.type + ": use Check";
      el.focus();
      el.value = value;
      fire(el, "input");
      fire(el, "change");
      return "";
    },
    // choose selects the options with these values (all of them on a
    // multiple select; exactly one otherwise).
    choose: function (el, values) {
      var bad = editable(el);
      if (bad) return bad;
      if (el.tagName !== "SELECT") return "not a select";
      if (!el.multiple && values.length !== 1) return "a single select takes one value";
      var opts = el.options;
      for (var i = 0; i < values.length; i++) {
        if (!opts.some(function (o) { return o.value === values[i]; })) {
          return "no option " + JSON.stringify(values[i]) + " (have " + JSON.stringify(opts.map(function (o) { return o.value; })) + ")";
        }
      }
      el.focus();
      opts.forEach(function (o) {
        var on = values.indexOf(o.value) >= 0;
        if (o.selected !== on || on) o.selected = on;
      });
      fire(el, "input");
      fire(el, "change");
      return "";
    },
    check: function (el, on) {
      var bad = editable(el);
      if (bad) return bad;
      if (el.type !== "checkbox" && el.type !== "radio") return "not a checkbox or radio";
      if (!on && el.type === "radio") return "a radio is unchecked by checking another";
      el.focus();
      if (el.checked !== on) el.click();
      return "";
    },
    click: function (el) {
      if (FOCUSABLE[el.tagName]) el.focus();
      else if (G.document._active) G.document._active.blur();
      el.click();
      return "";
    },
    submit: function (el) {
      var form = el.tagName === "FORM" ? el : el.form;
      if (!form) return "not in a form";
      form.requestSubmit(el.tagName === "BUTTON" || el.tagName === "INPUT" && el.type === "submit" ? el : null);
      return "";
    },
    focus: function (el) { el.focus(); },
    blur: function () { if (G.document._active) G.document._active.blur(); },
    press: function (el, key) {
      el.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: key }));
      el.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, cancelable: true, key: key }));
      return "";
    },
  };
})(globalThis);
