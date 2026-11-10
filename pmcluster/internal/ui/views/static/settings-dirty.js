/* The settings page's sticky save bar.
 *
 * Two jobs, both delegated from the document so they survive an htmx swap of
 * #view (this script is loaded once by the shell, not re-run per fragment):
 *
 *   1. dirty/clean. The cluster settings are one form; its bar says whether the
 *      form differs from what the daemon last rendered. Comparing serialized
 *      values (not a `change` flag) means typing a value and typing it back
 *      reports clean, which is the truth the operator needs before deciding to
 *      press Save. There is no autosave anywhere on this page.
 *
 *   2. dismissing a confirmation. Server-rendered, so it survives a reload
 *      until the operator closes it.
 *
 * A form with no bar (or a bar with no form, e.g. the state where the daemon did
 * not answer) is a no-op, never an error.
 */
(function () {
  "use strict";

  var baseline = null;

  function bar() { return document.querySelector("[data-savebar]"); }
  function form() { return document.querySelector("[data-save-form]"); }

  // serialize returns the form's current values as one comparable string. The
  // encoding is internal to this file; it only has to be stable across two
  // snapshots of the same form.
  function serialize(f) {
    var parts = [];
    var fields = f.querySelectorAll("input, select, textarea");
    for (var i = 0; i < fields.length; i++) {
      var el = fields[i];
      if (!el.name || el.type === "submit" || el.type === "button") continue;
      var value;
      if (el.type === "checkbox" || el.type === "radio") {
        value = el.checked ? "1" : "0";
      } else if (el.type === "file") {
        // A picked file cannot be compared across snapshots; its mere presence
        // counts, and its name is not part of the cluster form anyway.
        value = el.files && el.files.length ? "file" : "";
      } else {
        value = el.value;
      }
      parts.push(el.name + "=" + value);
    }
    return parts.join("&");
  }

  function paint(state) {
    var b = bar();
    if (!b) return;
    if (b.getAttribute("data-save-state") !== state) {
      b.setAttribute("data-save-state", state);
    }
  }

  function sync() {
    var f = form();
    if (!f) { baseline = null; return; }
    var now = serialize(f);
    if (baseline === null) baseline = now;
    paint(now === baseline ? "clean" : "dirty");
  }

  // init re-reads the baseline: a swap brought a new form (fresh from the
  // daemon) or none at all.
  function init() { baseline = null; sync(); }

  document.addEventListener("input", sync, true);
  document.addEventListener("change", sync, true);
  document.addEventListener("DOMContentLoaded", init);
  document.addEventListener("htmx:afterSwap", init);
  document.addEventListener("htmx:afterSettle", init);

  document.addEventListener("click", function (ev) {
    var target = ev.target;
    if (!target || !target.closest) return;
    var btn = target.closest("[data-dismiss]");
    if (!btn) return;
    var box = btn.closest("[data-msg], .msg, .banner");
    if (box) box.remove();
    ev.preventDefault();
  });

  if (document.readyState !== "loading") init();
})();
