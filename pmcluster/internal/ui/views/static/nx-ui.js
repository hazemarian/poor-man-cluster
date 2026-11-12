// Site-native dropdowns and confirmations, ported from nx-master's ui.js.
// Every <select> is replaced by a styled button + listbox menu (the real <select> stays in the DOM, hidden,
// so forms, htmx and change handlers behave as before); touch devices get the same menu as a bottom sheet.
// hx-confirm questions open in the site's own dialog instead of the browser's confirm().
(function () {
  "use strict";
  var coarse = !!(window.matchMedia && matchMedia("(pointer: coarse)").matches);

  var CHEV = '<svg class="dd-chev" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9l6 6 6-6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var CHECK = '<svg class="dd-check" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var current = null; // the open dropdown, if any
  var seq = 0;

  function text(option) { return option ? option.textContent.replace(/\s+/g, " ").trim() : ""; }

  function enhance(sel) {
    if (sel.dataset.dd || sel.multiple || sel.size > 1 || sel.hasAttribute("data-native")) return;
    sel.dataset.dd = "1";
    var id = "dd" + (++seq);

    var wrap = document.createElement("div");
    // The control look lives on the button; copying finput/selectbox onto the wrapper drew a second box around it.
    wrap.className = "dd " + sel.className.replace(/\b(finput|selectbox|dd-native|dd)\b/g, "").replace(/\s+/g, " ").trim();
    sel.parentNode.insertBefore(wrap, sel);
    wrap.appendChild(sel);
    sel.classList.add("dd-native");
    sel.tabIndex = -1;
    sel.setAttribute("aria-hidden", "true");

    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "dd-btn";
    btn.id = id;
    btn.setAttribute("aria-haspopup", "listbox");
    btn.setAttribute("aria-expanded", "false");
    var aria = sel.getAttribute("aria-label");
    if (aria) btn.setAttribute("aria-label", aria);
    else if (sel.labels && sel.labels.length) {
      var l = sel.labels[0];
      if (!l.id) l.id = id + "-l";
      btn.setAttribute("aria-labelledby", l.id + " " + id);
    }
    if (sel.title) btn.title = sel.title;
    btn.innerHTML = '<span class="dd-label"></span>' + CHEV;
    wrap.insertBefore(btn, sel);

    var menu = null, items = [], active = -1, typed = "", typedAt = 0;

    function refresh() {
      var cur = sel.options[sel.selectedIndex];
      btn.querySelector(".dd-label").textContent =
        (sel.hasAttribute("data-dd-short") && cur && cur.dataset.short) || text(cur) || "—";
      btn.disabled = sel.disabled;
      wrap.classList.toggle("is-empty", !sel.value);
    }

    function setActive(i, scroll) {
      if (active >= 0 && items[active]) items[active].el.classList.remove("active");
      active = i;
      if (i < 0 || !items[i]) { btn.removeAttribute("aria-activedescendant"); return; }
      items[i].el.classList.add("active");
      btn.setAttribute("aria-activedescendant", items[i].el.id);
      if (scroll && menu) {
        // Scroll the menu itself. scrollIntoView also scrolls every ancestor (the dialog, the page),
        // which is what made the page jump and the scroll listener then slam the menu shut.
        var el = items[i].el, top = el.offsetTop, bottom = top + el.offsetHeight;
        if (top < menu.scrollTop) menu.scrollTop = top - 5;
        else if (bottom > menu.scrollTop + menu.clientHeight) menu.scrollTop = bottom - menu.clientHeight + 5;
      }
    }

    function step(dir) {
      var i = active;
      do { i += dir; } while (i >= 0 && i < items.length && items[i].option.disabled);
      if (i >= 0 && i < items.length) setActive(i, true);
    }

    function close(refocus) {
      if (!menu) return;
      menu.remove(); menu = null; items = []; active = -1;
      wrap.classList.remove("open");
      btn.setAttribute("aria-expanded", "false");
      btn.removeAttribute("aria-activedescendant");
      if (current === api) current = null;
      document.removeEventListener("pointerdown", onOutside, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onResize);
      if (refocus) btn.focus();
    }

    function choose(i) {
      var o = items[i] && items[i].option;
      if (!o || o.disabled) return;
      var changed = sel.selectedIndex !== o.index;
      sel.selectedIndex = o.index;
      refresh();
      close(true);
      if (changed) {
        sel.dispatchEvent(new Event("input", { bubbles: true }));
        sel.dispatchEvent(new Event("change", { bubbles: true }));
      }
    }

    function place() {
      if (coarse) { menu.classList.add("is-sheet"); menu.style.left = menu.style.top = ""; menu.style.maxHeight = ""; return; }
      var r = btn.getBoundingClientRect();
      menu.style.minWidth = Math.max(r.width, btn.closest(".lang-switch") ? 116 : 160) + "px";
      var w = menu.offsetWidth, h = menu.offsetHeight, vw = innerWidth, vh = innerHeight;
      var below = vh - r.bottom - 10, above = r.top - 10;
      // Decide on the height the menu will really have (capped), not its natural height: a long list
      // is scrolled, so it needs 340px at most. Using the natural height flipped it upward and, once
      // capped, left it floating far from its button.
      var want = Math.min(h, 340);
      var up = want > below && above > below;
      var mh = Math.max(120, Math.min(340, up ? above : below));
      menu.style.maxHeight = mh + "px";
      var real = Math.min(h, mh);
      var top = up ? Math.max(8, r.top - real - 6) : r.bottom + 6;
      var rtl = getComputedStyle(btn).direction === "rtl";
      var left = rtl ? r.right - w : r.left;
      menu.style.left = Math.max(8, Math.min(left, vw - w - 8)) + "px";
      menu.style.top = top + "px";
    }

    function open() {
      if (menu || sel.disabled) return;
      if (current) current.close(false);
      menu = document.createElement("ul");
      menu.className = "dd-menu" + (btn.closest(".lang-switch") ? " is-lang" : "");
      menu.setAttribute("role", "listbox");
      menu.id = id + "-menu";
      if (aria) menu.setAttribute("aria-label", aria);
      var n = 0;
      function addOption(o, parent) {
        var li = document.createElement("li");
        li.className = "dd-opt";
        li.id = id + "-o" + n;
        li.setAttribute("role", "option");
        var selected = o.index === sel.selectedIndex;
        li.setAttribute("aria-selected", String(selected));
        if (o.disabled) li.setAttribute("aria-disabled", "true");
        var label = document.createElement("span");
        label.className = "dd-text";
        label.textContent = text(o) || "—";
        li.appendChild(label);
        if (o.dataset.hint) {
          var hint = document.createElement("span");
          hint.className = "dd-hint";
          hint.textContent = o.dataset.hint;
          li.appendChild(hint);
        }
        if (selected && !btn.closest(".lang-switch")) li.insertAdjacentHTML("beforeend", CHECK);
        (function (idx) {
          li.addEventListener("pointermove", function () { if (active !== idx) setActive(idx, false); });
          li.addEventListener("click", function () { choose(idx); });
        })(n);
        items.push({ el: li, option: o });
        parent.appendChild(li);
        n++;
      }
      Array.prototype.forEach.call(sel.children, function (c) {
        if (c.tagName === "OPTGROUP") {
          var g = document.createElement("li");
          g.className = "dd-group"; g.setAttribute("role", "presentation"); g.textContent = c.label;
          menu.appendChild(g);
          Array.prototype.forEach.call(c.children, function (o) { addOption(o, menu); });
        } else if (c.tagName === "OPTION") addOption(c, menu);
      });
      // A menu inside a modal dialog must live in it, or the dialog's top layer hides the menu.
      (sel.closest("dialog") || document.body).appendChild(menu);
      wrap.classList.add("open");
      btn.setAttribute("aria-expanded", "true");
      btn.setAttribute("aria-controls", menu.id);
      place();
      setActive(Math.max(0, sel.selectedIndex), true);
      current = api;
      document.addEventListener("pointerdown", onOutside, true);
      window.addEventListener("scroll", onScroll, true);
      window.addEventListener("resize", onResize);
    }

    function onOutside(e) { if (menu && !menu.contains(e.target) && !btn.contains(e.target)) close(false); }
    function onScroll(e) {
      if (!menu || menu.contains(e.target)) return;
      // Only a scroll that carries the button away closes it.
      var t = e.target;
      if (t === document || t === document.documentElement || (t.contains && t.contains(btn))) close(false);
    }
    function onResize() { close(false); }

    btn.addEventListener("click", function () { menu ? close(true) : open(); });
    btn.addEventListener("keydown", function (e) {
      var k = e.key;
      if (!menu) {
        if (k === "ArrowDown" || k === "ArrowUp" || k === "Enter" || k === " ") { e.preventDefault(); open(); }
        return;
      }
      if (k === "ArrowDown") { e.preventDefault(); step(1); }
      else if (k === "ArrowUp") { e.preventDefault(); step(-1); }
      else if (k === "Home") { e.preventDefault(); setActive(0, true); }
      else if (k === "End") { e.preventDefault(); setActive(items.length - 1, true); }
      else if (k === "Enter" || k === " ") { e.preventDefault(); choose(active); }
      else if (k === "Escape") { e.preventDefault(); e.stopPropagation(); close(true); }
      else if (k === "Tab") close(false);
      else if (k.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        var now = Date.now();
        typed = (now - typedAt > 700 ? "" : typed) + k.toLowerCase(); typedAt = now;
        for (var i = 0; i < items.length; i++) {
          var j = (active + 1 + i) % items.length;
          if (!items[j].option.disabled && text(items[j].option).toLowerCase().indexOf(typed) === 0) { setActive(j, true); break; }
        }
      }
    });
    // A <label for> or a script focusing the select lands on the visible control.
    sel.addEventListener("focus", function () { btn.focus(); });
    sel.addEventListener("change", refresh);

    var api = { close: close, refresh: refresh };
    wrap._dd = api;
    refresh();
  }

  function scan(root) {
    if (root.nodeType !== 1) return;
    if (root.tagName === "SELECT") enhance(root);
    var found = root.querySelectorAll ? root.querySelectorAll("select") : [];
    for (var i = 0; i < found.length; i++) enhance(found[i]);
  }

  scan(document.body);
  new MutationObserver(function (records) {
    records.forEach(function (r) { r.addedNodes.forEach(scan); });
  }).observe(document.body, { childList: true, subtree: true });

  // form.reset() changes the select's value without a change event.
  document.addEventListener("reset", function (e) {
    setTimeout(function () {
      e.target.querySelectorAll(".dd").forEach(function (w) { if (w._dd) w._dd.refresh(); });
    }, 0);
  });
})();

