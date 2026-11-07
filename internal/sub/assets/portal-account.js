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
  function accountRequest(path,method,body) {
    var controller=new AbortController(),timeout=setTimeout(function(){controller.abort()},12000);
    return fetch('/u/'+path,{method:method||'GET',headers:{'X-Polaris-Portal':'1','Content-Type':'application/json'},body:body?JSON.stringify(body):undefined,credentials:'same-origin',cache:'no-store',signal:controller.signal}).then(function(r){
      return r.json().catch(function(){return {}}).then(function(j){if(!r.ok){var error=new Error(j.error||'操作失败，请重试');error.status=r.status;throw error}return j});
    }).finally(function(){clearTimeout(timeout)});
  }
  var sessionList=document.getElementById('p-sessions-list'),sessionBusy=false;
  function sessionError(message){var el=document.getElementById('p-sessions-error');el.textContent=message;el.hidden=!message}
  function sessionLock(locked){sessionBusy=locked;document.getElementById('p-sessions').setAttribute('aria-busy',String(locked));document.querySelectorAll('#p-sessions button').forEach(function(button){button.disabled=locked||button.dataset.empty==='true'})}
  function renderSessions(data){
    sessionList.replaceChildren();var items=data.items||[],hasOthers=items.some(function(item){return !item.current})||data.hasMore;
    items.forEach(function(item){
      var row=document.createElement('div');row.className='p-session-row';
      var info=document.createElement('div'),title=document.createElement('b'),detail=document.createElement('p');
      title.textContent=item.device||'未知设备';detail.className='mute';
      detail.textContent=(item.via==='email'?'邮箱登录':'Telegram 登录')+' · '+(item.ip||'地址未记录')+' · 最近活动 '+new Date(item.lastSeen).toLocaleString();
      info.append(title,detail);row.append(info);
      if(item.current){var badge=document.createElement('span');badge.className='p-badge verified';badge.textContent='当前设备';row.append(badge)}
      var button=document.createElement('button');button.type='button';button.className='btn alt sm';button.dataset.sessionId=item.id;button.textContent=item.current?'退出当前设备':'退出此设备';row.append(button);sessionList.append(row);
    });
    if(!items.length){var empty=document.createElement('p');empty.className='mute';empty.textContent='暂无登录设备记录';sessionList.append(empty)}
    document.getElementById('p-sessions-more').hidden=!data.hasMore;
    document.getElementById('p-sessions-others').dataset.empty=String(!hasOthers);
  }
  function loadSessions(){
    if(!sessionList||sessionBusy)return;sessionLock(true);sessionError('');
    accountRequest('sessions').then(renderSessions).catch(function(error){if(error.status===401){location.replace('/u/login');return}sessionError('登录设备加载失败，请重试')}).finally(function(){sessionLock(false)});
  }
  if(sessionList){
    document.getElementById('p-sessions-refresh').addEventListener('click',loadSessions);
    document.getElementById('p-sessions').addEventListener('click',function(event){
      var button=event.target.closest('button'),others=button&&button.id==='p-sessions-others';
      if(!button||sessionBusy||(!others&&!button.dataset.sessionId))return;
      if(!confirm(others?'退出全部其他用户中心登录设备？订阅和代理连接继续有效。':'退出这个用户中心登录设备？'))return;
      sessionLock(true);sessionError('');
      accountRequest(others?'sessions/revoke-others':'sessions/revoke','POST',others?{}:{id:button.dataset.sessionId}).then(function(data){if(data.current){location.replace('/u/login');return}return accountRequest('sessions').then(renderSessions)}).catch(function(error){if(error.status===401){location.replace('/u/login');return}sessionError('退出设备失败，请检查网络后重试')}).finally(function(){sessionLock(false)});
    });
    function accountVisible(){if(location.hash==='#account'&&!document.hidden)loadSessions()}
    window.addEventListener('hashchange',accountVisible);document.addEventListener('visibilitychange',accountVisible);setInterval(function(){if(!document.getElementById('p-sessions').contains(document.activeElement))accountVisible()},30000);accountVisible();
  }
  document.addEventListener('click', function (e) {
    if (!e.target.closest('[data-portal-logout]') || busy) return;
    busy = true;
    var buttons = document.querySelectorAll('[data-portal-logout]'), error = document.getElementById('p-session-error');
    buttons.forEach(function (button) { button.disabled = true; });
    error.hidden = true;
    accountRequest('logout','POST',{}).then(function () {
      location.replace('/u/login');
    }).catch(function () {
      busy = false;
      buttons.forEach(function (button) { button.disabled = false; });
      error.textContent = '退出登录失败，请检查网络后重试'; error.hidden = false;
    });
  });
})();
