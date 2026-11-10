(function(){
  var dialog=document.getElementById('p-subscription-binding');
  if(!dialog)return;
  var emailDialog=document.getElementById('p-email-binding'),telegramDialog=document.getElementById('p-telegram-binding'),tgChallenge=null,tgTimer=0,tgUntil=0,active=null,request=null,committing=false,sequence=0,ticket='',sentAddress='',sendUntil=0;
  var link=document.getElementById('p-binding-link'),check=document.getElementById('p-binding-check'),confirmButton=document.getElementById('p-binding-confirm');
  function show(id,text){var el=document.getElementById(id);el.textContent=text;el.hidden=!text}
  function lock(locked){if(active){active.querySelectorAll('button').forEach(function(button){button.disabled=locked&&(!button.hasAttribute('data-binding-close')||committing)});active.querySelectorAll('input').forEach(function(input){input.disabled=locked&&committing})}if(!locked&&active===emailDialog)updateCooldown()}
  function stop(){sequence++;if(request)request.abort();request=null;lock(false)}
  function open(which){if(active&&active!==which&&active.open)active.close();stop();active=which;ticket='';show('p-binding-error','');show('p-binding-preview','');confirmButton.hidden=true;check.hidden=false;if(!which.open)which.showModal()}
  function post(path,body){
    var seq=++sequence,owner=active,controller=new AbortController(),timeout=setTimeout(function(){controller.abort()},12000);request=controller;committing=path==='subscription/bind'||path==='email/verify'||path==='telegram/bind/poll';lock(true);
    return fetch('/u/'+path,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(body),signal:controller.signal}).then(function(response){return response.json().catch(function(){return {}}).then(function(data){
      if(seq!==sequence||!owner.open)throw {stale:true};
      if(!response.ok){if(response.status===401)location.replace('/u/login');throw new Error(data.error||'操作失败，请重试')}return data;
    })}).catch(function(error){if(seq!==sequence||!owner.open)throw {stale:true};throw error}).finally(function(){clearTimeout(timeout);if(seq===sequence){request=null;lock(false);committing=false}});
  }
  function fail(id,error){if(error.stale||!active||!active.open)return;show(id,error.name==='AbortError'?'请求超时，请重试':error.message||'操作失败，请重试')}
  document.querySelectorAll('[data-binding-close]').forEach(function(button){button.addEventListener('click',function(){button.closest('dialog').close()})});
  [dialog,emailDialog,telegramDialog].filter(Boolean).forEach(function(item){item.addEventListener('cancel',function(event){if(committing&&request)event.preventDefault()});item.addEventListener('close',function(){if(active===item){if(item===telegramDialog)cancelTelegram();stop();ticket='';active=null}})});
  document.getElementById('p-bind-subscription').addEventListener('click',function(){open(dialog);link.focus()});
  link.addEventListener('input',function(){stop();ticket='';show('p-binding-preview','');show('p-binding-error','');check.hidden=false;confirmButton.hidden=true});
  document.getElementById('p-subscription-binding-form').addEventListener('submit',function(event){
    event.preventDefault();if(request||!link.value.trim())return;show('p-binding-error','');
    post('subscription/preview',{subscription:link.value.trim()}).then(function(data){ticket=data.ticket;show('p-binding-preview',(data.sameAccount?'这条订阅已属于当前账号。':'将关联到账号「'+data.username+'」。')+' '+data.message);check.hidden=true;confirmButton.hidden=false;confirmButton.focus()}).catch(function(error){fail('p-binding-error',error)});
  });
  confirmButton.addEventListener('click',function(){if(request||!ticket)return;show('p-binding-error','');post('subscription/bind',{ticket:ticket}).then(function(){link.value='';try{sessionStorage.removeItem('polaris.pendingSubscription')}catch(_){}location.replace('/u#account');location.reload()}).catch(function(error){ticket='';check.hidden=false;confirmButton.hidden=true;fail('p-binding-error',error)})});
  function updateCooldown(){var button=document.getElementById('p-binding-send');if(!button)return;var seconds=Math.max(0,Math.ceil((sendUntil-Date.now())/1000));button.disabled=seconds>0;button.textContent=seconds?seconds+' 秒后重发':'发送验证码'}
  if(emailDialog){
    var email=document.getElementById('p-binding-email'),code=document.getElementById('p-binding-code');
    window.portalEmailBinding=function(){open(emailDialog);show('p-binding-mail-error','');show('p-binding-mail-note','');email.focus();updateCooldown()};
    email.addEventListener('input',function(){stop();sentAddress='';code.value='';show('p-binding-mail-note','');show('p-binding-mail-error','')});
    document.getElementById('p-binding-send').addEventListener('click',function(){
      if(request||Date.now()<sendUntil||!email.reportValidity())return;show('p-binding-mail-error','');var address=email.value.trim();
      post('email/code',{email:address,purpose:'bind'}).then(function(data){sentAddress=address;sendUntil=Date.now()+(data.cooldown||60)*1000;updateCooldown();show('p-binding-mail-note','验证码已发送，请检查邮箱和垃圾邮件。');code.focus()}).catch(function(error){fail('p-binding-mail-error',error)});
    });
    document.getElementById('p-email-binding-form').addEventListener('submit',function(event){event.preventDefault();if(request)return;if(sentAddress!==email.value.trim()){show('p-binding-mail-error','请先向当前邮箱发送验证码');return}show('p-binding-mail-error','');post('email/verify',{email:email.value.trim(),code:code.value.trim(),purpose:'bind'}).then(function(){location.reload()}).catch(function(error){fail('p-binding-mail-error',error)})});
    setInterval(function(){if(active===emailDialog&&!request)updateCooldown()},1000);
  }
  function cancelTelegram(){
    clearTimeout(tgTimer);var old=tgChallenge;tgChallenge=null;
    if(old)fetch('/u/telegram/bind/cancel',{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},credentials:'same-origin',keepalive:true,body:JSON.stringify({id:old.id,secret:old.secret})}).catch(function(){});
  }
  function telegramFailure(error){
    if(error.stale||active!==telegramDialog||!telegramDialog.open)return;
    cancelTelegram();document.getElementById('p-binding-tg-number').textContent='—';document.getElementById('p-binding-tg-open').hidden=true;document.getElementById('p-binding-tg-retry').hidden=false;
    fail('p-binding-tg-error',error);
  }
  function pollTelegram(){
    if(!tgChallenge||active!==telegramDialog||!telegramDialog.open)return;
    if(Date.now()>tgUntil){telegramFailure(new Error('绑定请求已失效，请重新发起'));return}
    post('telegram/bind/poll',{id:tgChallenge.id,secret:tgChallenge.secret}).then(function(data){
      if(data.status==='ok'){tgChallenge=null;location.replace('/u#account');location.reload();return}
      if(data.status!=='pending'){telegramFailure(new Error(data.status==='denied'?'已拒绝本次绑定':'绑定请求已失效，请重新发起'));return}
      show('p-binding-tg-note',data.opened?'请在机器人中选择网页上显示的数字。':'请打开 Telegram 机器人确认绑定。');
      tgTimer=setTimeout(pollTelegram,1500);
    }).catch(telegramFailure);
  }
  function startTelegram(){
    cancelTelegram();open(telegramDialog);document.getElementById('p-binding-tg-number').textContent='—';show('p-binding-tg-error','');show('p-binding-tg-note','正在创建绑定请求…');document.getElementById('p-binding-tg-retry').hidden=true;document.getElementById('p-binding-tg-open').hidden=true;
    post('telegram/bind/start',{}).then(function(data){
      tgChallenge=data;tgUntil=Date.now()+data.expiresIn*1000;document.getElementById('p-binding-tg-number').textContent=data.code;
      var link=document.getElementById('p-binding-tg-open');link.href=data.url;link.hidden=false;show('p-binding-tg-note','请在机器人中选择上方数字。不是本人操作时请拒绝。');
      tgTimer=setTimeout(pollTelegram,1500);
    }).catch(telegramFailure);
  }
  var telegramButton=document.getElementById('p-bind-telegram');
  if(telegramButton){telegramButton.addEventListener('click',startTelegram);document.getElementById('p-binding-tg-retry').addEventListener('click',startTelegram)}
  window.addEventListener('pagehide',cancelTelegram);
  var pending=null;
  if(location.hash.indexOf('#bind-subscription=')===0){var token=location.hash.slice(19);if(/^[A-Za-z0-9_-]{16,64}$/.test(token))pending={token:token,expires:Date.now()+600000};history.replaceState(null,'','/u#account');window.dispatchEvent(new HashChangeEvent('hashchange'))}
  try{if(!pending)pending=JSON.parse(sessionStorage.getItem('polaris.pendingSubscription')||'null');sessionStorage.removeItem('polaris.pendingSubscription')}catch(_){}
  if(pending&&pending.expires>Date.now()&&/^[A-Za-z0-9_-]{16,64}$/.test(pending.token)){location.hash='account';open(dialog);link.value=pending.token;link.focus()}
})();
