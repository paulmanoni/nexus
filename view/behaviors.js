// nexus view behaviors: what a page's markup asks for with data-nx
// attributes, with no styling of its own. Each works through events on the
// document, so markup a live re-render brings works the same; @view.Behaviors()
// loads it.
(function () {
  "use strict";

  // ---- Filter ---------------------------------------------------------------
  // A box with data-nx-filter="<selector>" hides, as it is typed in, the
  // [data-nx-filter-item] elements inside what the selector matches whose
  // text doesn't hold what was typed; a [data-nx-filter-empty] element there
  // shows when none is left. Hidden items keep their state: a checked box
  // stays checked, and is still sent.
  function filter(box) {
    var q = box.value.trim().toLowerCase();
    document.querySelectorAll(box.getAttribute("data-nx-filter")).forEach(function (scope) {
      var shown = 0;
      scope.querySelectorAll("[data-nx-filter-item]").forEach(function (item) {
        var text = (item.getAttribute("data-nx-filter-item") || item.textContent).toLowerCase();
        var on = q === "" || text.indexOf(q) >= 0;
        if (item.hidden === on) item.hidden = !on;
        if (on) shown++;
      });
      scope.querySelectorAll("[data-nx-filter-empty]").forEach(function (e) {
        if (e.hidden !== shown > 0) e.hidden = shown > 0;
      });
    });
  }
  document.addEventListener("input", function (e) {
    if (e.target.matches && e.target.matches("[data-nx-filter]")) filter(e.target);
  });

  // ---- Select all -----------------------------------------------------------
  // A checkbox with data-nx-check-all checks or clears the other boxes of
  // its group (the nearest [data-nx-checks], else its form), and shows
  // partly checked when some of them are; a [data-nx-checked-count] element
  // in the group shows how many are. It runs before the form's own change
  // handler, so a view.Change sends the boxes as they now are.
  function groupOf(el) { return el.closest("[data-nx-checks]") || el.closest("form"); }
  function boxesOf(group) {
    return Array.prototype.filter.call(group.querySelectorAll('input[type="checkbox"]'), function (b) {
      return !b.hasAttribute("data-nx-check-all");
    });
  }
  function syncChecks(root) {
    (root || document).querySelectorAll("[data-nx-check-all]").forEach(function (all) {
      var group = groupOf(all);
      if (!group) return;
      var boxes = boxesOf(group);
      var on = boxes.filter(function (b) { return b.checked; }).length;
      all.checked = boxes.length > 0 && on === boxes.length;
      all.indeterminate = on > 0 && on < boxes.length;
      group.querySelectorAll("[data-nx-checked-count]").forEach(function (c) {
        if (c.textContent !== String(on)) c.textContent = on;
      });
    });
  }
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (!(t instanceof HTMLInputElement) || t.type !== "checkbox") return;
    if (t.hasAttribute("data-nx-check-all")) {
      var group = groupOf(t);
      if (group) boxesOf(group).forEach(function (b) { if (!b.disabled) b.checked = t.checked; });
    }
    syncChecks();
  }, true);

  // ---- Valid ----------------------------------------------------------------
  // A form with data-nx-valid has its submit buttons enabled only while its
  // fields pass the browser's own checks (required, minlength, pattern,
  // type=email, ...): the button says the form is ready.
  function validate(form) {
    var ok = form.checkValidity();
    form.querySelectorAll('button[type="submit"], button:not([type]), input[type="submit"]').forEach(function (b) {
      if (b.getAttribute("aria-busy") === "true") return;
      if (b.disabled === ok) b.disabled = !ok;
    });
  }
  function validateAll(root) {
    (root || document).querySelectorAll("form[data-nx-valid]").forEach(validate);
  }
  ["input", "change"].forEach(function (type) {
    document.addEventListener(type, function (e) {
      var form = e.target.closest && e.target.closest("form[data-nx-valid]");
      if (form) validate(form);
    });
  });

  // A live re-render renders the page as the server knows it: items without
  // hidden, buttons as rendered. Each behavior is applied again after it;
  // every write above is made only on a change, so this settles at once.
  function refresh() {
    document.querySelectorAll("[data-nx-filter]").forEach(function (box) { if (box.value) filter(box); });
    syncChecks();
    validateAll();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", refresh);
  else refresh();
  new MutationObserver(refresh).observe(document.documentElement,
    { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden", "disabled"] });
})();
