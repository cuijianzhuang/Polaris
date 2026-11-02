(function () {
  var panel = document.getElementById('cdn-preferences');
  if (!panel) return;
  if (location.hash === '#cdn-preferences') panel.open = true;
  var select = document.getElementById('cdn-profile');
  if (!select) return;
  var addresses = document.getElementById('cdn-addresses');
  var ports = document.getElementById('cdn-port');
  var save = document.getElementById('cdn-save');
  var error = document.getElementById('cdn-preferences-error');
  var bulk = document.getElementById('cdn-bulk-edit');
  var pool = {}, manual = [], busy = false, dirty = false, cancelled = false;
  var profileID = '', port = '', entries = [], version = '';
  var initialPolicy = 'keep', initialHours = 24, previewSequence = 0;
  var entrySelect = el('cdn-endpoint');
  function currentEntry() { return entries.find(function (e) { return e.endpointId === entrySelect.value; }); }
  function el(id) { return document.getElementById(id); }
  function remember(key, value) { try { sessionStorage.setItem(key, value); } catch (_) {} }
  function recalled(key) { try { return sessionStorage.getItem(key) || ''; } catch (_) { return ''; } }
  function split(value) {
    return Array.from(new Set(value.split(/[\s,，]+/).map(function (v) {
      return v.trim().toLowerCase().replace(/\.$/, '').replace(/^\[(.*)\]$/, '$1');
    }).filter(Boolean)));
  }
  function fail(e) { error.textContent = e.message; error.hidden = false; if (/其他窗口|版本|最新版本|配置已改变/.test(e.message)) el('cdn-conflict-reload').hidden = false; }
  function key(address) { return (address.includes(':') ? '[' + address + ']' : address) + ':' + port; }
  function automatic() { return (pool.automatic || {})[port] || []; }
  function allAddresses() { return Array.from(new Set(manual.concat(automatic()))); }
  function disabled(address) { return (pool.disabled || []).includes(address); }
  function verdict(address) { var entry=currentEntry(); return entry ? (entry.checks || {})[address] : undefined; }
  function request(action, extra) {
    var entry=currentEntry();
    return pj('cdn-pool', Object.assign({profileId: profileID, port: Number(port), endpointId:entry && entry.endpointId,context:entry && entry.context,action: action}, extra || {}));
  }
  function lock(value) {
    busy = value;
    select.disabled = value;
    ports.disabled = value; entrySelect.disabled = value;
    el('cdn-stale-policy').disabled = value;el('cdn-stale-hours').disabled = value;el('cdn-preview-refresh').disabled=value||dirty;
    addresses.disabled = value;
    el('cdn-add-address').disabled = value;
    el('cdn-add').disabled = value;
    el('cdn-import').disabled = value;
    el('cdn-import-label').disabled = value;
    save.disabled = value || !dirty || !Number.isInteger(Number(el('cdn-stale-hours').value)) || Number(el('cdn-stale-hours').value)<1 || Number(el('cdn-stale-hours').value)>720;
    el('cdn-save-policy').disabled=save.disabled;
    panel.querySelectorAll('#cdn-pool-items button').forEach(function (button) { button.disabled = value || dirty || (button.dataset.check==='1' && !currentEntry()); });
    var option = select.options[select.selectedIndex];
    var supported = option.getAttribute('data-supported') === 'true';
    el('cdn-pair').disabled = value || dirty || !supported || !currentEntry();
    el('cdn-revoke').disabled = value || dirty || !option.getAttribute('data-probe');
    el('cdn-check-all').disabled = value || dirty || !currentEntry() || !allAddresses().some(function (a) { return !disabled(a); });
  }
  function draftChanged() {
    dirty = split(addresses.value).join('\n') !== manual.join('\n') || el('cdn-stale-policy').value!==initialPolicy || Number(el('cdn-stale-hours').value)!==initialHours;
    el('cdn-draft').textContent = dirty ? '有未保存的修改，请先保存再操作地址。' : '手动地址已保存';
    lock(busy);
  }
  function addToDraft(values) {
    var next = Array.from(new Set(split(addresses.value).concat(values)));
    if (next.length > 32) { fail(new Error('每个配置最多 32 个手动地址，当前合并后为 ' + next.length + ' 个。')); return; }
    addresses.value = next.join('\n');
    bulk.open = true;
    error.hidden = true;
    draftChanged();
  }
  function refreshPage() {
    dirty = false;
    remember('polaris.cdn.profile', profileID);
    remember('polaris.cdn.port.' + profileID, port);
    history.replaceState(null, '', '/u#cdn-preferences');
    location.reload();
  }
  function render() {
    var box = el('cdn-pool-items'), list = allAddresses(), auto = automatic();
    box.replaceChildren();
    var disabledCount = list.filter(disabled).length;
    var failedCount = list.filter(function (a) { var r=verdict(a);return !disabled(a) && r && (r.failed || (!r.ok&&!r.partial)); }).length;
    showFreshness();
    el('cdn-selection').textContent = list.length ? '个人优选池按手动顺序优先，追加当前端口的自动结果；无可用个人地址时使用公共池。' : '个人池暂无当前端口的地址，将按地区／运营商规则使用公共池。';
    var summary = el('cdn-summary');
    summary.replaceChildren();
    [manual.length + ' 个手动', auto.filter(function (a) { return !manual.includes(a); }).length + ' 个自动', disabledCount + ' 个禁用', failedCount + ' 个检测失败'].forEach(function (text) {
      var badge = document.createElement('span'); badge.className = 'tag'; badge.textContent = text; summary.appendChild(badge);
    });
    if (!list.length) {
      var empty = document.createElement('p'); empty.className = 'mute'; empty.textContent = '暂无个人地址，当前使用公共池。可手动添加，或安装官方程序自动优选。'; box.appendChild(empty);
    }
    list.forEach(function (address) {
      var item = document.createElement('div'); item.className = 'cdn-item';
      var title = document.createElement('strong'); title.className = 'cdn-item-address'; title.textContent = address; item.appendChild(title);
      var row = document.createElement('div'); row.className = 'row'; item.appendChild(row);
      var source = document.createElement('span'); source.className = 'tag'; source.textContent = manual.includes(address) ? '手动' : '自动'; row.appendChild(source);
      var check = verdict(address), isDisabled = disabled(address);
      var state = document.createElement('span'); state.className = 'st ' + (isDisabled ? '' : check ? check.ok ? 'ok' : 'bad' : '');
      state.textContent = isDisabled ? '已禁用' : check ? check.ok ? '检测通过' : check.failed ? '检测失败' : check.partial ? '部分完成' : '检测失败' : '未检测'; row.appendChild(state);
      var spacer = document.createElement('span'); spacer.className = 'sp'; row.appendChild(spacer);
      function button(label, action) {
        var b = document.createElement('button'); b.type = 'button'; b.className = 'btn sm'; b.textContent = label; b.disabled = busy || dirty || (label==='检测' && !currentEntry()); if(label==='检测')b.dataset.check='1';b.onclick = action; row.appendChild(b); return b;
      }
      button(isDisabled ? '启用' : '禁用', function () {
        if (busy || dirty) return;
        var denied = (pool.disabled || []).filter(function (a) { return a !== address; });
        if (!isDisabled) denied.push(address);
        lock(true); error.hidden = true;
        pj('cdn-preferences', {profileId: profileID, addresses: manual, disabled: denied, version: version}).then(refreshPage).catch(fail).finally(function () { lock(false); });
      });
      button('检测', function () {
        if (busy || dirty) return;
        lock(true); error.hidden = true;
        el('cdn-check-progress').textContent = '正在检测 ' + address + '…';
        checkOne(address).then(function () { el('cdn-check-progress').textContent = '检测完成';refreshPreview(); }).catch(fail).finally(function () { render(); lock(false); });
      });
      button(manual.includes(address) ? '移除' : '保留为手动', function () {
        if (busy || dirty) return;
        if (manual.includes(address)) {
          addresses.value = manual.filter(function (a) { return a !== address; }).join('\n');
          bulk.open = true; draftChanged();
        } else { addToDraft([address]); }
      });
      if (check) {
        var detail = document.createElement('p'); detail.className = check.ok || (check.partial&&!check.failed) ? 'mute' : 'mute bad';
        detail.textContent = check.message + ' · ' + new Date(check.at).toLocaleString(); item.appendChild(detail);
      }
      if(!check && (pool.checks||{})[key(address)]){var legacy=document.createElement('p');legacy.className='mute';legacy.textContent='旧版结果未记录入口配置，请重新检测。';item.appendChild(legacy)}
      box.appendChild(item);
    });
    lock(busy);
  }
  function checkOne(address) {
    return request('check', {address: address}).then(function (r) {
      var entry=currentEntry();if(entry){entry.checks=entry.checks||{};entry.checks[address]=r.check;}render();
    });
  }
  function load() {
    profileID = select.value;
    var option = select.options[select.selectedIndex];
    pool = JSON.parse(option.getAttribute('data-pool') || '{}');
    entries=JSON.parse(option.getAttribute('data-entries')||'[]');version=option.getAttribute('data-version')||'';
    initialPolicy=pool.stalePolicy==='fallback'?'fallback':'keep';initialHours=pool.staleHours||24;el('cdn-stale-policy').value=initialPolicy;el('cdn-stale-hours').value=initialHours;
    manual = split(option.getAttribute('data-addresses') || '');
    addresses.value = manual.join('\n'); dirty = false;
    ports.replaceChildren();
    JSON.parse(option.getAttribute('data-ports') || '[]').forEach(function (p) {
      var o = document.createElement('option'); o.value = p; o.textContent = p; ports.appendChild(o);
    });
    var rememberedPort = recalled('polaris.cdn.port.' + profileID);
    if (Array.from(ports.options).some(function (o) { return o.value === rememberedPort; })) ports.value = rememberedPort;
    port = ports.value;populateEntries();
    el('cdn-selection').textContent = option.getAttribute('data-selection') || '';
    el('cdn-pool-warning').hidden = option.getAttribute('data-expanded') === 'true';
    el('cdn-probe-status').textContent = option.getAttribute('data-supported') !== 'true' ? '此配置的服务商暂不支持官方测速程序，可继续手动维护。' : option.getAttribute('data-probe') ? '令牌 ' + option.getAttribute('data-probe') + '… · 测速端口 ' + option.getAttribute('data-probe-port') + (pool.reportedAt ? ' · 最后上报 ' + new Date(pool.reportedAt).toLocaleString() : ' · 等待首次上报') : '尚未创建令牌，手动维护可直接使用。';
    el('cdn-pair').textContent = option.getAttribute('data-probe') ? '重置专用上报令牌' : '创建专用上报令牌';
    el('cdn-install').hidden = true; el('cdn-command').value = ''; el('cdn-copy-status').textContent = '';
    bulk.open = !manual.length; error.hidden = true; el('cdn-check-progress').textContent = '';
    el('cdn-conflict-reload').hidden=true;draftChanged(); render(); remember('polaris.cdn.profile', profileID);refreshPreview();
  }
  var rememberedProfile = recalled('polaris.cdn.profile');
  if (Array.from(select.options).some(function (o) { return o.value === rememberedProfile; })) select.value = rememberedProfile;
  select.addEventListener('change', function () {
    if (dirty && !confirm('手动地址尚未保存，切换配置会丢弃这些修改。继续切换？')) { select.value = profileID; return; }
    load();
  });
  ports.addEventListener('change', function () { port = ports.value; remember('polaris.cdn.port.' + profileID, port); el('cdn-check-progress').textContent = ''; populateEntries();render();refreshPreview(); });
  addresses.addEventListener('input', draftChanged);
  window.addEventListener('beforeunload', function (event) { if (dirty) { event.preventDefault(); event.returnValue = ''; } });
  save.addEventListener('click', function () {
    if (busy) return;
    var list = split(addresses.value);
    if (list.length > 32) { fail(new Error('每个配置最多 32 个手动地址。')); return; }
    lock(true); error.hidden = true;
    pj('cdn-preferences', {profileId: profileID, addresses: list, disabled: pool.disabled || [],version:version,stalePolicy:el('cdn-stale-policy').value,staleHours:Number(el('cdn-stale-hours').value)}).then(refreshPage).catch(fail).finally(function () { lock(false); });
  });
  el('cdn-add').onclick = function () { addToDraft(split(el('cdn-add-address').value)); el('cdn-add-address').value = ''; };
  el('cdn-add-address').addEventListener('keydown', function (event) { if (event.key === 'Enter') { event.preventDefault(); el('cdn-add').click(); } });
  el('cdn-import-label').onclick = function () { if (!busy) el('cdn-import').click(); };
  el('cdn-import').onchange = function (event) {
    var file = event.target.files[0]; event.target.value = ''; if (!file || busy) return;
    if (file.size > 16384) { fail(new Error('导入文件不能超过 16 KB。')); return; }
    var owner = profileID; lock(true);
    file.text().then(function (text) { if (owner === profileID) addToDraft(split(text)); }).catch(fail).finally(function () { lock(false); });
  };
  el('cdn-check-all').onclick = async function () {
    if (busy || dirty) return;
    var list = allAddresses().filter(function (a) { return !disabled(a); }), done = 0;
    cancelled = false; error.hidden = true; lock(true); el('cdn-stop-checks').hidden = false;
    try {
      for (var address of list) {
        if (cancelled) break;
        el('cdn-check-progress').textContent = '检测 ' + (done + 1) + ' / ' + list.length + ' · ' + address;
        await checkOne(address); done++;
      }
      el('cdn-check-progress').textContent = (cancelled ? '已停止，' : '检测完成，') + '已检测 ' + done + ' / ' + list.length + ' 个地址';
    } catch (e) { fail(e); el('cdn-check-progress').textContent = '检测中断，已检测 ' + done + ' / ' + list.length; }
    finally { el('cdn-stop-checks').hidden = true; render(); lock(false);refreshPreview(); }
  };
  el('cdn-stop-checks').onclick = function () { cancelled = true; el('cdn-check-progress').textContent = '正在停止，等待当前地址检测结束…'; };
  ['pair', 'revoke'].forEach(function (action) {
    el('cdn-' + action).onclick = function () {
      if (busy || dirty) return;
      lock(true); error.hidden = true;
      request(action).then(function (r) {
        if (action === 'revoke') { refreshPage(); return; }
        el('cdn-command').value = r.command; el('cdn-install').hidden = false;
        el('cdn-probe-status').textContent = '专用令牌已创建，旧令牌已失效；请复制安装命令。';
        select.options[select.selectedIndex].setAttribute('data-probe', 'created');
        el('cdn-pair').textContent = '重置专用上报令牌';
      }).catch(fail).finally(function () { lock(false); });
    };
  });
  el('cdn-copy-command').onclick = function () {
    copyText(el('cdn-command').value).then(function () { el('cdn-copy-status').textContent = ' 已复制'; }).catch(function () { fail(new Error('复制失败，请手动复制命令。')); });
  };
  function populateEntries() {
    entrySelect.replaceChildren();
    entries.filter(function(e){return String(e.target.port)===String(port)}).forEach(function(e){var option=document.createElement('option');option.value=e.endpointId;option.textContent=e.label+' · '+e.target.scheme.toUpperCase()+'://'+e.target.sni+':'+e.target.port+e.target.path+(e.target.host!==e.target.sni?' · Host '+e.target.host:'');entrySelect.appendChild(option)});
    var remembered=recalled('polaris.cdn.entry.'+profileID+'.'+port);
    if(Array.from(entrySelect.options).some(function(o){return o.value===remembered}))entrySelect.value=remembered;
    showTarget();
  }
  function showTarget(){var e=currentEntry();el('cdn-endpoint-target').textContent=e?'SNI '+e.target.sni+' · Host '+e.target.host+' · 路径 '+e.target.path:'当前端口没有可检测的授权入口。';}
  function showFreshness(){
    var meta=(pool.portReports||{})[port],at=meta?meta.at:pool.reportedAt;
    var expired=!at||Date.now()-new Date(at).getTime()>=(pool.staleHours||24)*3600000;
    var protocolChanged=meta&&meta.scheme&&currentEntry()&&meta.scheme!==currentEntry().target.scheme;
    el('cdn-freshness').textContent='端口 '+port+'：'+(!automatic().length?'暂无自动结果':protocolChanged?'原结果协议已改变，需重新测速':expired?'自动结果已过期；'+(pool.stalePolicy==='fallback'?'自动地址暂停使用，可用手动地址仍优先，否则回退公共池':'继续使用最近结果'):'自动结果未过期')+(at?' · 最后上报 '+new Date(at).toLocaleString():'');
  }
  var reasonText={selected:'已选用',selected_public_fallback:'公共池兜底选用',disabled:'本人已禁用',public_blocked:'公共池规则禁用',check_failed:'当前入口检测失败',public_check_failed:'公共入口检测失败',node_check_failed:'节点检测失败',entry_override:'入口固定地址或未展开',automatic_expired_or_protocol_changed:'自动结果过期或协议已改变',personal_preferred:'优先使用个人地址',account_unavailable:'账号当前不可用',top_n:'超出公共自动地址数量',not_selected:'未选用（入口或配额不可用）'};
  function refreshPreview(){
    var requestedProfile=profileID,sequence=++previewSequence;el('cdn-preview-refresh').disabled=true;
    return request('preview').then(function(r){
      if(requestedProfile!==profileID||sequence!==previewSequence)return;
      var box=el('cdn-preview');box.replaceChildren();
      (r.entries||[]).forEach(function(entry){var card=document.createElement('details');card.className='cdn-item';card.dataset.endpoint=entry.endpointId;card.open=currentEntry()&&currentEntry().endpointId===entry.endpointId;var heading=document.createElement('summary');heading.textContent=entry.label+' · '+entry.target.port+' · '+entry.target.sni;card.appendChild(heading);
        var mode=document.createElement('p');mode.className='mute';mode.textContent=({personal:'使用个人地址池',public:'公共池兜底',entry:'使用入口指定地址／回源域名',unavailable:'账号当前不可用'})[entry.mode]||entry.mode;card.appendChild(mode);
        var identity=document.createElement('p');identity.className='mute';identity.textContent=entry.target.scheme.toUpperCase()+':'+entry.target.port+' · SNI '+entry.target.sni+' · Host '+entry.target.host+' · 路径 '+entry.target.path;card.appendChild(identity);
        entry.rows.forEach(function(row){var line=document.createElement('p');line.className=row.selected?'ok':'mute';line.textContent=row.address+' · '+({manual:'手动',automatic:'自动',public:'公共',entry:'入口'})[row.source]+' · '+(reasonText[row.reason]||row.reason);card.appendChild(line)});
        if(!entry.selected.length){var empty=document.createElement('p');empty.textContent='此入口暂无可输出地址。';card.appendChild(empty)}box.appendChild(card);
      });
      var note=document.createElement('p');note.className='mute';note.textContent=r.note||'暂无可用授权入口。';box.appendChild(note);
    }).catch(function(e){if(sequence===previewSequence)fail(e)}).finally(function(){if(sequence===previewSequence)lock(busy)});
  }
  entrySelect.addEventListener('change',function(){remember('polaris.cdn.entry.'+profileID+'.'+port,entrySelect.value);showTarget();render();refreshPreview()});
  el('cdn-stale-policy').addEventListener('change',draftChanged);el('cdn-stale-hours').addEventListener('input',draftChanged);
  el('cdn-preview-refresh').onclick=refreshPreview;el('cdn-save-policy').onclick=function(){save.click()};
  el('cdn-conflict-reload').onclick=function(){if(confirm('请先复制需要保留的草稿。重新加载会放弃当前未保存的修改。'))refreshPage()};
  load();
})();
