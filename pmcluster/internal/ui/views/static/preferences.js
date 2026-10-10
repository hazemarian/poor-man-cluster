// Shared by the console, sign-in, and setup pages.
(function () {
  var ACCENTS = /^(orange|blue|violet|rose|cyan)$/;

  // Colors change in one frame, so a theme or accent switch never shows a
  // half-transitioned page.
  function withoutTransitions(change) {
    var freeze = document.createElement('style');
    freeze.textContent = '*,*::before,*::after{transition:none !important}';
    document.head.appendChild(freeze);
    change();
    void document.body.offsetHeight;
    requestAnimationFrame(function () {
      requestAnimationFrame(function () { freeze.remove(); });
    });
  }

  // The Settings fragment is rendered from the cookie, but localStorage can be
  // newer (the pre-paint script prefers it), so line the radios up with <html>.
  function syncAccentPicker() {
    var current = document.documentElement.dataset.accent;
    var radio = document.querySelector('input[name="pmc-accent"][value="' + current + '"]');
    if (radio) radio.checked = true;
  }
  document.addEventListener('DOMContentLoaded', syncAccentPicker);
  document.addEventListener('htmx:afterSwap', syncAccentPicker);

  document.addEventListener('change', function (event) {
    var radio = event.target.closest('input[name="pmc-accent"]');
    if (!radio || !ACCENTS.test(radio.value)) return;
    withoutTransitions(function () { document.documentElement.dataset.accent = radio.value; });
    try { localStorage.setItem('pmc_accent', radio.value); } catch (error) {}
    document.cookie = 'pmc_accent=' + radio.value + ';path=/;max-age=31536000;samesite=lax';
  });

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
    var theme = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    withoutTransitions(function () { document.documentElement.dataset.theme = theme; });
    try { localStorage.setItem('pmc_theme', theme); } catch (error) {}
    document.cookie = 'pmc_theme=' + theme + ';path=/;max-age=31536000;samesite=lax';
  });
})();
