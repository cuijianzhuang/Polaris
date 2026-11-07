(function () {
  var menu = document.querySelector('.p-user-menu'), busy = false;
  if (!menu) return;
  function fallback(image) { if (image.matches('img[data-avatar]')) image.hidden = true; }
  document.querySelectorAll('img[data-avatar]').forEach(function (image) { if (image.complete && !image.naturalWidth) fallback(image); });
  document.addEventListener('error', function (e) { if (e.target instanceof HTMLImageElement) fallback(e.target); }, true);
  function close(restoreFocus) {
    if (!menu.open) return;
    menu.open = false;
    if (restoreFocus) menu.querySelector('summary').focus();
  }
  document.addEventListener('pointerdown', function (e) { if (!menu.contains(e.target)) close(false); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || !menu.open || document.querySelector('dialog[open]')) return;
    e.preventDefault(); close(true);
  });
  menu.addEventListener('click', function (e) { if (e.target.closest('a')) close(true); });
  window.addEventListener('hashchange', function () { close(false); });
  document.addEventListener('click', function (e) {
    if (!e.target.closest('[data-portal-logout]') || busy) return;
    busy = true;
    var buttons = document.querySelectorAll('[data-portal-logout]'), error = document.getElementById('p-session-error');
    buttons.forEach(function (button) { button.disabled = true; });
    error.hidden = true;
    fetch('/u/logout', {method:'POST', headers:{'X-Polaris-Portal':'1'}, credentials:'same-origin'}).then(function (r) {
      if (!r.ok) throw new Error('退出登录失败，请稍后重试');
      location.replace('/u/login');
    }).catch(function () {
      busy = false;
      buttons.forEach(function (button) { button.disabled = false; });
      error.textContent = '退出登录失败，请检查网络后重试'; error.hidden = false;
    });
  });
})();