(function () {
  var AR = document.documentElement.lang === "ar";
  function meta(n, d) { var m = document.querySelector('meta[name="' + n + '"]'); return (m && m.content) || (AR ? { "nx-cancel": "إلغاء", "nx-confirm": "تأكيد" }[n] : d); }
  function ask(message, danger) {
    return new Promise(function (resolve) {
      var d = document.createElement("dialog");
      d.className = "nx-confirm"; d.setAttribute("aria-label", message);
      var p = document.createElement("p"); p.textContent = message; p.setAttribute("dir", "auto");
      var row = document.createElement("div"); row.className = "nx-confirm-actions";
      var no = document.createElement("button"); no.type = "button"; no.className = "btn"; no.textContent = meta("nx-cancel", "Cancel");
      var yes = document.createElement("button"); yes.type = "button"; yes.className = "btn btn-primary" + (danger ? " danger" : ""); yes.textContent = meta("nx-confirm", "Confirm");
      row.appendChild(no); row.appendChild(yes); d.appendChild(p); d.appendChild(row);
      var answer = false;
      function done(v) { answer = v; d.close(); }
      no.addEventListener("click", function () { done(false); });
      yes.addEventListener("click", function () { done(true); });
      d.addEventListener("click", function (e) { if (e.target === d) done(false); });
      d.addEventListener("close", function () { d.remove(); resolve(answer); });
      document.body.appendChild(d);
      d.showModal();
      (danger ? no : yes).focus(); // a destructive action never starts one Enter away from happening
    });
  }
  window.nxConfirm = ask;

  var DANGER = /delete|remove|revoke|destroy|rollback|restart|stop|prune|purge|drain|reset/i;
  document.addEventListener("htmx:confirm", function (e) {
    if (!e.detail || !e.detail.question) return;
    e.preventDefault();
    var elt = e.detail.elt, path = (e.detail.path || "") + " " + ((elt && elt.getAttribute && elt.getAttribute("hx-post")) || "");
    ask(e.detail.question, DANGER.test(path) || DANGER.test(e.detail.question)).then(function (ok) { if (ok) e.detail.issueRequest(true); });
  });

  document.addEventListener("submit", function (e) {
    var f = e.target;
    if (!f.hasAttribute || f.dataset.confirmed) return;
    var submitter = e.submitter;
    // A button may carry its own question (a form that both saves and removes), else the form's.
    var from = (submitter && submitter.hasAttribute && submitter.hasAttribute("data-confirm")) ? submitter : (f.hasAttribute("data-confirm") ? f : null);
    if (!from) return;
    e.preventDefault();
    ask(from.getAttribute("data-confirm"), from.hasAttribute("data-danger") || f.hasAttribute("data-danger")).then(function (ok) {
      if (!ok) return;
      f.dataset.confirmed = "1";
      if (f.requestSubmit) f.requestSubmit(submitter || undefined); else f.submit();
      setTimeout(function () { delete f.dataset.confirmed; }, 0);
    });
  });
})();

