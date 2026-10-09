// Shared by the console, sign-in, and setup pages.
(function () {
  document.addEventListener('click', function (event) {
    var languageLink = event.target.closest('[data-language-switch]');
    if (languageLink) {
      var url = new URL(languageLink.href);
      // HTMX changes the current route without rendering the shell again.
      // Keep filters too, and let the destination language cookie take effect.
      var next = new URL(location.href);
      next.searchParams.delete('lang');
      url.searchParams.set('next', next.pathname + next.search);
      languageLink.href = url.pathname + url.search;
    }
    if (!event.target.closest('[data-theme-toggle]')) return;
    var freeze = document.createElement('style');
    freeze.textContent = '*,*::before,*::after{transition:none !important}';
    document.head.appendChild(freeze);
    var theme = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = theme;
    try { localStorage.setItem('pmc_theme', theme); } catch (error) {}
    document.cookie = 'pmc_theme=' + theme + ';path=/;max-age=31536000;samesite=lax';
    void document.body.offsetHeight;
    requestAnimationFrame(function () {
      requestAnimationFrame(function () { freeze.remove(); });
    });
  });
})();

// Brand palette: six accents that recolour the logo mark + wordmark. The boot
// script in the document head already applied the persisted palette before
// first paint; this section owns the picker interaction and re-syncs the
// control after a change or a reload.
(function () {
  var PALETTES = ['orange', 'purple', 'blue', 'red', 'green', 'teal'];
  var KEY = 'pmc_palette';

  function valid(p) { return PALETTES.indexOf(p) !== -1; }
  function stored() {
    try {
      var p = localStorage.getItem(KEY);
      if (valid(p)) return p;
    } catch (error) {}
    return 'orange';
  }
  function apply(p) {
    if (valid(p)) document.documentElement.dataset.palette = p;
  }

  function sync() {
    var p = stored();
    document.querySelectorAll('[data-palette-choose]').forEach(function (btn) {
      var on = btn.getAttribute('data-palette-choose') === p;
      btn.classList.toggle('is-active', on);
      btn.setAttribute('aria-checked', on ? 'true' : 'false');
      if (on) {
        var name = btn.querySelector('.palette-opt-name');
        var label = document.querySelector('[data-palette-current]');
        if (name && label) label.textContent = name.textContent;
      }
    });
  }

  function setPop(picker, open) {
    var pop = picker.querySelector('.palette-pop');
    var btn = picker.querySelector('[data-palette-toggle]');
    if (pop) pop.hidden = !open;
    if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  }

  document.addEventListener('click', function (event) {
    var picker = event.target.closest('[data-palette-picker]');
    if (!picker) {
      document.querySelectorAll('[data-palette-picker]').forEach(function (p) { setPop(p, false); });
      return;
    }
    var toggle = event.target.closest('[data-palette-toggle]');
    if (toggle) {
      var pop = picker.querySelector('.palette-pop');
      setPop(picker, pop && pop.hidden);
      return;
    }
    var choose = event.target.closest('[data-palette-choose]');
    if (choose) {
      var p = choose.getAttribute('data-palette-choose');
      apply(p);
      try { localStorage.setItem(KEY, p); } catch (error) {}
      setPop(picker, false);
      sync();
    }
  });

  document.addEventListener('keydown', function (event) {
    if (event.key === 'Escape') {
      document.querySelectorAll('[data-palette-picker]').forEach(function (p) { setPop(p, false); });
    }
  });

  apply(stored());
  sync();
})();
