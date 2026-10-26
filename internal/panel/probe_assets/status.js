(function () {
  'use strict';
  const $ = (id) => document.getElementById(id);
  let last = null, received = 0, busy = false, region = '', state = 'all', selected = null;
  const key = 'polaris.status.view:' + location.pathname;
  let view = 'card';
  try { const v = localStorage.getItem(key) || localStorage.getItem('status-view'); if (['ring', 'table'].includes(v)) view = v; } catch (_) {}
  const esc = (v) => String(v == null ? '' : v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const clamp = (v) => Math.max(0, Math.min(100, Number(v) || 0));
  const now = () => last ? Date.parse(last.at) + Math.max(0, performance.now() - received) : Date.now();
  const fresh = (n) => n.online && n.telemetryFresh !== false && Number.isFinite(Date.parse(n.lastSeen)) && now() - Date.parse(n.lastSeen) <= 90000 && Date.parse(n.lastSeen) - now() <= 60000;
  const liveLatency = (l) => l && Number.isFinite(Date.parse(l.at)) && now() - Date.parse(l.at) <= 180000 && Date.parse(l.at) - now() <= 60000;
  const flag = (cc) => /^[a-z]{2}$/i.test(cc || '') ? String.fromCodePoint(...cc.toUpperCase().split('').map((v) => 127397 + v.charCodeAt(0))) : '◎';
  const format = (value, units, base) => { let n = Number(value) || 0, i = 0; while (n >= base && i < units.length - 1) { n /= base; i++; } return n.toFixed(i ? 1 : 0) + ' ' + units[i]; };
  const bytes = (v) => format(v, ['B', 'KB', 'MB', 'GB', 'TB', 'PB'], 1024);
  const bps = (v) => format(v, ['bps', 'Kbps', 'Mbps', 'Gbps', 'Tbps'], 1000);
  const uptime = (s) => s >= 86400 ? Math.floor(s / 86400) + '天 ' + Math.floor(s % 86400 / 3600) + '时' : Math.floor(s / 3600) + '时 ' + Math.floor(s % 3600 / 60) + '分';
  const uc = (v) => v >= 85 ? 'var(--pv-red)' : v >= 60 ? 'var(--pv-yellow)' : 'var(--pv-green)';
  const lc = (v) => v >= 5 ? 'var(--pv-red)' : v >= 1 ? 'var(--pv-yellow)' : 'var(--pv-green)';
  const pc = (v) => v == null ? 'var(--pv-mute)' : v <= 0 ? 'var(--pv-red)' : v <= 80 ? 'var(--pv-green)' : v <= 160 ? 'var(--pv-cyan)' : v <= 260 ? 'var(--pv-yellow)' : 'var(--pv-red)';
  const bucketTime = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const col = { ct: '#3b82f6', cu: '#e8795b', cm: '#10b981' };
  const targets = (n) => n.probeTargets || last.targets || [];
  const concern = (n) => !n.online || !fresh(n) || n.status === 'error' || n.cpu >= 85 || n.mem >= 85 || (n.diskTotal && n.disk >= 85) || Object.values(n.latency || {}).some((l) => liveLatency(l) && (l.ms <= 0 || l.loss >= 5));
  function badge(n) {
    const s = !n.online ? 'off' : !fresh(n) ? 'stale' : n.status === 'error' ? 'error' : 'on';
    return '<span class="pv-st ' + s + '"><i></i>' + ({ off: '离线', stale: '数据滞后', error: '配置异常', on: '在线' }[s]) + '</span>';
  }
  function badges(n) {
    let h = '<div class="pv-meta">';
    if (n.os) h += '<span class="pv-badge pv-os" title="' + esc(n.os) + '">' + esc(n.os) + '</span>';
    const size = [n.cores ? n.cores + 'C' : '', n.memTotal ? bytes(n.memTotal).replace(' ', '') : '', n.diskTotal ? bytes(n.diskTotal).replace(' ', '') : ''].filter(Boolean).join(' / ');
    if (size) h += '<span class="pv-badge">' + size + '</span>';
    if (n.ipv4 || n.ipv6) h += '<span class="pv-badge v4">' + (n.ipv4 && n.ipv6 ? 'IPv4/6' : n.ipv4 ? 'IPv4' : 'IPv6') + '</span>';
    if (n.price) h += '<span class="pv-badge cost">' + esc(n.price) + '</span>';
    const at = Date.parse(n.expireAt);
    if (Number.isFinite(at)) { const days = at <= now() ? -Math.max(1, Math.ceil((now() - at) / 864e5)) : Math.ceil((at - now()) / 864e5); h += '<span class="pv-badge exp' + (days <= 7 ? ' late' : '') + '">' + (days < 0 ? '已过期' : '剩 ' + days + ' 天') + '</span>'; }
    return h + '</div>';
  }
  function meter(label, value) {
    return '<div class="pv-row"><span class="pv-k">' + label + '</span><span class="pv-track"><i style="width:' + (value == null ? 0 : clamp(value)) + '%;background:' + uc(value || 0) + '"></i></span><span class="pv-val">' + (value == null ? '—' : value.toFixed(1) + '%') + '</span></div>';
  }
  function ring(label, value) {
    const c = 2 * Math.PI * 31;
    return '<div class="pv-ring"><svg viewBox="0 0 80 80" role="img" aria-label="' + label + ' ' + (value == null ? '等待上报' : value.toFixed(1) + '%') + '"><circle cx="40" cy="40" r="31" class="pv-ring-track"/><circle cx="40" cy="40" r="31" class="pv-ring-fill" style="stroke:' + (value == null ? 'transparent' : uc(value)) + ';stroke-dasharray:' + c + ';stroke-dashoffset:' + c * (1 - clamp(value) / 100) + '"/><text x="40" y="43" text-anchor="middle">' + (value == null ? '—' : value.toFixed(0) + '%') + '</text></svg><span>' + label + '</span></div>';
  }
  function buckets(vs, losses, start, loss) {
    vs = vs || Array(25).fill(null);
    const max = Math.max(1, ...vs.filter((v) => v != null)), startAt = Date.parse(start);
    return '<span class="pv-bk">' + vs.map((v, i) => {
      const failed = !loss && v == null && losses && losses[i] === 100;
      const when = Number.isFinite(startAt) ? bucketTime.format(startAt + i * 300000) + ' · ' : '';
      const tip = when + (failed ? '本时段全部探测失败' : v == null ? '本时段无样本' : loss ? '探测失败率 ' + v.toFixed(1) + '%' : '平均 ' + v.toFixed(1) + ' ms');
      const height = v == null ? 0 : loss ? Math.max(4, clamp(v)) : Math.max(8, v / max * 100);
      return '<span class="' + (failed ? 'failed' : v == null ? 'missing' : '') + '" title="' + esc(tip) + '"><i style="height:' + height + '%;background:' + (loss ? lc(v || 0) : pc(v)) + '"></i></span>';
    }).join('') + '</span>';
  }
  function quality(n, full) {
    const ts = targets(n), shown = full ? ts : ts.slice(0, 3);
    if (!ts.length) return '<div class="pv-lat-empty">此节点未启用延迟探测</div>';
    let h = '<div class="pv-lat"><div class="pv-lat-head"><span>网络质量 <small>TCP</small></span><span title="延迟为最近一次探测，失败率按近 1 小时样本统计。">延迟 / 失败率</span></div>';
    h += shown.map((tg) => {
      const l = (n.latency || {})[tg.id], live = n.online && liveLatency(l);
      const value = !live ? l ? '数据过期' : '待采集' : l.ms > 0 ? l.ms.toFixed(l.ms < 1 ? 1 : 0) + ' ms' : '超时';
      return '<div class="pv-quality"><div class="pv-quality-title"><span class="pv-ln" title="' + esc(tg.name) + '"><i style="background:' + (col[tg.carrier] || '#a78bfa') + '"></i>' + esc(tg.name) + '</span><b style="color:' + (live ? pc(l.ms) : 'var(--pv-mute)') + '">' + value + '</b><span style="color:' + (live ? lc(l.loss) : 'var(--pv-mute)') + '">' + (live ? l.loss.toFixed(1) + '%' : '—') + '</span></div><div class="pv-quality-charts">' + buckets(l && l.spark, l && l.sparkLoss, l && l.sparkStart, false) + buckets(l && l.sparkLoss, null, l && l.sparkStart, true) + '</div></div>';
    }).join('');
    if (!full && ts.length > 3) h += '<div class="pv-lat-empty">另有 ' + (ts.length - 3) + ' 个目标，点击查看</div>';
    return h + '</div>';
  }
  function card(n) {
    const f = fresh(n), cpu = f ? n.cpu : null, mem = f ? n.mem : null, disk = f && n.diskTotal > 0 ? n.disk : null;
    let h = '<button type="button" data-node="' + esc(n.id) + '" class="pv-card' + (!n.online ? ' off' : concern(n) ? ' attention' : '') + '" aria-label="查看节点 ' + esc(n.name) + '"><div class="pv-hd"><span class="pv-flag">' + flag(n.flag) + '</span><span class="pv-identity"><strong class="pv-name">' + esc(n.name) + '</strong><span class="pv-region">' + esc(n.region || n.arch || '—') + '</span></span>' + badge(n) + '</div>' + badges(n);
    h += view === 'ring' ? '<div class="pv-rings">' + ring('CPU', cpu) + ring('RAM', mem) + ring('DISK', disk) + '</div>' : '<div class="pv-rows">' + meter('CPU', cpu) + meter('RAM', mem) + meter('DISK', disk) + '</div>';
    h += '<div class="pv-load"><span>LOAD <small>1m</small></span><b>' + (f ? n.load1.toFixed(2) : '—') + '</b></div>';
    h += '<div class="pv-network"><span><small>接收速率</small><b class="pv-dn">↓ ' + (f ? bps(n.rxBps) : '—') + '</b></span><span><small>发送速率</small><b class="pv-up">↑ ' + (f ? bps(n.txBps) : '—') + '</b></span></div>';
    if (last.showTraffic) h += '<div class="pv-traffic"><span>本期流量 <b>' + bytes(n.netUsed) + '</b></span><span>' + (n.netLimit > 0 ? '/ ' + bytes(n.netLimit) : '不限') + '</span>' + (n.netLimit > 0 ? '<span class="pv-track"><i style="width:' + clamp(100 * n.netUsed / n.netLimit) + '%;background:' + uc(100 * n.netUsed / n.netLimit) + '"></i></span>' : '') + '</div>';
    return h + quality(n, false) + '<div class="pv-card-foot"><span>' + (f ? '运行 ' + uptime(n.uptime) : n.lastSeen ? esc(new Date(n.lastSeen).toLocaleString()) : '等待上报') + '</span><span>查看详情 ↗</span></div></button>';
  }
  function summary() {
    const ns = last.nodes, on = ns.filter((n) => n.online), valid = ns.filter(fresh), rx = valid.reduce((a, n) => a + n.rxBps, 0), tx = valid.reduce((a, n) => a + n.txBps, 0);
    const tile = (k, v, hint, tone) => '<div class="pv-tile"><div class="k">' + k + '</div><div class="v ' + (tone || '') + '">' + v + '</div><div class="pv-tile-hint">' + hint + '</div></div>';
    return '<div class="pv-sum">' + tile('在线节点', on.length + ' / ' + ns.length, '离线 ' + (ns.length - on.length) + ' · 数据滞后 ' + (on.length - valid.length), 'g') + tile('总接收速率', valid.length ? bps(rx) : '—', '仅统计有效上报', 'g') + tile('总发送速率', valid.length ? bps(tx) : '—', '仅统计有效上报', 'b') + tile('平均 CPU', valid.length ? (valid.reduce((a, n) => a + n.cpu, 0) / valid.length).toFixed(1) + '%' : '—', '有效上报 ' + valid.length + ' 台', 'c') + tile('平均内存', valid.length ? (valid.reduce((a, n) => a + n.mem, 0) / valid.length).toFixed(1) + '%' : '—', '有效上报 ' + valid.length + ' 台') + (last.showTraffic ? tile('本期流量', bytes(ns.reduce((a, n) => a + n.netUsed, 0)), '按各节点计费周期统计', 'y') : tile('覆盖地区', new Set(ns.map((n) => n.flag).filter(Boolean)).size, '按公开节点统计')) + '</div>';
  }
  function table(ns) {
    const mini = (v) => '<span class="pv-mini"><span class="pv-track"><i style="width:' + (v == null ? 0 : clamp(v)) + '%;background:' + uc(v || 0) + '"></i></span>' + (v == null ? '—' : v.toFixed(0) + '%') + '</span>';
    return '<div class="pv-scroll"><table class="pv-table"><thead><tr><th>节点</th><th>状态</th><th>CPU</th><th>RAM</th><th>DISK</th><th>接收 / 发送</th>' + (last.showTraffic ? '<th>本期流量</th>' : '') + '<th>网络质量</th><th>运行</th></tr></thead><tbody>' + ns.map((n) => {
      const f = fresh(n);
      const pings = targets(n).map((tg) => { const l = (n.latency || {})[tg.id], live = n.online && liveLatency(l); return '<span title="' + esc(tg.name) + '" style="color:' + (live ? pc(l.ms) : 'var(--pv-mute)') + '">' + esc(tg.name) + ' ' + (live ? l.ms > 0 ? l.ms.toFixed(l.ms < 1 ? 1 : 0) + ' ms' : '超时' : '—') + '</span>'; }).join('');
      return '<tr><td><button class="pv-node-link" data-node="' + esc(n.id) + '">' + flag(n.flag) + ' ' + esc(n.name) + '</button></td><td>' + badge(n) + '</td><td>' + mini(f ? n.cpu : null) + '</td><td>' + mini(f ? n.mem : null) + '</td><td>' + mini(f && n.diskTotal > 0 ? n.disk : null) + '</td><td><span class="pv-dn">↓ ' + (f ? bps(n.rxBps) : '—') + '</span><br><span class="pv-up">↑ ' + (f ? bps(n.txBps) : '—') + '</span></td>' + (last.showTraffic ? '<td>' + bytes(n.netUsed) + '</td>' : '') + '<td><div class="pv-pings">' + pings + '</div></td><td>' + (f ? uptime(n.uptime) : '—') + '</td></tr>';
    }).join('') + '</tbody></table></div>';
  }
  function filtered() {
    const q = $('search').value.trim().toLocaleLowerCase(), sort = $('sort').value;
    const ns = last.nodes.filter((n) => (!q || [n.name, n.region, n.os, n.arch].join(' ').toLocaleLowerCase().includes(q)) && (!region || n.flag === region) && (state === 'all' || state === 'online' && n.online || state === 'offline' && !n.online || state === 'attention' && concern(n)));
    const latency = (n) => { if (!n.online) return Infinity; const ls = Object.values(n.latency || {}).filter((l) => liveLatency(l) && l.ms > 0); return ls.length ? ls.reduce((a, l) => a + l.ms, 0) / ls.length : Infinity; };
    const value = (n) => sort === 'cpu' ? (fresh(n) ? -n.cpu : Infinity) : sort === 'network' ? (fresh(n) ? -(n.rxBps + n.txBps) : Infinity) : sort === 'latency' ? latency(n) : Number.isFinite(Date.parse(n.expireAt)) ? Date.parse(n.expireAt) : Infinity;
    if (sort === 'name') ns.sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
    else if (sort !== 'default') ns.sort((a, b) => value(a) - value(b) || a.name.localeCompare(b.name));
    return ns;
  }
  function draw() {
    if (!last) return;
    $('sum').innerHTML = summary();
    const regions = new Map(); last.nodes.forEach((n) => { if (n.flag) regions.set(n.flag, (regions.get(n.flag) || 0) + 1); });
    $('chips').innerHTML = '<button class="pv-chip' + (!region ? ' on' : '') + '" data-region="" aria-pressed="' + !region + '">全部地区 ' + last.nodes.length + '</button>' + Array.from(regions, ([f, count]) => '<button class="pv-chip' + (region === f ? ' on' : '') + '" data-region="' + esc(f) + '" aria-pressed="' + (region === f) + '">' + flag(f) + ' ' + esc(f) + ' <small>' + count + '</small></button>').join('');
    for (const v of ['card', 'ring', 'table']) { $('v-' + v).classList.toggle('on', view === v); $('v-' + v).setAttribute('aria-pressed', String(view === v)); }
    document.querySelectorAll('[data-state]').forEach((b) => { b.classList.toggle('on', b.dataset.state === state); b.setAttribute('aria-pressed', String(b.dataset.state === state)); });
    const ns = filtered(); $('count').textContent = '显示 ' + ns.length + ' / ' + last.nodes.length + ' 个节点';
    const focused = $('list').contains(document.activeElement) ? document.activeElement.getAttribute('data-node') : null;
    $('list').innerHTML = !ns.length ? '<div class="pv-empty">' + (last.nodes.length ? '没有符合筛选的节点' : '暂无节点') + '<div><button class="pv-clear" type="button">清除筛选</button></div></div>' : view === 'table' ? table(ns) : '<div class="pv-grid">' + ns.map(card).join('') + '</div>';
    if (focused) { const button = Array.from($('list').querySelectorAll('[data-node]')).find((b) => b.dataset.node === focused); if (button) button.focus({ preventScroll: true }); }
    if (selected) drawDetail();
  }
  function drawDetail() {
    const n = last.nodes.find((n) => n.id === selected);
    if (!n) { $('detail').close(); selected = null; return; }
    $('detail-title').textContent = n.name;
    const f = fresh(n);
    $('detail-content').innerHTML = '<div class="row">' + badges(n) + badge(n) + '</div><div class="pv-detail-summary"><div><small>运行时间</small><b>' + (f ? uptime(n.uptime) : '—') + '</b></div><div><small>接收速率</small><b>' + (f ? bps(n.rxBps) : '—') + '</b></div><div><small>发送速率</small><b>' + (f ? bps(n.txBps) : '—') + '</b></div></div><h3>近 2 小时网络质量</h3>' + quality(n, true) + '<p class="pv-footnote">每格 5 分钟；左侧为平均延迟，右侧为失败率。灰格表示无样本。失败率表示 TCP 连接探测失败的比例，不是 ICMP 丢包率。</p>';
  }
  $('list').addEventListener('click', (event) => {
    const button = event.target.closest('[data-node]');
    if (button) { selected = button.dataset.node; drawDetail(); if (!$('detail').open) $('detail').showModal(); }
    if (event.target.closest('.pv-clear')) { region = ''; state = 'all'; $('search').value = ''; draw(); }
  });
  $('detail-close').onclick = () => $('detail').close();
  $('detail').addEventListener('close', () => { selected = null; });
  $('chips').addEventListener('click', (event) => { const button = event.target.closest('[data-region]'); if (button) { const r = button.dataset.region; region = region === r ? '' : r; draw(); } });
  document.querySelectorAll('[data-state]').forEach((b) => { b.onclick = () => { state = b.dataset.state; draw(); }; });
  for (const v of ['card', 'ring', 'table']) $('v-' + v).onclick = () => { view = v; try { localStorage.setItem(key, v); } catch (_) {} draw(); };
  $('search').oninput = draw; $('sort').onchange = draw;
  async function load() {
    if (busy || document.hidden) return;
    busy = true;
    const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch(DATA, { cache: 'no-store', signal: controller.signal });
      if (response.status === 404 || response.status === 403 || (response.ok && !(response.headers.get('Content-Type') || '').includes('application/json'))) {
        last = null; selected = null;
        if ($('detail').open) $('detail').close();
        $('sum').textContent = ''; $('chips').textContent = ''; $('count').textContent = '';
      }
      if (!response.ok) throw new Error('unavailable');
      const data = await response.json();
      if (!Array.isArray(data.nodes) || !Array.isArray(data.targets) || !Number.isFinite(Date.parse(data.at))) throw new Error('invalid data');
      last = data; received = performance.now();
      $('error').hidden = true; $('at').closest('.pv-live').classList.remove('stale'); $('at').textContent = '更新于 ' + new Date(data.at).toLocaleTimeString();
      $('note').textContent = data.note || ''; $('note').hidden = !data.note;
      $('probe-disabled').hidden = data.probeEnabled !== false;
      draw();
    } catch (_) {
      $('error').hidden = false; $('at').closest('.pv-live').classList.add('stale');
      if (!last) $('list').innerHTML = '<div class="pv-empty">暂无可显示的数据</div>';
      $('error-message').textContent = last ? '更新失败，已保留上次数据。过期指标会标记为数据滞后。' : '暂时无法读取节点状态，请稍后重试。';
    } finally { clearTimeout(timeout); busy = false; }
  }
  $('retry').onclick = load;
  document.addEventListener('visibilitychange', () => { if (!document.hidden) load(); });
  load(); setInterval(load, 15000);
  // Only redraw between refreshes when telemetry actually changes freshness.
  let freshness = '';
  setInterval(() => { if (last && !document.hidden) { const value = last.nodes.map((n) => String(fresh(n)) + targets(n).map((tg) => String(!!liveLatency((n.latency || {})[tg.id]))).join('')).join('|'); if (value !== freshness) { freshness = value; draw(); } } }, 5000);
})();