// A dialog whose content failed to load used to open as an empty bar with only a close button.
// Say what happened inside the dialog instead.
(function () {
  var AR = document.documentElement.lang === "ar";
  function fail(e) {
    var t = e.detail && e.detail.target;
    if (!t || t.id !== "modal-body") return;
    var status = e.detail.xhr && e.detail.xhr.status;
    var h = document.createElement("h2"), p = document.createElement("p");
    h.textContent = AR ? "تعذّر فتح النافذة" : "This dialog could not be opened";
    p.className = "sub";
    p.textContent = status === 401 || status === 403
      ? (AR ? "انتهت الجلسة أو لا تملك الصلاحية. سجّل الدخول من جديد ثم أعد المحاولة." : "Your session has ended or you do not have permission. Sign in again and retry.")
      : (AR ? "لم يستجب الخادم. أعد المحاولة بعد قليل." : "The server did not answer. Try again in a moment.");
    t.replaceChildren(h, p);
    if (typeof window.openModal === "function") window.openModal();
  }
  document.addEventListener("htmx:responseError", fail);
  document.addEventListener("htmx:sendError", fail);
})();

// Tooltips (nx-master's approach): one floating element, centred under (or above) the control, with a
// short hover delay. A tooltip that only repeats the control's own visible label ("Refresh" on a button
// that says Refresh) is noise, so it is skipped; icon-only controls and the collapsed rail still get one.
(function () {
  var tip = document.createElement("div");
  tip.className = "nx-tip"; tip.setAttribute("role", "tooltip"); tip.hidden = true;
  document.body.appendChild(tip);
  var cur = null, timer = 0;

  function norm(s) { return (s || "").replace(/\s+/g, " ").trim(); }
  function redundant(el, t) {
    if (norm(el.textContent) !== norm(t)) return false;
    var spans = el.querySelectorAll("span");
    if (!spans.length) return el.getBoundingClientRect().width > 3;
    for (var i = 0; i < spans.length; i++) {
      var r = spans[i].getBoundingClientRect();
      if (r.width > 3 && r.height > 3 && !spans[i].classList.contains("sr-only")) return true;
    }
    return false;
  }
  function place(el) {
    var rtl = getComputedStyle(el).direction === "rtl";
    var r = el.getBoundingClientRect(), w = tip.offsetWidth, h = tip.offsetHeight, gap = 8, x, y;
    if (el.closest(".sidebar")) {            // the rail's tooltips sit beside the item
      x = rtl ? r.left - gap - w : r.right + gap;
      y = r.top + r.height / 2 - h / 2;
    } else {
      var below = r.bottom + gap + h < innerHeight - 8;
      x = r.left + r.width / 2 - w / 2;
      y = below ? r.bottom + gap : r.top - gap - h;
    }
    x = Math.min(Math.max(8, x), innerWidth - w - 8);
    y = Math.min(Math.max(8, y), innerHeight - h - 8);
    tip.style.left = Math.round(x) + "px"; tip.style.top = Math.round(y) + "px";
  }
  function show(el, instant) {
    var t = el.getAttribute("data-tip");
    // The theme toggle's server-rendered tip goes stale once the theme flips in the browser; read the
    // wording for the current theme from its own hidden label instead.
    if (el.hasAttribute("data-theme-toggle")) {
      var v = el.querySelector(document.documentElement.dataset.theme === "dark" ? ".theme-dark-only .sr-only" : ".theme-light-only .sr-only");
      if (v) t = v.textContent.replace(/^\s*—\s*/, "").trim() || t;
    }
    clearTimeout(timer);
    if (!t || redundant(el, t)) return;
    cur = el;
    timer = setTimeout(function () {
      if (cur !== el) return;
      tip.textContent = t; tip.hidden = false; tip.style.left = "0px"; tip.style.top = "0px";
      place(el); tip.classList.add("on");
    }, instant ? 0 : 280);
  }
  function hide() { clearTimeout(timer); cur = null; tip.classList.remove("on"); tip.hidden = true; }

  function target(e) { var n = e.target; return n && n.closest ? n.closest("[data-tip]") : null; }
  document.addEventListener("mouseover", function (e) { var el = target(e); if (el && el !== cur) show(el, false); });
  document.addEventListener("mouseout", function (e) { var el = target(e); if (el && !el.contains(e.relatedTarget)) hide(); });
  document.addEventListener("focusin", function (e) { var el = target(e); if (el && el.matches(":focus-visible")) show(el, true); });
  document.addEventListener("focusout", hide);
  document.addEventListener("click", hide, true);
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") hide(); });
  window.addEventListener("scroll", function (e) { var n = e.target; if (cur && n && n.nodeType === 1 && !n.contains(cur)) return; hide(); }, true);
  document.addEventListener("htmx:beforeSwap", hide);
})();

