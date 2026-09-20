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
