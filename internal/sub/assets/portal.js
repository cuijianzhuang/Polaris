(function () {
  var titles={overview:'仪表盘',subscription:'我的订阅',nodes:'可用节点',preferences:'高级订阅设置',orders:'我的订单',account:'个人中心',help:'使用教程'};
  function navigate() {
    var hash=location.hash.slice(1),view=hash==='cdn-preferences'?'preferences':hash==='qr'?'subscription':hash;
    if(!titles[view])view='overview';
    document.querySelectorAll('[data-view]').forEach(function(p){p.hidden=p.dataset.view!==view});
    var parent=view;if(view==='orders'||(view==='help'&&!document.querySelector('.p-bottom [data-view-link=help]')))parent='account';if(view==='preferences'||(view==='nodes'&&!document.querySelector('.p-bottom [data-view-link=nodes]')))parent='subscription';
    document.querySelectorAll('[data-view-link]').forEach(function(a){var selected=a.closest('.p-bottom')?a.dataset.viewLink===parent:a.dataset.viewLink===view;if(selected)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current')});
    document.getElementById('p-current-title').textContent=titles[view];
    document.title=titles[view]+' · 用户中心';
    if(hash==='qr'){document.getElementById('p-qr').open=true;document.getElementById('p-qr').scrollIntoView({block:'center'})}
    else window.scrollTo(0,0);
  }
  window.addEventListener('hashchange',navigate);navigate();
  var search=document.getElementById('p-node-search'),region=document.getElementById('p-node-region'),protocol=document.getElementById('p-node-protocol'),state=document.getElementById('p-node-state'),rows=Array.from(document.querySelectorAll('.p-node-row'));
  var dialog=document.getElementById('p-node-dialog'),lastDetail=null,lastRow=null,refreshing=false;
  var nodeLabels={online:'在线',offline:'离线',unknown:'未上报'},networkLabels={passed:'检查通过',failed:'检查失败',stale:'待复检',unknown:'尚未检查'},businessLabels={passed:'认证与传输通过',failed:'最近验证失败',stale:'验证已过期',unknown:'尚未验证'};
  function options(select,key){Array.from(new Set(rows.map(function(r){return r.dataset[key]}))).filter(Boolean).sort().forEach(function(value){var option=document.createElement('option');option.value=value;option.textContent=value;select.appendChild(option)})}
  options(region,'region');options(protocol,'protocol');
  function filter(){var q=search.value.trim().toLowerCase(),visible=0;rows.forEach(function(r){var show=(!q||r.querySelector('.p-node-name').textContent.toLowerCase().includes(q))&&(!region.value||r.dataset.region===region.value)&&(!protocol.value||r.dataset.protocol===protocol.value)&&(!state.value||r.dataset.state===state.value);r.hidden=!show;if(show)visible++});document.getElementById('p-node-empty').hidden=visible>0}
  [search,region,protocol,state].forEach(function(input){input.addEventListener(input===search?'input':'change',filter)});
  function counts(){var totals={verified:0,reachable:0,unknown:0,unavailable:0};rows.forEach(function(r){totals[r.dataset.state in totals?r.dataset.state:'unknown']++});document.getElementById('p-node-counts').textContent='业务已验证 '+totals.verified+' · 连接通过 '+totals.reachable+' · 待检查 '+totals.unknown+' · 暂不可用 '+totals.unavailable;document.querySelector('.p-node-summary>b').textContent='共 '+rows.length+' 个节点'}
  counts();
  function checkedTime(value){var date=new Date(value);return value&&Number.isFinite(date.getTime())?date.toLocaleString():value}
  function showDetails(row,preserve){
    lastRow=row;
    var badge=row.querySelector('.p-badge'),qr=row.querySelector('.p-node-qr');
    document.getElementById('p-dialog-title').textContent=row.querySelector('.p-node-name b').textContent;
    var target=document.getElementById('p-dialog-state');target.textContent=badge.textContent;target.className='p-badge '+row.dataset.state;
    document.getElementById('p-dialog-reason').textContent=row.querySelector('.p-node-reason').textContent;
    var at=row.querySelector('.p-node-time').textContent;document.getElementById('p-dialog-time').textContent=at?'最近检查：'+checkedTime(at):'暂无近期检查记录';
    document.getElementById('p-proof-node').textContent=nodeLabels[row.dataset.online]||'未上报';document.getElementById('p-proof-network').textContent=networkLabels[row.dataset.network]||'尚未检查';document.getElementById('p-proof-business').textContent=businessLabels[row.dataset.business]||'尚未验证';document.getElementById('p-proof-scope').textContent=row.dataset.scope||'暂无近期检查';
    var result=document.getElementById('p-dialog-check-result');result.textContent=row.dataset.entryCheck||'';result.hidden=!result.textContent;
    var code=document.getElementById('p-dialog-code');if(!preserve)code.open=false;code.hidden=!qr;
    var image=document.getElementById('p-dialog-qr');if(qr)image.src=qr.src;else image.removeAttribute('src');
    document.getElementById('p-dialog-copy').setAttribute('data-copy',row.querySelector('[data-copy]').getAttribute('data-copy'));
  }
  document.addEventListener('click',function(e){var button=e.target.closest('[data-node-detail]');if(!button)return;lastDetail=button;showDetails(button.closest('.p-node-row'));dialog.showModal()});
  dialog.querySelector('.p-close').addEventListener('click',function(){dialog.close()});dialog.addEventListener('close',function(){document.getElementById('p-dialog-code').open=false;if(lastDetail&&lastDetail.isConnected)lastDetail.focus();lastRow=null});dialog.addEventListener('click',function(e){if(e.target===dialog){var rect=dialog.getBoundingClientRect();if(e.clientX<rect.left||e.clientX>rect.right||e.clientY<rect.top||e.clientY>rect.bottom)dialog.close()}});
  function refreshNodes(){
    if(refreshing)return;refreshing=true;
    var button=document.getElementById('p-nodes-refresh'),error=document.getElementById('p-nodes-error'),controller=new AbortController(),timeout=setTimeout(function(){controller.abort()},12000);
    button.disabled=true;error.hidden=true;
    fetch('/u/nodes/status',{credentials:'same-origin',cache:'no-store',signal:controller.signal}).then(function(response){if(response.status===401){location.replace('/u/login');throw new Error('请重新登录')}if(!response.ok)throw new Error('无法更新状态');return response.json()}).then(function(data){
      if(!Array.isArray(data.items))throw new Error('状态响应无效');
      var available=new Map(data.items.map(function(item){return [item.key,item]})),oldKeys=new Set(rows.map(function(row){return row.dataset.nodeKey}));
      rows=rows.filter(function(row){
        var item=available.get(row.dataset.nodeKey);if(!item){if(lastRow===row)dialog.close();row.remove();return false}
        row.dataset.state=['verified','reachable','unknown','unavailable'].includes(item.state)?item.state:'unknown';row.dataset.online=item.online;row.dataset.network=item.network;row.dataset.business=item.business;row.dataset.scope=item.scope;
        var badge=row.querySelector('.p-badge');badge.className='p-badge p-node-state '+row.dataset.state;badge.textContent=item.label;badge.setAttribute('aria-label','查看'+item.label+'说明');
        row.querySelector('.p-node-reason').textContent=item.reason;row.querySelector('.p-node-time').textContent=item.checkedAt||'';
        row.querySelector('.p-text-button').textContent=item.state==='unavailable'?'查看原因':'查看详情';return true;
      });
      document.getElementById('p-node-list-change').hidden=!data.items.some(function(item){return !oldKeys.has(item.key)});
      counts();filter();if(lastRow&&lastRow.isConnected)showDetails(lastRow,true);
      document.getElementById('p-nodes-updated').textContent='状态更新于 '+checkedTime(data.at)+' · 后台检查记录，不是本地测速';
    }).catch(function(){error.textContent='节点状态更新失败，已保留上次结果，请重试';error.hidden=false}).finally(function(){clearTimeout(timeout);button.disabled=false;refreshing=false});
  }
  var checkingEntry=false,checkButton=document.getElementById('p-dialog-check'),autoCheck=document.getElementById('p-nodes-auto'),autoStatus=document.getElementById('p-auto-check-status'),autoPausedUntil=0;
  try{autoCheck.checked=localStorage.getItem('polaris.portal.autoCheck')!=='false'}catch(_){autoCheck.checked=true}
  function runEntryCheck(row,automatic){
    if(checkingEntry||!row)return;
    var result=document.getElementById('p-dialog-check-result'),controller=new AbortController(),timeout=setTimeout(function(){controller.abort()},8000);
    checkingEntry=true;row.dataset.entryAttempt=String(Date.now());checkButton.disabled=true;checkButton.textContent='检查中…';
    if(!automatic){result.hidden=false;result.textContent='正在从面板检查订阅入口的 TCP／TLS 连接…'}
    else autoStatus.textContent='自动检查中：'+row.querySelector('.p-node-name b').textContent;
    fetch('/u/nodes/check',{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},credentials:'same-origin',cache:'no-store',signal:controller.signal,body:JSON.stringify({key:row.dataset.nodeKey})}).then(function(r){return r.json().then(function(data){if(!r.ok){if(r.status===429)autoPausedUntil=Date.now()+30000;if(r.status===401)location.replace('/u/login');throw new Error(data.error||'检查失败，请重试')}return data})}).then(function(data){
      row.dataset.entryCheck=data.message+'（'+checkedTime(data.checkedAt)+(data.state==='passed'?' · '+data.latencyMs+' ms':'')+'）';
      if(lastRow===row){result.textContent=row.dataset.entryCheck;result.hidden=false}
      if(automatic)autoStatus.textContent='最近自动检查：'+row.querySelector('.p-node-name b').textContent+' · '+(data.state==='passed'?'连接通过':data.state==='unsupported'?'检查不适用':'连接失败');
      refreshNodes();
    }).catch(function(error){
      var message=error.name==='AbortError'?'检查超时，请稍后重试':error.message;
      if(lastRow===row){result.textContent=message;result.hidden=false}
      if(automatic)autoStatus.textContent=message;
    }).finally(function(){clearTimeout(timeout);checkingEntry=false;checkButton.disabled=false;checkButton.textContent='检查连接'});
  }
  checkButton.addEventListener('click',function(){runEntryCheck(lastRow,false)});
  function autoTick(){
    if(!autoCheck.checked||document.hidden||location.hash!=='#nodes'||dialog.open||checkingEntry||Date.now()<autoPausedUntil)return;
    var row=rows.filter(function(row){return !row.hidden&&Date.now()-Number(row.dataset.entryAttempt||0)>=300000}).sort(function(a,b){return Number(a.dataset.entryAttempt||0)-Number(b.dataset.entryAttempt||0)})[0];
    if(row)runEntryCheck(row,true);
  }
  autoCheck.addEventListener('change',function(){try{localStorage.setItem('polaris.portal.autoCheck',String(autoCheck.checked))}catch(_){}autoStatus.textContent=autoCheck.checked?'自动检查已开启，每 12 秒检查一个入口，5 分钟内不重复检查。':'自动检查已暂停，仍可在节点详情中手动检查。';if(autoCheck.checked)autoTick()});
  setInterval(autoTick,12000);
  document.getElementById('p-nodes-refresh').addEventListener('click',refreshNodes);
  function nodesVisible(){if(location.hash==='#nodes'&&!document.hidden&&!dialog.open)refreshNodes()}
  window.addEventListener('hashchange',nodesVisible);document.addEventListener('visibilitychange',nodesVisible);setInterval(nodesVisible,30000);nodesVisible();
  var ua=navigator.userAgent,device=/Android/i.test(ua)?'Android':/iPhone|iPad|iPod/i.test(ua)?'iOS':/Macintosh/i.test(ua)?'macOS':/Windows/i.test(ua)?'Windows':'';
  if(device){document.getElementById('p-platform-tip').textContent='当前设备：'+device+'。请选择已安装、且支持订阅中协议的客户端。';document.getElementById('p-device-guide').textContent=device+' 使用指引'}
})();