/* Every code block gets a copy button. Done here, once, instead of in each template, so a new <pre>
   cannot ship without one. Rows of the live log are skipped: they are lines, not snippets. */
(function () {
  "use strict";
  function enhance(root) {
    (root.querySelectorAll ? root.querySelectorAll("pre") : []).forEach(function (pre) {
      if (pre.dataset.copyReady || pre.closest(".obs-log, .obs-log-msg, .code-wrap")) return;
      pre.dataset.copyReady = "1";
      var wrap = document.createElement("div");
      wrap.className = "code-wrap";
      pre.parentNode.insertBefore(wrap, pre);
      wrap.appendChild(pre);
      var b = document.createElement("button");
      var label = document.body.dataset.tCopy || "Copy";
      b.type = "button"; b.className = "code-copy"; b.setAttribute("aria-label", label); b.setAttribute("data-tip", label);
      b.innerHTML = '<svg class="ic" aria-hidden="true" focusable="false"><use href="#i-copy"></use></svg>';
      b.addEventListener("click", function () {
        var text = pre.innerText.replace(/\n$/, "");
        var done = function () {
          b.classList.add("done"); b.innerHTML = '<svg class="ic" aria-hidden="true" focusable="false"><use href="#i-check"></use></svg>';
          setTimeout(function () { b.classList.remove("done"); b.innerHTML = '<svg class="ic" aria-hidden="true" focusable="false"><use href="#i-copy"></use></svg>'; }, 1400);
        };
        if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, function () {});
        else { var ta = document.createElement("textarea"); ta.value = text; ta.style.cssText = "position:fixed;opacity:0"; document.body.appendChild(ta); ta.select(); try { document.execCommand("copy"); done(); } catch (e) {} ta.remove(); }
      });
      wrap.appendChild(b);
    });
  }
  document.addEventListener("DOMContentLoaded", function () { enhance(document); });
  document.addEventListener("htmx:afterSettle", function (e) { enhance(e.target || document); });
  document.addEventListener("htmx:load", function (e) { enhance(e.target || document); });
})();

