(function () {
  var root = document.documentElement;
  var key = 'polaris.theme';
  var media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  var preference = readPreference();

  function readPreference() {
    try {
      var raw = localStorage.getItem(key);
      if (raw) {
        var value = JSON.parse(raw);
        // The public status page stores the same object; older builds stored a
        // bare "light"/"dark" string there, which is still honoured.
        if (value && (value.mode === 'light' || value.mode === 'dark' || value.mode === 'system')) return value;
        if (value === 'light' || value === 'dark') return { mode: value };
      }
    } catch (_) {}
    return { mode: 'system' };
  }

  function applyTheme() {
    var dark = preference.mode === 'dark' || (preference.mode === 'system' && !!(media && media.matches));
    root.dataset.mode = dark ? 'dark' : 'light';
    // Labels come from the template's string table so the button reads in the
    // visitor's language; the Chinese text is only a fallback.
    var table = window.__portalI18n || {};
    function s(key, args) { var v = table[key] || ''; if (args) for (var k in args) v = v.split('{' + k + '}').join(args[k]); return v; }
    document.querySelectorAll('[data-theme-toggle]').forEach(function (button) {
      var target = dark ? 'light' : 'dark';
      var label = target === 'dark' ? (s('themeDark') || '深色模式') : (s('themeLight') || '浅色模式');
      var text = button.querySelector('[data-theme-label]');
      var icon = button.querySelector('use');
      if (text) text.textContent = label;
      else button.textContent = label;
      if (icon) icon.setAttribute('href', target === 'dark' ? '#pi-moon' : '#pi-sun');
      var hint = s('switchTo', { mode: label }) || ('切换到' + label);
      button.setAttribute('aria-label', hint);
      button.setAttribute('title', hint);
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
