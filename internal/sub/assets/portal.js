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
  function options(select,key){Array.from(new Set(rows.map(function(r){return r.dataset[key]}))).filter(Boolean).sort().forEach(function(value){var option=document.createElement('option');option.value=value;option.textContent=value;select.appendChild(option)})}
  options(region,'region');options(protocol,'protocol');
  function filter(){var q=search.value.trim().toLowerCase(),visible=0;rows.forEach(function(r){var show=(!q||r.querySelector('.p-node-name').textContent.toLowerCase().includes(q))&&(!region.value||r.dataset.region===region.value)&&(!protocol.value||r.dataset.protocol===protocol.value)&&(!state.value||r.dataset.state===state.value);r.hidden=!show;if(show)visible++});document.getElementById('p-node-empty').hidden=visible>0}
  [search,region,protocol,state].forEach(function(input){input.addEventListener(input===search?'input':'change',filter)});
  var counts={verified:0,unknown:0,unavailable:0};rows.forEach(function(r){counts[r.dataset.state in counts?r.dataset.state:'unknown']++});
  document.getElementById('p-node-counts').textContent='已验证 '+counts.verified+' · 未验证 '+counts.unknown+' · 暂不可用 '+counts.unavailable;
  var dialog=document.getElementById('p-node-dialog'),lastDetail=null;
  document.addEventListener('click',function(e){var b=e.target.closest('[data-node-detail]');if(!b)return;lastDetail=b;var row=b.closest('.p-node-row'),badge=row.querySelector('.p-badge'),qr=row.querySelector('.p-node-qr');document.getElementById('p-dialog-title').textContent=row.querySelector('.p-node-name b').textContent;var target=document.getElementById('p-dialog-state');target.textContent=badge.textContent;target.className=badge.className;document.getElementById('p-dialog-reason').textContent=row.querySelector('.p-node-reason').textContent;var at=row.querySelector('.p-node-time').textContent;document.getElementById('p-dialog-time').textContent=at?'最近检查：'+at:'暂无近期业务验证记录';var image=document.getElementById('p-dialog-qr');image.hidden=!qr;if(qr)image.src=qr.src;document.getElementById('p-dialog-copy').setAttribute('data-copy',row.querySelector('[data-copy]').getAttribute('data-copy'));dialog.showModal()});
  dialog.querySelector('.p-close').addEventListener('click',function(){dialog.close()});dialog.addEventListener('close',function(){if(lastDetail)lastDetail.focus()});dialog.addEventListener('click',function(e){if(e.target===dialog){var r=dialog.getBoundingClientRect();if(e.clientX<r.left||e.clientX>r.right||e.clientY<r.top||e.clientY>r.bottom)dialog.close()}});
  var ua=navigator.userAgent,device=/Android/i.test(ua)?'Android':/iPhone|iPad|iPod/i.test(ua)?'iOS':/Macintosh/i.test(ua)?'macOS':/Windows/i.test(ua)?'Windows':'';
  if(device){document.getElementById('p-platform-tip').textContent='当前设备：'+device+'。请选择已安装、且支持订阅中协议的客户端。';document.getElementById('p-device-guide').textContent=device+' 使用指引'}
})();
