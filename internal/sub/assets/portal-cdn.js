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
  var profileID = '', port = '';
  function el(id) { return document.getElementById(id); }
  function remember(key, value) { try { sessionStorage.setItem(key, value); } catch (_) {} }
  function recalled(key) { try { return sessionStorage.getItem(key) || ''; } catch (_) { return ''; } }
  function split(value) {
    return Array.from(new Set(value.split(/[\s,，]+/).map(function (v) {
      return v.trim().toLowerCase().replace(/\.$/, '').replace(/^\[(.*)\]$/, '$1');
    }).filter(Boolean)));
  }
  function fail(e) { error.textContent = e.message; error.hidden = false; }
  function key(address) { return (address.includes(':') ? '[' + address + ']' : address) + ':' + port; }
  function automatic() { return (pool.automatic || {})[port] || []; }
  function allAddresses() { return Array.from(new Set(manual.concat(automatic()))); }
  function disabled(address) { return (pool.disabled || []).includes(address); }
  function verdict(address) { return (pool.checks || {})[key(address)]; }
  function request(action, extra) {
    return pj('cdn-pool', Object.assign({profileId: profileID, port: Number(port), action: action}, extra || {}));
  }
  function lock(value) {
    busy = value;
    select.disabled = value;
    ports.disabled = value;
    addresses.disabled = value;
    el('cdn-add-address').disabled = value;
    el('cdn-add').disabled = value;
    el('cdn-import').disabled = value;
    el('cdn-import-label').disabled = value;
    save.disabled = value || !dirty;
    panel.querySelectorAll('#cdn-pool-items button').forEach(function (button) { button.disabled = value || dirty; });
    var option = select.options[select.selectedIndex];
    var supported = option.getAttribute('data-supported') === 'true';
    el('cdn-pair').disabled = value || dirty || !supported;
    el('cdn-revoke').disabled = value || dirty || !option.getAttribute('data-probe');
    el('cdn-check-all').disabled = value || dirty || !allAddresses().some(function (a) { return !disabled(a); });
  }
  function draftChanged() {
    dirty = split(addresses.value).join('\n') !== manual.join('\n');
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
    var failedCount = list.filter(function (a) { return !disabled(a) && verdict(a) && !verdict(a).ok; }).length;
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
      state.textContent = isDisabled ? '已禁用' : check ? check.ok ? '检测通过' : '检测失败' : '未检测'; row.appendChild(state);
      var spacer = document.createElement('span'); spacer.className = 'sp'; row.appendChild(spacer);
      function button(label, action) {
        var b = document.createElement('button'); b.type = 'button'; b.className = 'btn sm'; b.textContent = label; b.disabled = busy || dirty; b.onclick = action; row.appendChild(b); return b;
      }
      button(isDisabled ? '启用' : '禁用', function () {
        if (busy || dirty) return;
        var denied = (pool.disabled || []).filter(function (a) { return a !== address; });
        if (!isDisabled) denied.push(address);
        lock(true); error.hidden = true;
        pj('cdn-preferences', {profileId: profileID, addresses: manual, disabled: denied}).then(refreshPage).catch(fail).finally(function () { lock(false); });
      });
      button('检测', function () {
        if (busy || dirty) return;
        lock(true); error.hidden = true;
        el('cdn-check-progress').textContent = '正在检测 ' + address + '…';
        checkOne(address).then(function () { el('cdn-check-progress').textContent = '检测完成'; }).catch(fail).finally(function () { render(); lock(false); });
      });
      button(manual.includes(address) ? '移除' : '保留为手动', function () {
        if (busy || dirty) return;
        if (manual.includes(address)) {
          addresses.value = manual.filter(function (a) { return a !== address; }).join('\n');
          bulk.open = true; draftChanged();
        } else { addToDraft([address]); }
      });
      if (check) {
        var detail = document.createElement('p'); detail.className = check.ok ? 'mute' : 'mute bad';
        detail.textContent = check.message + ' · ' + new Date(check.at).toLocaleString(); item.appendChild(detail);
      }
      box.appendChild(item);
    });
    lock(busy);
  }
  function checkOne(address) {
    return request('check', {address: address}).then(function (r) {
      pool.checks = pool.checks || {}; pool.checks[key(address)] = r.check; render();
    });
  }
  function load() {
    profileID = select.value;
    var option = select.options[select.selectedIndex];
    pool = JSON.parse(option.getAttribute('data-pool') || '{}');
    manual = split(option.getAttribute('data-addresses') || '');
    addresses.value = manual.join('\n'); dirty = false;
    ports.replaceChildren();
    JSON.parse(option.getAttribute('data-ports') || '[]').forEach(function (p) {
      var o = document.createElement('option'); o.value = p; o.textContent = p; ports.appendChild(o);
    });
    var rememberedPort = recalled('polaris.cdn.port.' + profileID);
    if (Array.from(ports.options).some(function (o) { return o.value === rememberedPort; })) ports.value = rememberedPort;
    port = ports.value;
    el('cdn-selection').textContent = option.getAttribute('data-selection') || '';
    el('cdn-pool-warning').hidden = option.getAttribute('data-expanded') === 'true';
    el('cdn-probe-status').textContent = option.getAttribute('data-supported') !== 'true' ? '此配置的服务商暂不支持官方测速程序，可继续手动维护。' : option.getAttribute('data-probe') ? '令牌 ' + option.getAttribute('data-probe') + '… · 测速端口 ' + option.getAttribute('data-probe-port') + (pool.reportedAt ? ' · 最后上报 ' + new Date(pool.reportedAt).toLocaleString() : ' · 等待首次上报') : '尚未创建令牌，手动维护可直接使用。';
    el('cdn-pair').textContent = option.getAttribute('data-probe') ? '重置专用上报令牌' : '创建专用上报令牌';
    el('cdn-install').hidden = true; el('cdn-command').value = ''; el('cdn-copy-status').textContent = '';
    bulk.open = !manual.length; error.hidden = true; el('cdn-check-progress').textContent = '';
    draftChanged(); render(); remember('polaris.cdn.profile', profileID);
  }
  var rememberedProfile = recalled('polaris.cdn.profile');
  if (Array.from(select.options).some(function (o) { return o.value === rememberedProfile; })) select.value = rememberedProfile;
  select.addEventListener('change', function () {
    if (dirty && !confirm('手动地址尚未保存，切换配置会丢弃这些修改。继续切换？')) { select.value = profileID; return; }
    load();
  });
  ports.addEventListener('change', function () { port = ports.value; remember('polaris.cdn.port.' + profileID, port); el('cdn-check-progress').textContent = ''; render(); });
  addresses.addEventListener('input', draftChanged);
  window.addEventListener('beforeunload', function (event) { if (dirty) { event.preventDefault(); event.returnValue = ''; } });
  save.addEventListener('click', function () {
    if (busy) return;
    var list = split(addresses.value);
    if (list.length > 32) { fail(new Error('每个配置最多 32 个手动地址。')); return; }
    lock(true); error.hidden = true;
    pj('cdn-preferences', {profileId: profileID, addresses: list, disabled: pool.disabled || []}).then(refreshPage).catch(fail).finally(function () { lock(false); });
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
    finally { el('cdn-stop-checks').hidden = true; render(); lock(false); }
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
  load();
})();