/* Free-text inputs with suggestions (<input list=…>): the browser's own datalist popup is unstyleable,
   mis-positioned inside dialogs and covers the hint below it. Swap it for the site's dropdown menu.
   The input stays a plain text field: typing anything is still allowed. */
(function () {
  "use strict";
  var seq = 0;
  function enhance(root) {
    (root.querySelectorAll ? root.querySelectorAll("input[list]") : []).forEach(function (input) {
      if (input.dataset.combo) return;
      var dl = document.getElementById(input.getAttribute("list"));
      if (!dl) return;
      input.dataset.combo = "1";
      var values = Array.prototype.map.call(dl.options, function (o) { return o.value; });
      input.removeAttribute("list");
      dl.remove();
      var id = "cb" + (++seq), menu = null, active = -1, shown = [];
      input.setAttribute("role", "combobox");
      input.setAttribute("aria-autocomplete", "list");
      input.setAttribute("aria-expanded", "false");

      function close() {
        if (!menu) return;
        menu.remove(); menu = null; active = -1;
        input.setAttribute("aria-expanded", "false");
        input.removeAttribute("aria-activedescendant");
        document.removeEventListener("pointerdown", outside, true);
        window.removeEventListener("resize", close);
      }
      function outside(e) { if (menu && !menu.contains(e.target) && e.target !== input) close(); }
      function mark(i) {
        var lis = menu ? menu.children : [];
        if (active >= 0 && lis[active]) lis[active].classList.remove("active");
        active = i;
        if (i >= 0 && lis[i]) {
          lis[i].classList.add("active");
          input.setAttribute("aria-activedescendant", lis[i].id);
          lis[i].scrollIntoView({ block: "nearest" });
        }
      }
      function pick(v) {
        input.value = v; close();
        input.dispatchEvent(new Event("input", { bubbles: true }));
        input.focus();
      }
      function open() {
        var q = input.value.trim().toLowerCase();
        shown = values.filter(function (v) { return !q || v.toLowerCase().indexOf(q) !== -1; });
        // A single suggestion identical to the text adds nothing.
        if (!shown.length || (shown.length === 1 && shown[0].toLowerCase() === q)) { close(); return; }
        if (!menu) {
          menu = document.createElement("ul");
          menu.className = "dd-menu is-combo";
          menu.setAttribute("role", "listbox");
          menu.id = id;
          document.body.appendChild(menu);
          input.setAttribute("aria-expanded", "true");
          document.addEventListener("pointerdown", outside, true);
          window.addEventListener("resize", close);
        }
        menu.innerHTML = "";
        shown.forEach(function (v, i) {
          var li = document.createElement("li");
          li.className = "dd-opt mono"; li.id = id + "-" + i; li.setAttribute("role", "option"); li.textContent = v;
          li.addEventListener("pointerdown", function (e) { e.preventDefault(); pick(v); });
          menu.appendChild(li);
        });
        var r = input.getBoundingClientRect();
        menu.style.minWidth = r.width + "px";
        menu.style.left = r.left + "px";
        var h = menu.offsetHeight, below = innerHeight - r.bottom - 10;
        menu.style.maxHeight = Math.max(120, Math.min(260, below)) + "px";
        menu.style.top = r.bottom + 6 + "px";
        active = -1;
      }
      input.addEventListener("focus", open);
      input.addEventListener("click", open);
      input.addEventListener("input", open);
      input.addEventListener("blur", function () { setTimeout(close, 120); });
      input.addEventListener("keydown", function (e) {
        if (e.key === "ArrowDown" || e.key === "ArrowUp") {
          if (!menu) open();
          if (!menu) return;
          e.preventDefault();
          var n = shown.length;
          mark(active < 0 ? (e.key === "ArrowDown" ? 0 : n - 1) : (active + (e.key === "ArrowDown" ? 1 : -1) + n) % n);
        } else if (e.key === "Enter" && menu && active >= 0) {
          e.preventDefault(); pick(shown[active]);
        } else if (e.key === "Escape" && menu) {
          e.stopPropagation(); close();
        }
      });
    });
  }
  document.addEventListener("DOMContentLoaded", function () { enhance(document); });
  document.addEventListener("htmx:afterSettle", function (e) { enhance(e.target || document); });
  document.addEventListener("htmx:load", function (e) { enhance(e.target || document); });
})();
