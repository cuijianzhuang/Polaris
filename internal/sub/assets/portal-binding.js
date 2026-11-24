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
(function(){
  // GitHub binding + sign-in method removals (identity batch 2). The
  // bind round trip leaves for GitHub; the callback parks a one-use
  // ticket in the query string and this page redeems it under the
  // portal session that started the flow.
  function post(path,body){return fetch('/u/'+path,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(body||{})}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok){if(r.status===401)location.replace('/u/login');throw new Error(j.error||'操作失败，请重试')}return j})})}
  function showCardError(text){var el=document.getElementById('p-binding-gh-error');if(el){el.textContent=text;el.hidden=!text}}
  var bindButton=document.getElementById('p-bind-github');
  if(bindButton)bindButton.addEventListener('click',function(){showCardError('');bindButton.disabled=true;post('github/bind-url',{}).then(function(d){location.href=d.url}).catch(function(e){bindButton.disabled=false;showCardError(e.message)})});
  var params=new URLSearchParams(location.search),ticket=params.get('ghbind'),ghError=params.get('gh_error');
  if(ticket||ghError){
    history.replaceState(null,'',location.pathname+'#account');
    if(ghError)showCardError(ghError);
    else post('bindings/github/complete',{ticket:ticket}).then(function(){location.replace('/u#account');location.reload()}).catch(function(e){showCardError(e.message)});
  }
  function wireRemoval(dialogId,openId,formId,errorId,closeAttr,path,body){
    var dialog=document.getElementById(dialogId),openButton=document.getElementById(openId);
    if(!dialog||!openButton)return;
    function show(text){var el=document.getElementById(errorId);if(el){el.textContent=text;el.hidden=!text}}
    openButton.addEventListener('click',function(){show('');if(!dialog.open)dialog.showModal()});
    dialog.querySelectorAll('['+closeAttr+']').forEach(function(b){b.addEventListener('click',function(){dialog.close()})});
    document.getElementById(formId).addEventListener('submit',function(event){
      event.preventDefault();show('');
      post(path,body()).then(function(){location.replace('/u/login')}).catch(function(e){show(e.message)});
    });
  }
  wireRemoval('p-github-unbind','p-unbind-github','p-github-unbind-form','p-gh-error','data-gh-close','bindings/github/unbind',function(){var cur=document.getElementById('p-gh-current');return cur?{currentPassword:cur.value}:{}});
  wireRemoval('p-password-clear','p-clear-password','p-password-clear-form','p-pwc-error','data-pwc-close','bindings/password/clear',function(){return {currentPassword:document.getElementById('p-pwc-current').value}});
})();
(function(){
  // SSO binding (identity batch 3b). Same ticket pattern as GitHub:
  // the callback parks a one-use ticket in the query string and this
  // page redeems it under the portal session that started the flow.
  // The provider rows render from the same /u/bindings view as the
  // other methods.
  var listEl=document.getElementById('p-sso-list'),dialog=document.getElementById('p-sso-unbind');
  if(!listEl&&!dialog)return;
  function post(path,body){return fetch('/u/'+path,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(body||{})}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok){if(r.status===401)location.replace('/u/login');throw new Error(j.error||'操作失败，请重试')}return j})})}
  function get(path){return fetch('/u/'+path,{credentials:'same-origin',cache:'no-store',headers:{'X-Polaris-Portal':'1'}}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok)throw new Error(j.error||'操作失败，请重试');return j})})}
  function showCardError(text){var el=document.getElementById('p-binding-gh-error');if(el){el.textContent=text;el.hidden=!text}}
  var params=new URLSearchParams(location.search),ticket=params.get('ssobind'),ssoError=params.get('sso_error');
  if(ticket||ssoError){
    history.replaceState(null,'',location.pathname+'#account');
    if(ssoError)showCardError(ssoError);
    else post('bindings/sso/complete',{ticket:ticket}).then(function(){location.replace('/u#account');location.reload()}).catch(function(e){showCardError(e.message)});
  }
  var unbinding=null;
  function showUnbindError(text){var el=document.getElementById('p-sso-unbind-error');if(el){el.textContent=text;el.hidden=!text}}
  if(dialog){
    dialog.querySelectorAll('[data-sso-close]').forEach(function(b){b.addEventListener('click',function(){dialog.close()})});
    document.getElementById('p-sso-unbind-form').addEventListener('submit',function(event){
      event.preventDefault();if(!unbinding)return;showUnbindError('');
      var cur=document.getElementById('p-sso-current');
      post('bindings/sso/unbind',{providerId:unbinding.providerId,currentPassword:cur?cur.value:undefined}).then(function(){location.replace('/u/login')}).catch(function(e){showUnbindError(e.message)});
    });
  }
  function render(m){
    if(!listEl)return;
    listEl.innerHTML='';
    (m.ssoProviders||[]).forEach(function(p){
      var bound=(m.sso||[]).filter(function(x){return x.providerId===p.id})[0];
      var row=document.createElement('div');row.className='p-inline-actions';row.style.alignItems='center';
      var label=document.createElement('span');label.textContent=p.name+' · '+(bound?('已绑定'+(bound.login?' · '+bound.login:'')):'未绑定');
      var btn=document.createElement('button');btn.className='btn alt';btn.type='button';
      if(bound){
        btn.textContent='解绑';
        btn.addEventListener('click',function(){unbinding={providerId:p.id};document.getElementById('p-sso-name').textContent=p.name;showUnbindError('');if(dialog&&!dialog.open)dialog.showModal()});
      }else{
        btn.textContent='绑定';
        if(!m.ssoAvailable)btn.disabled=true;
        btn.addEventListener('click',function(){showCardError('');btn.disabled=true;post('sso/bind-url',{providerId:p.id}).then(function(d){location.href=d.url}).catch(function(e){btn.disabled=false;showCardError(e.message)})});
      }
      row.appendChild(label);row.appendChild(btn);listEl.appendChild(row);
    });
  }
  if(listEl)get('bindings').then(render).catch(function(){});
})();
(function(){
  var dialog=document.getElementById('p-password-set'),openButton=document.getElementById('p-set-password');
  if(!dialog||!openButton)return;
  function show(id,text){var el=document.getElementById(id);if(el){el.textContent=text;el.hidden=!text}}
  function post(path,body){return fetch('/u/'+path,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(body)}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok){if(r.status===401)location.replace('/u/login');throw new Error(j.error||'操作失败，请重试')}return j})})}
  openButton.addEventListener('click',function(){show('p-pw-error','');show('p-pw-note','');if(!dialog.open)dialog.showModal()});
  dialog.querySelectorAll('[data-pw-close]').forEach(function(b){b.addEventListener('click',function(){dialog.close()})});
  var send=document.getElementById('p-pw-send'),sendUntil=0;
  function updateCooldown(){if(!send)return;var s=Math.max(0,Math.ceil((sendUntil-Date.now())/1000));send.disabled=s>0;send.textContent=s?s+' 秒后重发':'发送验证码'}
  if(send){send.addEventListener('click',function(){if(Date.now()<sendUntil)return;show('p-pw-error','');post('email/code',{email:send.getAttribute('data-email'),purpose:'setpw'}).then(function(data){sendUntil=Date.now()+(data.cooldown||60)*1000;updateCooldown();show('p-pw-note','验证码已发送，请检查邮箱和垃圾邮件。')}).catch(function(e){show('p-pw-error',e.message)})});setInterval(updateCooldown,1000)}
  document.getElementById('p-password-set-form').addEventListener('submit',function(event){
    event.preventDefault();show('p-pw-error','');
    var nw=document.getElementById('p-pw-new'),again=document.getElementById('p-pw-again');
    if(nw.value!==again.value){show('p-pw-error','两次输入的新密码不一致');again.focus();return}
    var body={newPassword:nw.value},cur=document.getElementById('p-pw-current'),code=document.getElementById('p-pw-code');
    if(cur)body.currentPassword=cur.value;
    if(code)body.code=code.value.trim();
    post('password/set',body).then(function(){location.replace('/u/login')}).catch(function(e){show('p-pw-error',e.message)});
  });
})();
(function(){
  // Passkey management (identity batch 3a). The list comes from the
  // same /u/bindings view as the other methods; ceremonies run against
  // this face's relying party, which the server configured from
  // portal_public_url — keys registered under another domain simply
  // cannot be used here, and the list shows each key's domain.
  var listEl=document.getElementById('p-passkey-list'),regDialog=document.getElementById('p-passkey-register'),delDialog=document.getElementById('p-passkey-delete'),renDialog=document.getElementById('p-passkey-rename'),regButton=document.getElementById('p-bind-passkey');
  if(!listEl&&!regButton)return;
  function post(path,body){return fetch('/u/'+path,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(body||{})}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok){if(r.status===401)location.replace('/u/login');throw new Error(j.error||'操作失败，请重试')}return j})})}
  function get(path){return fetch('/u/'+path,{credentials:'same-origin',cache:'no-store',headers:{'X-Polaris-Portal':'1'}}).then(function(r){return r.json().catch(function(){return {}}).then(function(j){if(!r.ok)throw new Error(j.error||'操作失败，请重试');return j})})}
  function show(id,text){var el=document.getElementById(id);if(el){el.textContent=text;el.hidden=!text}}
  function dec(s){s=String(s).replace(/-/g,'+').replace(/_/g,'/');while(s.length%4)s+='=';var raw=atob(s),buf=new Uint8Array(raw.length);for(var i=0;i<raw.length;i++)buf[i]=raw.charCodeAt(i);return buf.buffer}
  function enc(v){if(!v)return null;var b='',u=new Uint8Array(v);for(var i=0;i<u.length;i++)b+=String.fromCharCode(u[i]);return btoa(b).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')}
  function supported(){return window.isSecureContext&&typeof PublicKeyCredential!=='undefined'}
  function createCredential(options){
    var p=options.publicKey;
    return navigator.credentials.create({publicKey:{challenge:dec(p.challenge),rp:p.rp,user:{id:dec(p.user.id),name:p.user.name,displayName:p.user.displayName},pubKeyCredParams:p.pubKeyCredParams,timeout:p.timeout,attestation:p.attestation,authenticatorSelection:p.authenticatorSelection,excludeCredentials:(p.excludeCredentials||[]).map(function(c){return {type:c.type||'public-key',id:dec(c.id),transports:c.transports}})}}).then(function(cred){
      var r=cred.response;
      return {id:cred.id,rawId:enc(cred.rawId),type:cred.type,authenticatorAttachment:cred.authenticatorAttachment,clientExtensionResults:cred.getClientExtensionResults?cred.getClientExtensionResults():{},response:{clientDataJSON:enc(r.clientDataJSON),attestationObject:enc(r.attestationObject),transports:r.getTransports?r.getTransports():[]}};
    });
  }
  function render(keys){
    if(!listEl)return;
    listEl.innerHTML='';
    (keys||[]).forEach(function(k){
      var row=document.createElement('div');row.className='p-inline-actions';row.style.alignItems='center';
      var label=document.createElement('span');label.textContent=k.name+' · '+k.rpId+(k.lastUsedAt?' · 最近使用 '+String(k.lastUsedAt).slice(0,10):'');
      var ren=document.createElement('button');ren.className='btn alt';ren.type='button';ren.textContent='改名';
      ren.addEventListener('click',function(){openDialog(renDialog);renaming=k;document.getElementById('p-pk-rename-name').value=k.name;show('p-pk-rename-error','')});
      var del=document.createElement('button');del.className='btn alt';del.type='button';del.textContent='删除';
      del.addEventListener('click',function(){deleting=k;document.getElementById('p-pk-del-name').textContent=k.name;show('p-pk-del-error','');openDialog(delDialog)});
      row.appendChild(label);row.appendChild(ren);row.appendChild(del);listEl.appendChild(row);
    });
  }
  function refresh(){get('bindings').then(function(m){render(m.passkeys)}).catch(function(){})}
  function openDialog(d){if(d&&!d.open)d.showModal()}
  document.querySelectorAll('[data-pk-close]').forEach(function(b){b.addEventListener('click',function(){b.closest('dialog').close()})});
  var deleting=null,renaming=null;
  if(regButton){
    if(!supported())regButton.disabled=true;
    regButton.addEventListener('click',function(){show('p-pk-reg-error','');openDialog(regDialog)});
  }
  var regForm=document.getElementById('p-passkey-register-form');
  if(regForm)regForm.addEventListener('submit',function(event){
    event.preventDefault();show('p-pk-reg-error','');
    var name=document.getElementById('p-pk-name').value.trim(),cur=document.getElementById('p-pk-current');
    post('passkeys/register/begin',{name:name,currentPassword:cur?cur.value:undefined}).then(function(b){
      return createCredential(b.options).then(function(credential){
        return post('passkeys/register/finish',{challengeId:b.challengeId,credential:credential});
      });
    }).then(function(){location.replace('/u#account');location.reload()}).catch(function(e){show('p-pk-reg-error',e&&e.name==='NotAllowedError'?'设备验证已取消或超时，请重试':(e.message||'操作失败，请重试'))});
  });
  var delForm=document.getElementById('p-passkey-delete-form');
  if(delForm)delForm.addEventListener('submit',function(event){
    event.preventDefault();if(!deleting)return;show('p-pk-del-error','');
    var cur=document.getElementById('p-pk-del-current');
    post('passkeys/delete',{id:deleting.id,currentPassword:cur?cur.value:undefined}).then(function(){location.replace('/u/login')}).catch(function(e){show('p-pk-del-error',e.message)});
  });
  var renForm=document.getElementById('p-passkey-rename-form');
  if(renForm)renForm.addEventListener('submit',function(event){
    event.preventDefault();if(!renaming)return;show('p-pk-rename-error','');
    post('passkeys/rename',{id:renaming.id,name:document.getElementById('p-pk-rename-name').value}).then(function(){renDialog.close();refresh()}).catch(function(e){show('p-pk-rename-error',e.message)});
  });
  refresh();
})();
