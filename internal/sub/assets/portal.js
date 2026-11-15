(function () {
  // Runtime strings come from the template (window.__portalI18n): the static
  // chrome is translated server side, but labels built here were Chinese only.
  var table=window.__portalI18n||{};
  function s(key,args){var v=table[key]||'';if(args)for(var k in args)v=v.split('{'+k+'}').join(args[k]);return v}
  var titles={overview:s('viewOverview'),subscription:s('viewSubscription'),nodes:s('viewNodes'),preferences:s('viewPreferences'),orders:s('viewOrders'),account:s('viewAccount'),help:s('viewHelp')};
  function navigate() {
    var hash=location.hash.slice(1),view=hash==='cdn-preferences'?'preferences':hash==='qr'?'subscription':hash;
    if(!titles[view])view='overview';
    document.querySelectorAll('[data-view]').forEach(function(p){p.hidden=p.dataset.view!==view});
    var parent=view;if(view==='orders'||(view==='help'&&!document.querySelector('.p-bottom [data-view-link=help]')))parent='account';if(view==='preferences'||(view==='nodes'&&!document.querySelector('.p-bottom [data-view-link=nodes]')))parent='subscription';
    document.querySelectorAll('[data-view-link]').forEach(function(a){var selected=a.closest('.p-bottom')?a.dataset.viewLink===parent:a.dataset.viewLink===view;if(selected)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current')});
    document.getElementById('p-current-title').textContent=titles[view];
    document.title=titles[view]+' · '+s('userCenterTitle');
    if(hash==='qr'){document.getElementById('p-qr').open=true;document.getElementById('p-qr').scrollIntoView({block:'center'})}
    else window.scrollTo(0,0);
  }
  window.addEventListener('hashchange',navigate);navigate();
  var search=document.getElementById('p-node-search'),region=document.getElementById('p-node-region'),protocol=document.getElementById('p-node-protocol'),state=document.getElementById('p-node-state'),rows=Array.from(document.querySelectorAll('.p-node-row'));
  var dialog=document.getElementById('p-node-dialog'),lastDetail=null,lastRow=null,refreshing=false;
  var nodeLabels={online:s('nodeOnline'),offline:s('nodeOffline'),unknown:s('nodeUnknown')},networkLabels={passed:s('proofPassed'),failed:s('proofFailed'),stale:s('proofStale'),unknown:s('proofUnknown')},businessLabels={passed:s('bizPassed'),failed:s('bizFailed'),stale:s('bizStale'),unknown:s('bizUnknown')};
  function options(select,key){Array.from(new Set(rows.map(function(r){return r.dataset[key]}))).filter(Boolean).sort().forEach(function(value){var option=document.createElement('option');option.value=value;option.textContent=value;select.appendChild(option)})}
  options(region,'region');options(protocol,'protocol');
  function filter(){var q=search.value.trim().toLowerCase(),visible=0;rows.forEach(function(r){var show=(!q||r.querySelector('.p-node-name').textContent.toLowerCase().includes(q))&&(!region.value||r.dataset.region===region.value)&&(!protocol.value||r.dataset.protocol===protocol.value)&&(!state.value||r.dataset.state===state.value);r.hidden=!show;if(show)visible++});document.getElementById('p-node-empty').hidden=visible>0}
  [search,region,protocol,state].forEach(function(input){input.addEventListener(input===search?'input':'change',filter)});
  function counts(){var totals={verified:0,reachable:0,unknown:0,unavailable:0};rows.forEach(function(r){totals[r.dataset.state in totals?r.dataset.state:'unknown']++});document.getElementById('p-node-counts').textContent=[[s('countBiz'),totals.verified],[s('countReachable'),totals.reachable],[s('countUnknown'),totals.unknown],[s('countUnavailable'),totals.unavailable]].map(function(p){return p[0]+' '+p[1]}).join(s('countJoin'));document.querySelector('.p-node-summary>b').textContent=s('nodeTotal',{n:rows.length})}
  counts();
  function checkedTime(value){var date=new Date(value);return value&&Number.isFinite(date.getTime())?date.toLocaleString():value}
  function showDetails(row,preserve){
    lastRow=row;
    var badge=row.querySelector('.p-badge'),qr=row.querySelector('.p-node-qr');
    document.getElementById('p-dialog-title').textContent=row.querySelector('.p-node-name b').textContent;
    var target=document.getElementById('p-dialog-state');target.textContent=badge.textContent;target.className='p-badge '+row.dataset.state;
    document.getElementById('p-dialog-reason').textContent=row.querySelector('.p-node-reason').textContent;
    var at=row.querySelector('.p-node-time').textContent;document.getElementById('p-dialog-time').textContent=at?s('lastCheck',{time:checkedTime(at)}):s('noRecentCheck');
    document.getElementById('p-proof-node').textContent=nodeLabels[row.dataset.online]||s('nodeUnknown');document.getElementById('p-proof-network').textContent=networkLabels[row.dataset.network]||s('proofUnknown');document.getElementById('p-proof-business').textContent=businessLabels[row.dataset.business]||s('bizUnknown');document.getElementById('p-proof-scope').textContent=row.dataset.scope||s('noRecentCheck');
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
    fetch('/u/nodes/status',{credentials:'same-origin',cache:'no-store',signal:controller.signal}).then(function(response){if(response.status===401){location.replace('/u/login');throw new Error(s('pleaseSignIn'))}if(!response.ok)throw new Error(s('cannotUpdateStatus'));return response.json()}).then(function(data){
      if(!Array.isArray(data.items))throw new Error(s('invalidStatusResponse'));
      var available=new Map(data.items.map(function(item){return [item.key,item]})),oldKeys=new Set(rows.map(function(row){return row.dataset.nodeKey}));
      rows=rows.filter(function(row){
        var item=available.get(row.dataset.nodeKey);if(!item){if(lastRow===row)dialog.close();row.remove();return false}
        row.dataset.state=['verified','reachable','unknown','unavailable'].includes(item.state)?item.state:'unknown';row.dataset.online=item.online;row.dataset.network=item.network;row.dataset.business=item.business;row.dataset.scope=item.scope;
        var badge=row.querySelector('.p-badge');badge.className='p-badge p-node-state '+row.dataset.state;badge.textContent=item.label;badge.setAttribute('aria-label',s('viewAbout',{name:item.label}));
        row.querySelector('.p-node-reason').textContent=item.reason;row.querySelector('.p-node-time').textContent=item.checkedAt||'';
        row.querySelector('.p-text-button').textContent=item.state==='unavailable'?s('viewReason'):s('viewDetail');return true;
      });
      document.getElementById('p-node-list-change').hidden=!data.items.some(function(item){return !oldKeys.has(item.key)});
      counts();filter();if(lastRow&&lastRow.isConnected)showDetails(lastRow,true);
      document.getElementById('p-nodes-updated').textContent=s('statusUpdatedAt',{time:checkedTime(data.at)});
    }).catch(function(){error.textContent=s('statusUpdateFailed');error.hidden=false}).finally(function(){clearTimeout(timeout);button.disabled=false;refreshing=false});
  }
  var checkingEntry=false,checkButton=document.getElementById('p-dialog-check'),autoCheck=document.getElementById('p-nodes-auto'),autoStatus=document.getElementById('p-auto-check-status'),autoPausedUntil=0;
  try{autoCheck.checked=localStorage.getItem('polaris.portal.autoCheck')!=='false'}catch(_){autoCheck.checked=true}
  function runEntryCheck(row,automatic){
    if(checkingEntry||!row)return;
    var result=document.getElementById('p-dialog-check-result'),controller=new AbortController(),timeout=setTimeout(function(){controller.abort()},8000);
    checkingEntry=true;row.dataset.entryAttempt=String(Date.now());checkButton.disabled=true;checkButton.textContent=s('checking');
    if(!automatic){result.hidden=false;result.textContent=s('checkProbing')}
    else autoStatus.textContent=s('autoCheckRunning',{name:row.querySelector('.p-node-name b').textContent});
    fetch('/u/nodes/check',{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},credentials:'same-origin',cache:'no-store',signal:controller.signal,body:JSON.stringify({key:row.dataset.nodeKey})}).then(function(r){return r.json().then(function(data){if(!r.ok){if(r.status===429)autoPausedUntil=Date.now()+30000;if(r.status===401)location.replace('/u/login');throw new Error(data.error||s('checkFailedRetry'))}return data})}).then(function(data){
      row.dataset.entryCheck=data.message+'（'+checkedTime(data.checkedAt)+(data.state==='passed'?' · '+data.latencyMs+' ms':'')+'）';
      if(lastRow===row){result.textContent=row.dataset.entryCheck;result.hidden=false}
      if(automatic)autoStatus.textContent=s('autoCheckLast',{name:row.querySelector('.p-node-name b').textContent,state:data.state==='passed'?s('checkPassed'):data.state==='unsupported'?s('checkUnsupported'):s('checkFailed')});
      refreshNodes();
    }).catch(function(error){
      var message=error.name==='AbortError'?s('checkTimeout'):error.message;
      if(lastRow===row){result.textContent=message;result.hidden=false}
      if(automatic)autoStatus.textContent=message;
    }).finally(function(){clearTimeout(timeout);checkingEntry=false;checkButton.disabled=false;checkButton.textContent=s('checkConnection')});
  }
  checkButton.addEventListener('click',function(){runEntryCheck(lastRow,false)});
  function autoTick(){
    if(!autoCheck.checked||document.hidden||location.hash!=='#nodes'||dialog.open||checkingEntry||Date.now()<autoPausedUntil)return;
    var row=rows.filter(function(row){return !row.hidden&&Date.now()-Number(row.dataset.entryAttempt||0)>=300000}).sort(function(a,b){return Number(a.dataset.entryAttempt||0)-Number(b.dataset.entryAttempt||0)})[0];
    if(row)runEntryCheck(row,true);
  }
  autoCheck.addEventListener('change',function(){try{localStorage.setItem('polaris.portal.autoCheck',String(autoCheck.checked))}catch(_){}autoStatus.textContent=autoCheck.checked?s('autoCheckOn'):s('autoCheckOff');if(autoCheck.checked)autoTick()});
  setInterval(autoTick,12000);
  document.getElementById('p-nodes-refresh').addEventListener('click',refreshNodes);
  function nodesVisible(){if(location.hash==='#nodes'&&!document.hidden&&!dialog.open)refreshNodes()}
  window.addEventListener('hashchange',nodesVisible);document.addEventListener('visibilitychange',nodesVisible);setInterval(nodesVisible,30000);nodesVisible();
  var ua=navigator.userAgent,device=/Android/i.test(ua)?'Android':/iPhone|iPad|iPod/i.test(ua)?'iOS':/Macintosh/i.test(ua)?'macOS':/Windows/i.test(ua)?'Windows':'';
  if(device){document.getElementById('p-platform-tip').textContent=s('currentDevice',{device:device});document.getElementById('p-device-guide').textContent=s('deviceGuide',{device:device})}
})();
