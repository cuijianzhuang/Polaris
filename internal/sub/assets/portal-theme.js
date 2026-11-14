(function () {
  var root = document.documentElement;
  var key = 'polaris.theme';
  var media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  var preference = readPreference();

  function readPreference() {
    try {
      var value = JSON.parse(localStorage.getItem(key) || '{}');
      if (value && (value.mode === 'light' || value.mode === 'dark' || value.mode === 'system')) return value;
    } catch (_) {}
    return { mode: 'system' };
  }

  function applyTheme() {
    var dark = preference.mode === 'dark' || (preference.mode === 'system' && !!(media && media.matches));
    root.dataset.mode = dark ? 'dark' : 'light';
    document.querySelectorAll('[data-theme-toggle]').forEach(function (button) {
      var target = dark ? 'light' : 'dark';
      var label = target === 'dark' ? '深色模式' : '浅色模式';
      var text = button.querySelector('[data-theme-label]');
      var icon = button.querySelector('use');
      if (text) text.textContent = label;
      else button.textContent = label;
      if (icon) icon.setAttribute('href', target === 'dark' ? '#pi-moon' : '#pi-sun');
      button.setAttribute('aria-label', '切换到' + label);
      button.setAttribute('title', '切换到' + label);
    });
  }

  function connectButtons() {
    document.querySelectorAll('[data-theme-toggle]').forEach(function (button) {
      button.addEventListener('click', function () {
        preference = readPreference();
        preference.mode = root.dataset.mode === 'dark' ? 'light' : 'dark';
        try { localStorage.setItem(key, JSON.stringify(preference)); } catch (_) {}
        applyTheme();
      });
    });
    applyTheme();
  }

  applyTheme();
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', connectButtons, { once: true });
  else connectButtons();
  if (media) {
    var onSystemThemeChange = function () { if (preference.mode === 'system') applyTheme(); };
    if (media.addEventListener) media.addEventListener('change', onSystemThemeChange);
    else if (media.addListener) media.addListener(onSystemThemeChange);
  }
})();
