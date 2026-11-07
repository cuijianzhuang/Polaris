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
  function showDetails(row){
    lastRow=row;
    var badge=row.querySelector('.p-badge'),qr=row.querySelector('.p-node-qr');
    document.getElementById('p-dialog-title').textContent=row.querySelector('.p-node-name b').textContent;
    var target=document.getElementById('p-dialog-state');target.textContent=badge.textContent;target.className='p-badge '+row.dataset.state;
    document.getElementById('p-dialog-reason').textContent=row.querySelector('.p-node-reason').textContent;
    var at=row.querySelector('.p-node-time').textContent;document.getElementById('p-dialog-time').textContent=at?'最近检查：'+checkedTime(at):'暂无近期检查记录';
    document.getElementById('p-proof-node').textContent=nodeLabels[row.dataset.online]||'未上报';document.getElementById('p-proof-network').textContent=networkLabels[row.dataset.network]||'尚未检查';document.getElementById('p-proof-business').textContent=businessLabels[row.dataset.business]||'尚未验证';document.getElementById('p-proof-scope').textContent=row.dataset.scope||'暂无近期检查';
    var code=document.getElementById('p-dialog-code');code.open=false;code.hidden=!qr;
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
      counts();filter();if(lastRow&&lastRow.isConnected)showDetails(lastRow);
      document.getElementById('p-nodes-updated').textContent='状态更新于 '+checkedTime(data.at)+' · 后台检查记录，不是本地测速';
    }).catch(function(){error.textContent='节点状态更新失败，已保留上次结果，请重试';error.hidden=false}).finally(function(){clearTimeout(timeout);button.disabled=false;refreshing=false});
  }
  document.getElementById('p-nodes-refresh').addEventListener('click',refreshNodes);
  function nodesVisible(){if(location.hash==='#nodes'&&!document.hidden&&!dialog.open)refreshNodes()}
  window.addEventListener('hashchange',nodesVisible);document.addEventListener('visibilitychange',nodesVisible);setInterval(nodesVisible,30000);nodesVisible();
  var ua=navigator.userAgent,device=/Android/i.test(ua)?'Android':/iPhone|iPad|iPod/i.test(ua)?'iOS':/Macintosh/i.test(ua)?'macOS':/Windows/i.test(ua)?'Windows':'';
  if(device){document.getElementById('p-platform-tip').textContent='当前设备：'+device+'。请选择已安装、且支持订阅中协议的客户端。';document.getElementById('p-device-guide').textContent=device+' 使用指引'}
})();
